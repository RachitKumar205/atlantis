package entity

import (
	"context"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// TestRoute_EntityDeclaredAfterBootIsServed is the end-to-end run's first
// finding against Postgres: the server boots without Task, a reload declares
// it, and the next Create and Get on TaskService succeed on the same
// process.
func TestRoute_EntityDeclaredAfterBootIsServed(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the router against Postgres")
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.notes_task`)
	if _, err := pool.Exec(ctx, `CREATE TABLE atlantis.notes_task (id bigserial PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.notes_task`) })

	h := newRouteHarnessWith(t, pool)
	if err := h.srv.Load(routeIR(t, routeNoteOnly)); err != nil {
		t.Fatal(err)
	}
	if code := h.get(t, "notes.Task", "Task"); code != codes.Unimplemented {
		t.Fatalf("GetTask before the reload: %v, want Unimplemented", code)
	}

	if err := h.srv.Reload(routeIR(t, routeNoteAndTask), "v2"); err != nil {
		t.Fatal(err)
	}
	meta := h.srv.snapshot.Load().entities["notes.Task"]
	fields := meta.msgDesc.Fields()

	create := dynamicpb.NewMessage(meta.createRequestDesc)
	e := dynamicpb.NewMessage(meta.msgDesc)
	e.Set(fields.ByName("name"), protoreflect.ValueOfString("write the test"))
	create.Set(meta.createRequestDesc.Fields().ByName("entity"), protoreflect.ValueOfMessage(e))
	created := dynamicpb.NewMessage(meta.createResponseDesc)
	if err := h.conn.Invoke(ctx, "/atlantis.notes.v1.TaskService/CreateTask", create, created); err != nil {
		t.Fatalf("CreateTask after the reload: %v", status.Convert(err).Message())
	}
	id := created.Get(meta.createResponseDesc.Fields().ByName("entity")).Message().Get(fields.ByName("id")).Int()

	get := dynamicpb.NewMessage(meta.getRequestDesc)
	get.Set(meta.getRequestDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(id))
	got := dynamicpb.NewMessage(meta.getResponseDesc)
	if err := h.conn.Invoke(ctx, "/atlantis.notes.v1.TaskService/GetTask", get, got); err != nil {
		t.Fatalf("GetTask after the reload: %v", err)
	}
	if name := got.Get(meta.getResponseDesc.Fields().ByName("entity")).Message().Get(fields.ByName("name")).String(); name != "write the test" {
		t.Errorf("GetTask returned name %q", name)
	}
	// The unrouted call before the reload ran the stream chain; the two
	// routed calls after it did not.
	if n := h.streamCalls.Load(); n != 1 {
		t.Errorf("stream chain ran %d times, want 1", n)
	}
	if n := h.unaryCalls.Load(); n != 2 {
		t.Errorf("unary chain ran %d times, want 2", n)
	}
}
