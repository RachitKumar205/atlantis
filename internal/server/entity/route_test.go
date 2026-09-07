package entity

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
)

const routeNoteOnly = `
entity Note in notes {
  id    bigint primary
  title text not null
}
`

const routeNoteAndTask = routeNoteOnly + `
entity Task in notes {
  id   bigint primary
  name text not null
}
`

func routeIR(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("route.atl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatal(err)
	}
	codegen.AssignProtoNumbers(nil, ir)
	return ir
}

// notFoundRow answers every scan with ErrNotFound, so a Get that reaches the
// handler returns NotFound and one that does not returns Unimplemented.
type notFoundRow struct{}

func (notFoundRow) Scan(...any) error { return runtime.ErrNotFound }

type notFoundPool struct{ fakePool }

func (*notFoundPool) QueryRow(context.Context, string, ...any) runtime.Row { return notFoundRow{} }

// routeHarness is a grpc.Server that serves nothing but the dynamic router,
// with counters on both interceptor chains.
type routeHarness struct {
	srv         *Server
	conn        *grpc.ClientConn
	unaryCalls  atomic.Int32
	streamCalls atomic.Int32
	lastMethod  atomic.Value
}

func newRouteHarness(t *testing.T) *routeHarness {
	return newRouteHarnessWith(t, &notFoundPool{})
}

func newRouteHarnessWith(t *testing.T, pool runtime.Pool) *routeHarness {
	t.Helper()
	h := &routeHarness{srv: NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)}
	count := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		h.unaryCalls.Add(1)
		h.lastMethod.Store(info.FullMethod)
		// A custom query or procedure is answered here: the fake pool has no
		// rows for one, and the route is what is under test.
		if strings.Contains(info.FullMethod, ".CustomService/") {
			return nil, status.Error(codes.Aborted, "routed")
		}
		return handler(ctx, req)
	}
	countStream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		h.streamCalls.Add(1)
		return handler(srv, ss)
	}
	static := func(service string) bool { return service == "atlantis.admin.v1.AdminService" }
	gs := grpc.NewServer(
		grpc.UnknownServiceHandler(h.srv.Handler(interceptors.ChainUnary(count, interceptors.NewStatus()), static)),
		grpc.StreamInterceptor(interceptors.StreamChainUnless(h.srv.Serves, countStream)),
	)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); gs.Stop() })
	h.conn = conn
	return h
}

// get invokes Get<entity> for id 1 and returns the status code.
func (h *routeHarness) get(t *testing.T, entityID, name string) codes.Code {
	t.Helper()
	req := any(&emptypb.Empty{})
	resp := any(&emptypb.Empty{})
	if meta, ok := h.srv.snapshot.Load().entities[entityID]; ok {
		r := dynamicpb.NewMessage(meta.getRequestDesc)
		r.Set(meta.getRequestDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(1))
		req = r
		resp = dynamicpb.NewMessage(meta.getResponseDesc)
	}
	err := h.conn.Invoke(context.Background(), "/atlantis.notes.v1."+name+"Service/Get"+name, req, resp)
	return status.Code(err)
}

func TestRoute_ServesNothingBeforeLoad(t *testing.T) {
	h := newRouteHarness(t)
	if code := h.get(t, "notes.Note", "Note"); code != codes.Unimplemented {
		t.Fatalf("before Load: %v, want Unimplemented", code)
	}
	if n := h.unaryCalls.Load(); n != 0 {
		t.Errorf("unary chain ran %d times for an unrouted method", n)
	}
}

// TestRoute_ReloadAddsAndRemovesServices is the defect from the end-to-end
// run: a service declared after boot answered "unknown service" until the
// process restarted.
func TestRoute_ReloadAddsAndRemovesServices(t *testing.T) {
	h := newRouteHarness(t)
	if err := h.srv.Load(routeIR(t, routeNoteOnly)); err != nil {
		t.Fatal(err)
	}
	if code := h.get(t, "notes.Note", "Note"); code != codes.NotFound {
		t.Fatalf("GetNote after Load: %v, want NotFound (handler reached)", code)
	}
	if code := h.get(t, "notes.Task", "Task"); code != codes.Unimplemented {
		t.Fatalf("GetTask before its entity exists: %v, want Unimplemented", code)
	}

	if err := h.srv.Reload(routeIR(t, routeNoteAndTask), "v2"); err != nil {
		t.Fatal(err)
	}
	if code := h.get(t, "notes.Task", "Task"); code != codes.NotFound {
		t.Fatalf("GetTask after Reload: %v, want NotFound (handler reached without a restart)", code)
	}
	if got := h.lastMethod.Load(); got != "/atlantis.notes.v1.TaskService/GetTask" {
		t.Errorf("unary chain saw FullMethod %v", got)
	}

	if err := h.srv.Reload(routeIR(t, routeNoteOnly), "v3"); err != nil {
		t.Fatal(err)
	}
	if code := h.get(t, "notes.Task", "Task"); code != codes.Unimplemented {
		t.Fatalf("GetTask after its entity was removed: %v, want Unimplemented", code)
	}

	// Two routed calls reached a handler and each ran the unary chain once;
	// the two unrouted calls ran the stream chain once each and the unary
	// chain not at all.
	if n := h.unaryCalls.Load(); n != 2 {
		t.Errorf("unary chain ran %d times, want 2", n)
	}
	if n := h.streamCalls.Load(); n != 2 {
		t.Errorf("stream chain ran %d times, want 2 (one per unrouted call)", n)
	}
}

