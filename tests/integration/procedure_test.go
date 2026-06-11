//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/entity"
)

// startGRPCServerWithIR boots an in-process dynamic gRPC server against a
// caller-supplied IR (instead of the checkpoint), so a test can register
// an arbitrary procedure. Torn down via t.Cleanup.
func startGRPCServerWithIR(t *testing.T, h *Harness, ir *dsl.IR) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer()
	dynServer := entity.NewServer(h.Pool, h.Cache, h.Outbox, h.QueryCache)
	if err := dynServer.Register(srv, ir); err != nil {
		t.Fatalf("dynServer.Register: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// procWireDescs builds request/response message descriptors wire-
// compatible with what the dynamic server registers for a procedure:
// request = one int64 field `id` (#1); response = int64 `rows_affected`
// (#1). Matching field numbers + types is all protobuf needs to round
// trip across the two independently-built descriptors.
func procWireDescs(t *testing.T, name string) (protoreflect.MessageDescriptor, protoreflect.MessageDescriptor) {
	t.Helper()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	one := int32(1)
	sp := func(s string) *string { return &s }

	file := &descriptorpb.FileDescriptorProto{
		Name:    sp("atlantis/consumer/v1/proc_test.proto"),
		Package: sp("atlantis.consumer.v1"),
		Syntax:  sp("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: sp(name + "Request"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: sp("id"), Number: &one, Label: &opt, Type: &i64},
				},
			},
			{
				Name: sp(name + "Response"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: sp("rows_affected"), Number: &one, Label: &opt, Type: &i64},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().ByName(protoreflect.Name(name + "Request")),
		fd.Messages().ByName(protoreflect.Name(name + "Response"))
}

// TestGRPC_Procedure_DeleteRoundTrip proves a `procedure` declaration is
// served dynamically end-to-end: real gRPC dispatch → real PG DELETE with
// a bound arg → rows_affected response → a cache_invalidations row for the
// touched entity. Before dynamic procedure serving this returned
// Unimplemented.
func TestGRPC_Procedure_DeleteRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A scratch table the procedure deletes from.
	if _, err := h.PgxPool().Exec(ctx,
		`CREATE TABLE consumer.proc_target (id bigint PRIMARY KEY, n int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := h.PgxPool().Exec(ctx,
		`INSERT INTO consumer.proc_target (id, n) VALUES (1,10),(2,20),(7,70)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Procedures-only IR: one raw delete-by-id procedure touching a
	// (nominal) entity id. No entity descriptors needed.
	ir := &dsl.IR{
		Version: dsl.CurrentIRVersion,
		Procedures: []dsl.CustomProcedure{{
			Name:   "DeleteProcTarget",
			Owner:  "consumer.ProcTarget",
			Inputs: []dsl.QueryParam{{Name: "id", Type: dsl.FieldType{Name: "bigint"}}},
			Steps: []dsl.ProcedureStepIR{{
				Raw: &dsl.RawSQLIR{
					SQL:     "DELETE FROM consumer.proc_target WHERE id = $id",
					Touches: []string{"consumer.ProcTarget"},
				},
			}},
		}},
	}

	conn := startGRPCServerWithIR(t, h, ir)
	reqDesc, respDesc := procWireDescs(t, "DeleteProcTarget")

	req := dynamicpb.NewMessage(reqDesc)
	req.Set(reqDesc.Fields().ByName("id"), protoreflect.ValueOfInt64(7))
	resp := dynamicpb.NewMessage(respDesc)

	if err := conn.Invoke(ctx, "/atlantis.consumer.v1.CustomService/DeleteProcTarget", req, resp); err != nil {
		t.Fatalf("Invoke DeleteProcTarget: %v", err)
	}

	if got := resp.Get(respDesc.Fields().ByName("rows_affected")).Int(); got != 1 {
		t.Errorf("rows_affected = %d, want 1", got)
	}

	// Row 7 gone, others remain.
	var remaining int
	if err := h.PgxPool().QueryRow(ctx,
		`SELECT count(*) FROM consumer.proc_target WHERE id = 7`).Scan(&remaining); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if remaining != 0 {
		t.Errorf("row 7 should be deleted, still present")
	}

	// A cache invalidation was enqueued for the touched entity.
	var invs int
	if err := h.PgxPool().QueryRow(ctx,
		`SELECT count(*) FROM atlantis.cache_invalidations WHERE entity = 'consumer.ProcTarget'`).Scan(&invs); err != nil {
		t.Fatalf("count invalidations: %v", err)
	}
	if invs == 0 {
		t.Errorf("expected a cache_invalidations row for consumer.ProcTarget")
	}
}
