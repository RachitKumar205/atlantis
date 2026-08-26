package admin

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/dynamicpb"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestMigratedRPCsAreTracked replaces the compile-time check that embedding
// UnimplementedAdminServiceServer gave away.
//
// The hand-rolled AdminServer interface made a missing method a build failure.
// The embedding makes it a runtime Unimplemented instead, which is exactly the
// forgotten-call-site shape the rest of this migration is trying to remove. So
// the descriptor is held to account here: every method is either implemented
// and listed in migratedRPCs, or not implemented and absent from it. A method
// added to the proto and forgotten in grpcgen.go fails this test.
func TestMigratedRPCsAreTracked(t *testing.T) {
	sd := adminpb.File_atlantis_admin_v1_admin_proto.Services().ByName("AdminService")
	if sd == nil {
		t.Fatal("AdminService missing from the compiled descriptor")
	}

	// Every method is invoked for real and classified by whether it answers
	// Unimplemented. Asserting on the map alone would be a guard that cannot
	// fire: reflect cannot distinguish a method promoted from the embedded
	// stub from one that overrides it, so only behaviour separates "migrated"
	// from "forgotten".
	//
	// Requests are empty dynamic messages, and a migrated RPC may reject one as
	// invalid. Any code other than Unimplemented means a real handler ran.
	conn, panics := dialTestConn(t, &Service{})
	ctx := context.Background()

	var migrated, pending []string
	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		name := string(m.Name())
		full := "/" + string(sd.FullName()) + "/" + name

		in := dynamicpb.NewMessage(m.Input())
		out := dynamicpb.NewMessage(m.Output())
		err := conn.Invoke(ctx, full, in, out)

		if status.Code(err) == codes.Unimplemented {
			pending = append(pending, name)
			if migratedRPCs[name] {
				t.Errorf("%s is listed in migratedRPCs but answers Unimplemented — "+
					"the method is missing from grpcServer", name)
			}
			continue
		}
		migrated = append(migrated, name)
		if !migratedRPCs[name] {
			t.Errorf("%s is implemented on grpcServer but missing from migratedRPCs — "+
				"add it, so the tracking map stays honest", name)
		}
	}

	// A panicking handler answers Internal, which the loop above counts as
	// migrated. That is the reading this test cannot afford to get wrong, so
	// assert no handler panicked rather than trusting the classification.
	if got := panics.seen(); len(got) > 0 {
		t.Errorf("handler(s) panicked and were counted as migrated: %v", got)
	}

	sort.Strings(pending)
	t.Logf("%d/%d RPCs migrated; pending: %v", len(migrated), sd.Methods().Len(), pending)

	if len(pending) == 0 {
		t.Log("all RPCs implemented. grpcServer could collapse into *Service and " +
			"migratedRPCs could go; what keeps them is that several handlers scan " +
			"rows into hand-written structs and convert via *ToPB, so the structs " +
			"outlive the wire format that named them")
	}
}

// panicRecorder captures handler panics instead of letting them kill the test
// process, and remembers them so a test can assert none happened.
//
// Recovering silently would be worse than not recovering: TestMigratedRPCsAreTracked
// classifies any non-Unimplemented response as "this RPC is migrated", so a
// handler that segfaults would be counted as working and the suite would go
// green over it. Recording is what keeps the recovery honest.
type panicRecorder struct {
	mu      sync.Mutex
	methods []string
}

func (p *panicRecorder) record(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.methods = append(p.methods, method)
}

func (p *panicRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.methods...)
}

