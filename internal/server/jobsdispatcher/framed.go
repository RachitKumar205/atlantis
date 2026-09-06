// The framed WorkerDispatch service, for clients that cannot select a codec.
//
// WorkerDispatch selects `atl-json-dispatch` by gRPC content-subtype, which
// grpc-go sets with CallContentSubtype. grpc-python has no equivalent:
// measured against a live server, it sends `content-type: application/grpc`
// with no subtype, call metadata does not override it, and the installed
// package exposes no such knob in its Python layer or in cygrpc. The server
// then decodes with the default proto codec and the first envelope fails with
// "failed to unmarshal, message is *jsonMsg, want proto.Message".
//
// This service carries the same JSON envelopes inside a Frame message over the
// default codec. Everything below the wire adapter is shared: the same
// WorkerSession, so authz, the queue check, the lease processor and every
// envelope rule hold for both without being written twice.

package jobsdispatcher

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	wdpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/workerdispatch/v1"
)

// RegisterFramed binds the framed service to a gRPC server.
//
// Additive: a server may register both, and a Go worker keeps using the
// unframed one. Nothing about the existing service changes.
func RegisterFramed(srv *grpc.Server, d *Dispatcher) {
	srv.RegisterService(&framedServiceDesc, d)
}

// framedServiceDesc mirrors serviceDesc with a different service name and the
// framing adapter in front of the handler.
//
// The stream name is the same, so a client that knows one path knows the other
// by swapping the service segment.
var framedServiceDesc = grpc.ServiceDesc{
	ServiceName: "atlantis.workerdispatch.v1.WorkerDispatchFramed",
	HandlerType: (*WorkerDispatchServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "WorkerSession",
			Handler:       handleFramedWorkerSession,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "atlantis/workerdispatch/v1/frame.proto",
}

// handleFramedWorkerSession wraps the stream and runs the ordinary session.
func handleFramedWorkerSession(srv any, stream grpc.ServerStream) error {
	d, ok := srv.(*Dispatcher)
	if !ok {
		return status.Errorf(codes.Internal, "handler bound to wrong type %T", srv)
	}
	return d.WorkerSession(framedStream{ServerStream: stream})
}

// framedStream translates between the jsonMsg the session logic speaks and the
// Frame the wire carries.
//
// Only SendMsg and RecvMsg are overridden; Context, SetHeader and the rest come
// from the embedded stream, so the caller's identity, deadline and metadata are
// the same values the unframed path sees. That is what makes
// CallerFromContext, the authz check and the partition interceptor behave
// identically here.
type framedStream struct {
	grpc.ServerStream
}

// SendMsg wraps an outbound envelope.
//
// A type other than *jsonMsg is refused rather than passed through. Every send
// on this stream comes from sendEnvelope, so another type means the session
// logic grew a second way to write and this adapter did not learn about it —
// passing it to the default codec would put a differently-shaped message on a
// wire the worker decodes as a Frame.
func (s framedStream) SendMsg(m any) error {
	msg, ok := m.(*jsonMsg)
	if !ok {
		return status.Errorf(codes.Internal, "framed stream: cannot send %T", m)
	}
	return s.ServerStream.SendMsg(&wdpb.Frame{Json: msg.Raw})
}

// RecvMsg unwraps an inbound envelope into the caller's jsonMsg.
func (s framedStream) RecvMsg(m any) error {
	msg, ok := m.(*jsonMsg)
	if !ok {
		return status.Errorf(codes.Internal, "framed stream: cannot receive into %T", m)
	}
	var frame wdpb.Frame
	if err := s.ServerStream.RecvMsg(&frame); err != nil {
		return err
	}
	// Copied rather than aliased. The proto runtime may reuse the Frame's
	// backing array on the next Recv, and recvEnvelope's json.Unmarshal reads
	// this slice after that call has been made.
	msg.Raw = append(msg.Raw[:0], frame.GetJson()...)
	return nil
}

// A compile-time check that the adapter still satisfies the interface the
// handler signature requires.
var _ grpc.ServerStream = framedStream{}

// FramedMethodPath returns the gRPC method a framed worker opens.
//
// Built from the ServiceDesc rather than written out, so it cannot name a
// service the server does not register. The Python SDK hardcodes the same
// string: it builds the stream with channel.stream_stream, because the
// envelopes are JSON and there is no generated service to bind to.
func FramedMethodPath() string {
	return "/" + framedServiceDesc.ServiceName + "/" + framedServiceDesc.Streams[0].StreamName
}
