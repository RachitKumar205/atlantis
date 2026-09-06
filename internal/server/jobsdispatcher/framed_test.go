package jobsdispatcher

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	wdpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/workerdispatch/v1"
)

// The framed adapter must carry an envelope unchanged in both directions.
//
// Driven over a real gRPC stream rather than by calling the methods directly:
// the thing being tested is that the default proto codec accepts what the
// adapter hands it and returns what the session logic expects, and a direct
// call exercises neither codec.
//
// The handler reads and writes with recvEnvelope and sendEnvelope — the same
// two functions WorkerSession uses — so a change that breaks the real session
// breaks this too.
func TestFramedStreamCarriesEnvelopesBothWays(t *testing.T) {
	var got *WorkerEnvelope

	echo := grpc.ServiceDesc{
		ServiceName: "atlantis.workerdispatch.v1.WorkerDispatchFramed",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "WorkerSession",
			ServerStreams: true,
			ClientStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				s := framedStream{ServerStream: stream}
				env, err := recvEnvelope(s)
				if err != nil {
					return err
				}
				got = env
				return sendEnvelope(s, &DispatchEnvelope{
					SessionAccepted: &SessionAccepted{
						SessionID: "sess-1", LeaseTTLMS: 30000, HeartbeatMS: 10000,
					},
				})
			},
		}},
	}

	target := serveFramed(t, &echo)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The client speaks Frame over the DEFAULT codec — no content-subtype,
	// which is the whole reason this service exists.
	desc := &grpc.StreamDesc{StreamName: "WorkerSession", ServerStreams: true, ClientStreams: true}
	stream, err := conn.NewStream(ctx, desc, FramedMethodPath())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	open, err := json.Marshal(&WorkerEnvelope{Open: &OpenSession{
		Queue:       "default",
		JobNames:    []string{"library.ReindexAuthor"},
		MaxInFlight: 4,
		PodID:       "py-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&wdpb.Frame{Json: open}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var reply wdpb.Frame
	if err := stream.RecvMsg(&reply); err != nil {
		t.Fatalf("recv: %v", err)
	}
	var dispatchEnv DispatchEnvelope
	if err := json.Unmarshal(reply.GetJson(), &dispatchEnv); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}

	if got == nil || got.Open == nil {
		t.Fatal("the server did not receive an Open envelope")
	}
	if got.Open.Queue != "default" || got.Open.PodID != "py-1" {
		t.Errorf("Open arrived as %+v", got.Open)
	}
	if len(got.Open.JobNames) != 1 || got.Open.JobNames[0] != "library.ReindexAuthor" {
		t.Errorf("job names arrived as %v", got.Open.JobNames)
	}
	if dispatchEnv.SessionAccepted == nil || dispatchEnv.SessionAccepted.SessionID != "sess-1" {
		t.Errorf("SessionAccepted arrived as %+v", dispatchEnv.SessionAccepted)
	}
	if dispatchEnv.SessionAccepted.HeartbeatMS != 10000 {
		t.Errorf("heartbeat_ms = %d, want 10000", dispatchEnv.SessionAccepted.HeartbeatMS)
	}
}

// Args and TraceCtx are Go []byte, which json.Marshal writes as base64.
//
// The Python port has to decode that rather than read the string as the
// payload, and getting it wrong hands a handler base64 text that parses as
// neither JSON nor anything else. Pinned here so the Go side's encoding is a
// stated fact the Python tests can be written against.
func TestDispatchArgsTravelAsBase64(t *testing.T) {
	raw, err := json.Marshal(&DispatchEnvelope{Dispatch: &Dispatch{
		JobID:    7,
		JobName:  "library.ReindexAuthor",
		Args:     []byte(`{"author_id":3}`),
		TraceCtx: []byte("trace-parent"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	var asMap map[string]map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	args, _ := asMap["dispatch"]["args"].(string)
	if args != "eyJhdXRob3JfaWQiOjN9" {
		t.Errorf("args on the wire = %q, want the base64 of the args JSON. "+
			"A port that reads this field as the payload hands the handler base64 text", args)
	}
	trace, _ := asMap["dispatch"]["trace_ctx"].(string)
	if trace != "dHJhY2UtcGFyZW50" {
		t.Errorf("trace_ctx on the wire = %q, want base64", trace)
	}
}

// The adapter refuses a message type the session logic does not send.
//
// Passing an unknown type through to the default codec would put a
// differently-shaped message on a wire the worker decodes as a Frame, which is
// a decode failure at the far end with nothing here to point at.
func TestFramedStreamRefusesForeignMessageTypes(t *testing.T) {
	s := framedStream{ServerStream: nopServerStream{}}

	if got := status.Code(s.SendMsg(&wdpb.Frame{})); got != codes.Internal {
		t.Errorf("SendMsg of a non-jsonMsg gave %v, want Internal", got)
	}
	if got := status.Code(s.RecvMsg(&wdpb.Frame{})); got != codes.Internal {
		t.Errorf("RecvMsg into a non-jsonMsg gave %v, want Internal", got)
	}
}

// The framed service and the unframed one differ only in name and adapter.
//
// Both must announce the same stream and the same bidi shape, or a worker that
// switches services finds a different protocol behind the same envelopes.
func TestFramedServiceMirrorsTheUnframedOne(t *testing.T) {
	if len(framedServiceDesc.Streams) != len(serviceDesc.Streams) {
		t.Fatalf("framed declares %d streams, unframed %d",
			len(framedServiceDesc.Streams), len(serviceDesc.Streams))
	}
	f, u := framedServiceDesc.Streams[0], serviceDesc.Streams[0]
	if f.StreamName != u.StreamName {
		t.Errorf("stream names differ: %q vs %q", f.StreamName, u.StreamName)
	}
	if f.ServerStreams != u.ServerStreams || f.ClientStreams != u.ClientStreams {
		t.Error("the framed stream is not bidi in the same way as the unframed one")
	}
	if framedServiceDesc.ServiceName == serviceDesc.ServiceName {
		t.Error("both services claim the same name; registering both would panic")
	}
	if got, want := FramedMethodPath(),
		"/atlantis.workerdispatch.v1.WorkerDispatchFramed/WorkerSession"; got != want {
		t.Errorf("FramedMethodPath() = %q, want %q", got, want)
	}
}

func serveFramed(t *testing.T, desc *grpc.ServiceDesc) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	srv.RegisterService(desc, struct{}{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// nopServerStream satisfies grpc.ServerStream for the type-guard test, which
// never reaches the embedded stream.
type nopServerStream struct{ grpc.ServerStream }