// dialTestConn stands up the generated service over an in-process connection.
// The new path had no coverage at all before this: the migrated RPCs were
// verified by the compiler and nothing else.
//
// Note the absence of ForceCodecV2 here, unlike every real client. That is
// what lets these tests speak protobuf to the generated service; see the note
// on grpcServer about why the production clients cannot migrate per-RPC over
// their existing connections.
func dialTestConn(t *testing.T, svc *Service) (*grpc.ClientConn, *panicRecorder) {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	// Mirrors the recovery interceptor cmd/server installs (cmd/server/auth.go),
	// so a panicking handler fails one call rather than the whole process.
	panics := &panicRecorder{}
	srv := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
			defer func() {
				if r := recover(); r != nil {
					panics.record(info.FullMethod)
					err = status.Errorf(codes.Internal, "panic: %v", r)
				}
			}()
			return h(ctx, req)
		}))
	RegisterGenerated(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, panics
}

func dialTestServer(t *testing.T, svc *Service) adminpb.AdminServiceClient {
	t.Helper()
	conn, panics := dialTestConn(t, svc)
	// Any panic in these focused tests is a bug in the handler under test,
	// never an expected outcome.
	t.Cleanup(func() {
		if got := panics.seen(); len(got) > 0 {
			t.Errorf("handler(s) panicked: %v", got)
		}
	})
	return adminpb.NewAdminServiceClient(conn)
}

// TestGeneratedPathServesMigratedRPC proves the protobuf path actually works
// end to end, not merely that it compiles. A nil pool takes the no-Postgres
// branch, which is enough to exercise the wire round-trip.
func TestGeneratedPathServesMigratedRPC(t *testing.T) {
	client := dialTestServer(t, &Service{})

	resp, err := client.GetCallerAliases(context.Background(),
		&adminpb.GetCallerAliasesRequest{Caller: "svc-a"})
	if err != nil {
		t.Fatalf("GetCallerAliases over the generated path: %v", err)
	}
	if resp.GetCaller() != "svc-a" {
		t.Errorf("caller = %q, want %q", resp.GetCaller(), "svc-a")
	}
}

// TestGeneratedPathValidatesInput pins that argument validation survived the
// move to protobuf getters, where an absent field yields a zero value rather
// than being distinguishable from an empty one.
//
// It asserts the message, not merely that some error came back: a nil-pointer
// panic recovered into codes.Internal would satisfy "an error occurred" while
// proving the opposite of what this test is for.
func TestGeneratedPathValidatesInput(t *testing.T) {
	client := dialTestServer(t, &Service{})

	_, err := client.GetCallerAliases(context.Background(),
		&adminpb.GetCallerAliasesRequest{})
	if err == nil {
		t.Fatal("expected an error for an empty caller")
	}
	if !strings.Contains(err.Error(), "caller is required") {
		t.Errorf("want the validation error, got: %v", err)
	}
}

// TestNoRPCAnswersUnimplemented is the completion property, replacing the test
// that used to assert the opposite for a specific not-yet-migrated method.
//
// Every method now dispatches to a real handler, so the embedded
// UnimplementedAdminServiceServer should be unreachable. Adding an RPC to the
// proto without implementing it fails here — which is the same guarantee the
// hand-rolled AdminServer interface used to give at compile time, and the
// reason migratedRPCs can eventually be deleted along with grpc.go.
func TestNoRPCAnswersUnimplemented(t *testing.T) {
	sd := adminpb.File_atlantis_admin_v1_admin_proto.Services().ByName("AdminService")
	if sd == nil {
		t.Fatal("AdminService missing from the compiled descriptor")
	}
	conn, panics := dialTestConn(t, &Service{})
	ctx := context.Background()

	for i := 0; i < sd.Methods().Len(); i++ {
		m := sd.Methods().Get(i)
		full := "/" + string(sd.FullName()) + "/" + string(m.Name())
		err := conn.Invoke(ctx, full, dynamicpb.NewMessage(m.Input()), dynamicpb.NewMessage(m.Output()))
		if status.Code(err) == codes.Unimplemented {
			t.Errorf("%s is declared in the proto but not implemented on grpcServer", m.Name())
		}
	}
	if got := panics.seen(); len(got) > 0 {
		t.Errorf("handler(s) panicked: %v", got)
	}
}