func TestRoute_StreamChainRunsForUnroutedMethods(t *testing.T) {
	h := newRouteHarness(t)
	if err := h.srv.Load(routeIR(t, routeNoteOnly)); err != nil {
		t.Fatal(err)
	}
	err := h.conn.Invoke(context.Background(), "/grpc.health.v1.Health/Check", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unrouted method: %v, want Unimplemented", err)
	}
	if n := h.streamCalls.Load(); n != 1 {
		t.Errorf("stream chain ran %d times for an unrouted method, want 1", n)
	}
	if n := h.unaryCalls.Load(); n != 0 {
		t.Errorf("unary chain ran %d times for an unrouted method, want 0", n)
	}
}

func TestRoute_UnimplementedWording(t *testing.T) {
	s := NewServer(&notFoundPool{}, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := s.Load(routeIR(t, routeNoteOnly)); err != nil {
		t.Fatal(err)
	}
	snap := s.snapshot.Load()
	static := func(service string) bool { return service == "atlantis.admin.v1.AdminService" }
	cases := map[string]string{
		"/atlantis.notes.v1.NoteService/ListNote": "unknown method ListNote for service atlantis.notes.v1.NoteService",
		"/atlantis.notes.v1.TaskService/GetTask":  "unknown service atlantis.notes.v1.TaskService",
		// grpc.Server hands an unknown method of a registered service to
		// the unknown handler too.
		"/atlantis.admin.v1.AdminService/Nope": "unknown method Nope for service atlantis.admin.v1.AdminService",
		"nope":                                 `malformed method name: "nope"`,
	}
	for method, want := range cases {
		err := unimplemented(snap, method, static)
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want Unimplemented %q", method, err, want)
		}
	}
}

func TestRoute_ReflectionSeesTheCurrentSnapshot(t *testing.T) {
	s := NewServer(&notFoundPool{}, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := s.Load(routeIR(t, routeNoteOnly)); err != nil {
		t.Fatal(err)
	}
	static := grpc.NewServer()
	provider := s.ServiceInfoWith(static)

	info := provider.GetServiceInfo()
	note, ok := info["atlantis.notes.v1.NoteService"]
	if !ok {
		t.Fatalf("NoteService not listed: %v", info)
	}
	if note.Metadata != "atlantis/notes/v1/note_dynamic.proto" || len(note.Methods) != 6 {
		t.Errorf("NoteService info = %+v", note)
	}
	if _, err := s.FindFileByPath(note.Metadata.(string)); err != nil {
		t.Errorf("the file ServiceInfo names cannot be resolved: %v", err)
	}
	if d, err := s.FindDescriptorByName("atlantis.notes.v1.NoteOrderField"); err != nil || d == nil {
		t.Errorf("FindDescriptorByName(enum): %v, %v", d, err)
	}
	if _, ok := info["atlantis.notes.v1.TaskService"]; ok {
		t.Fatal("TaskService listed before its entity exists")
	}

	if err := s.Reload(routeIR(t, routeNoteAndTask), "v2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.GetServiceInfo()["atlantis.notes.v1.TaskService"]; !ok {
		t.Fatal("TaskService not listed after Reload")
	}

	d, err := s.FindDescriptorByName("atlantis.notes.v1.TaskService")
	if err != nil || d.FullName() != "atlantis.notes.v1.TaskService" {
		t.Errorf("FindDescriptorByName: %v, %v", d, err)
	}
	fd, err := s.FindFileByPath("atlantis/notes/v1/task_dynamic.proto")
	if err != nil || fd.Path() != "atlantis/notes/v1/task_dynamic.proto" {
		t.Errorf("FindFileByPath(dynamic): %v, %v", fd, err)
	}
	if _, err := s.FindFileByPath("google/protobuf/timestamp.proto"); err != nil {
		t.Errorf("FindFileByPath(global fallback): %v", err)
	}
}

const routeCustom = routeNoteOnly + `
query NotesByTitle for Note {
  input { title: text }
  output as Note
  sql touches(Note) {
    SELECT id, title FROM notes_note WHERE title = $title
  }
}

procedure RetireNote for Note {
  input { id: bigint }
  steps {
    update Note set title = "retired" where id = $id
  }
}
`

func TestRoute_CustomServiceMethods(t *testing.T) {
	h := newRouteHarness(t)
	if err := h.srv.Load(routeIR(t, routeCustom)); err != nil {
		t.Fatal(err)
	}
	invoke := func(method string) error {
		return h.conn.Invoke(context.Background(), "/atlantis.notes.v1.CustomService/"+method, &emptypb.Empty{}, &emptypb.Empty{})
	}
	for _, m := range []string{"NotesByTitle", "RetireNote"} {
		if err := invoke(m); status.Code(err) != codes.Aborted {
			t.Errorf("%s: %v, want Aborted from the unary chain", m, err)
		}
	}
	err := invoke("Nope")
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "unknown method Nope for service atlantis.notes.v1.CustomService") {
		t.Errorf("Nope: %v", err)
	}
	if n := h.unaryCalls.Load(); n != 2 {
		t.Errorf("unary chain ran %d times, want 2", n)
	}

	info := h.srv.GetServiceInfo()["atlantis.notes.v1.CustomService"]
	if info.Metadata != "atlantis/notes/v1/custom_dynamic.proto" || len(info.Methods) != 2 ||
		info.Methods[0].Name != "NotesByTitle" || info.Methods[1].Name != "RetireNote" {
		t.Errorf("CustomService info = %+v", info)
	}
	d, err := h.srv.FindDescriptorByName("atlantis.notes.v1.CustomService")
	if err != nil {
		t.Fatalf("FindDescriptorByName(CustomService): %v", err)
	}
	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok || svc.Methods().Len() != 2 || svc.Methods().ByName("RetireNote") == nil {
		t.Errorf("CustomService descriptor = %v", d)
	}
	if _, err := h.srv.FindFileByPath(info.Metadata.(string)); err != nil {
		t.Errorf("the file ServiceInfo names cannot be resolved: %v", err)
	}
}
