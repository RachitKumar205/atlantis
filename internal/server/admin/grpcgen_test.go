package admin

import (
	"context"
	"net"
	"sort"
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
	// Requests are empty dynamic messages. A migrated RPC may well reject one
	// as invalid — that is fine and is the point. Any code other than
	// Unimplemented means a real handler ran.
	conn := dialTestConn(t, &Service{})
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

	sort.Strings(pending)
	t.Logf("%d/%d RPCs migrated; pending: %v", len(migrated), sd.Methods().Len(), pending)

	if len(pending) == 0 {
		t.Log("all RPCs migrated — delete grpc.go, the JSON structs, and " +
			"migratedRPCs, and collapse grpcServer into *Service")
	}
}

// dialTestConn stands up the generated service over an in-process connection.
// The new path had no coverage at all before this: the two migrated RPCs were
// verified by the compiler and nothing else.
//
// Note the absence of ForceCodecV2 here, unlike every real client. That is
// what lets these tests speak protobuf to the generated service; see the note
// on grpcServer about why the production clients cannot migrate per-RPC over
// their existing connections.
func dialTestConn(t *testing.T, svc *Service) *grpc.ClientConn {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
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
	return conn
}

func dialTestServer(t *testing.T, svc *Service) adminpb.AdminServiceClient {
	t.Helper()
	return adminpb.NewAdminServiceClient(dialTestConn(t, svc))
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
func TestGeneratedPathValidatesInput(t *testing.T) {
	client := dialTestServer(t, &Service{})

	_, err := client.GetCallerAliases(context.Background(),
		&adminpb.GetCallerAliasesRequest{})
	if err == nil {
		t.Fatal("expected an error for an empty caller")
	}
}

// TestUnmigratedRPCAnswersUnimplemented pins the property the migration relies
// on: an RPC that has not moved is cleanly unimplemented on this path rather
// than silently misbehaving, so the JSON path remains its only route.
func TestUnmigratedRPCAnswersUnimplemented(t *testing.T) {
	client := dialTestServer(t, &Service{})

	_, err := client.GetEntityOwners(context.Background(), &adminpb.GetEntityOwnersRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("got %v (code %v), want Unimplemented", err, status.Code(err))
	}
}
