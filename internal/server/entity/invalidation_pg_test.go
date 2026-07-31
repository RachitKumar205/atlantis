package entity

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// Drives the real write paths against a live Postgres and reads
// atlantis.cache_invalidations back.
//
// The SQL-string and index tests cannot catch what matters here, and did not:
// handleDelete scanned the parent key into a variable and never passed it to
// enqueueParents, so deleting a child invalidated nothing. It compiled because
// taking the address of an element counts as a use, and ten single-line
// mutations to the new code — including deleting the enqueueParents call from
// handleCreate outright — left the package suite green.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/server/entity/ -run ParentInvalidation -v

const invTestSchema = `
entity InvParent in invt {
  id bigint primary
  label text
  cache {
    read_through ttl=5m
    invalidate_on: write(self), write(InvChild where parent_id = self.id)
  }
}
entity InvChild in invt {
  id bigint primary
  parent_id bigint not null references invt.InvParent.id
}
`

func invIR(t *testing.T) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("inv.atl", []byte(invTestSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	// Without this every field gets number 0 and the descriptor build fails
	// with a field conflict. tidectl does it as part of codegen; a test that
	// builds an IR by hand has to do it too.
	codegen.AssignProtoNumbers(nil, ir)
	return ir
}

// The index must resolve from real parsed source, not just from a hand-built
// IR. A fixture assembled in Go can encode a shape the parser never produces.
func TestParentInvalidationRulesResolveFromSource(t *testing.T) {
	ir := invIR(t)
	rules := buildInboundIndex(ir)["invt.InvChild"]
	if len(rules) != 1 {
		t.Fatalf("invt.InvChild has %d inbound rules, want 1: %+v", len(rules), rules)
	}
	if rules[0].parentEntityID != "invt.InvParent" || rules[0].childCol != "parent_id" {
		t.Errorf("rule = %+v, want parent invt.InvParent via parent_id", rules[0])
	}
}

// The parent key must be scanned into the same Go type the parent's own
// handlers use to build a cache id. Scanning into *any lets pgx choose, and its
// choice differs for uuid, numeric, timestamptz and jsonb — producing an
// outbox row for an id nothing ever reads.
func TestInboundValuesUseTheSameRepresentationAsTheParent(t *testing.T) {
	ir := invIR(t)
	var child *dsl.Entity
	for i := range ir.Entities {
		if ir.Entities[i].Name == "InvChild" {
			child = &ir.Entities[i]
		}
	}
	meta := entityMetaFor(child, ir)

	if len(meta.inboundColMeta) != len(meta.inboundCols) {
		t.Fatalf("inboundColMeta has %d entries for %d columns; a rule naming an "+
			"unknown column would scan into the wrong slot",
			len(meta.inboundColMeta), len(meta.inboundCols))
	}
	targets := makeScanTargets(meta.inboundColMeta)
	if len(targets) != 1 {
		t.Fatalf("got %d scan targets, want 1", len(targets))
	}
	// parent_id is bigint, so the parent's own cache id comes from an int64.
	if _, ok := targets[0].(*int64); !ok {
		t.Errorf("scan target for a bigint parent key is %T, want *int64 — a "+
			"different type yields a different CompositeID than the parent's own "+
			"handlers produce, and the invalidation lands on a key nobody reads",
			targets[0])
	}
}

// A composite-PK parent cannot be addressed by one value, and must be rejected
// rather than silently producing a partial cache id.
func TestCompositePKParentIsRejected(t *testing.T) {
	src := `
entity CompParent in invt {
  aisle int not null
  slot  int not null
  primary by aisle, slot
  cache {
    read_through ttl=5m
    invalidate_on: write(CompChild where aisle = self.aisle)
  }
}
entity CompChild in invt {
  id bigint primary
  aisle int not null
}
`
	f, err := dsl.Parse("comp.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = dsl.Lower([]*dsl.File{f})
	if err == nil {
		t.Fatal("a composite-PK parent was accepted. One value cannot address a " +
			"two-column cache id, so every write to the child would enqueue an " +
			"invalidation for a key nothing reads — silently, forever")
	}
	if !contains(err.Error(), "sole primary key") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The end-to-end property: writing a child lands an invalidation for its PARENT
// in the outbox, in the write's own transaction, for create, update and delete.
//
// This is the test that catches the bug the SQL-shape assertions could not.
// handleDelete scanned the parent key and never passed it to enqueueParents;
// every string-level test stayed green while deleting a child invalidated
// nothing.
func TestParentInvalidationReachesTheOutbox(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the write paths")
	}
	ctx := context.Background()

	pool, err := pg.New(ctx, pg.Config{
		URL: url, MaxConns: 4, MinConns: 1,
		MaxConnIdleTime: time.Minute, MaxConnLifetime: time.Hour,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	raw := pool.Raw()
	exec := func(sql string) {
		t.Helper()
		if _, err := raw.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	reset := func() {
		_, _ = raw.Exec(ctx, `DROP TABLE IF EXISTS atlantis.invt_inv_child`)
		_, _ = raw.Exec(ctx, `DROP TABLE IF EXISTS atlantis.invt_inv_parent`)
		_, _ = raw.Exec(ctx, `DELETE FROM atlantis.cache_invalidations WHERE entity LIKE 'invt.%'`)
	}
	reset()
	t.Cleanup(reset)
	exec(`CREATE TABLE atlantis.invt_inv_parent (id BIGINT PRIMARY KEY, label TEXT)`)
	exec(`CREATE TABLE atlantis.invt_inv_child (
	        id BIGINT PRIMARY KEY,
	        parent_id BIGINT NOT NULL REFERENCES atlantis.invt_inv_parent(id))`)
	exec(`INSERT INTO atlantis.invt_inv_parent (id, label) VALUES (1, 'a'), (2, 'b')`)

	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(invIR(t), "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["invt.InvChild"]
	if meta == nil {
		t.Fatal("InvChild missing from the snapshot")
	}

	// parentsInvalidated reads the outbox and returns the parent row ids that
	// were enqueued since the last call.
	parentsInvalidated := func() []string {
		rows, err := raw.Query(ctx, `
SELECT row_id FROM atlantis.cache_invalidations
 WHERE entity = 'invt.InvParent' AND kind = 'invalidation' ORDER BY id`)
		if err != nil {
			t.Fatalf("read outbox: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, id)
		}
		_, _ = raw.Exec(ctx, `DELETE FROM atlantis.cache_invalidations WHERE entity LIKE 'invt.%'`)
		return out
	}

	decodeInto := func(src proto.Message) func(any) error {
		return func(dst any) error {
			proto.Reset(dst.(proto.Message))
			proto.Merge(dst.(proto.Message), src)
			return nil
		}
	}
	child := func(id, parentID int64) *dynamicpb.Message {
		m := dynamicpb.NewMessage(meta.msgDesc)
		m.Set(meta.msgDesc.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
		m.Set(meta.msgDesc.Fields().ByName("parent_id"), protoreflect.ValueOfInt64(parentID))
		return m
	}

	// CREATE: the new parent is invalidated.
	createReq := dynamicpb.NewMessage(meta.createRequestDesc)
	createReq.Set(meta.createRequestDesc.Fields().ByName("entity"),
		protoreflect.ValueOfMessage(child(10, 1)))
	if _, err := srv.handleCreate(ctx, meta, decodeInto(createReq)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := parentsInvalidated(); len(got) != 1 || got[0] != runtime.CompositeID(int64(1)) {
		t.Errorf("create invalidated %v, want the parent %q",
			got, runtime.CompositeID(int64(1)))
	}

	// UPDATE that reparents: BOTH the old and the new parent are invalidated.
	updReq := dynamicpb.NewMessage(meta.updateRequestDesc)
	updReq.Set(meta.updateRequestDesc.Fields().ByName("entity"),
		protoreflect.ValueOfMessage(child(10, 2)))
	if _, err := srv.handleUpdate(ctx, meta, decodeInto(updReq)); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := parentsInvalidated()
	want := map[string]bool{
		runtime.CompositeID(int64(1)): false,
		runtime.CompositeID(int64(2)): false,
	}
	for _, g := range got {
		want[g] = true
	}
	if !want[runtime.CompositeID(int64(1))] {
		t.Errorf("reparenting did not invalidate the parent the row LEFT; it still "+
			"lists a child it no longer has. got=%v", got)
	}
	if !want[runtime.CompositeID(int64(2))] {
		t.Errorf("reparenting did not invalidate the parent the row JOINED. got=%v", got)
	}

	// DELETE: the parent is invalidated. This is the case that shipped broken.
	delReq := dynamicpb.NewMessage(meta.deleteRequestDesc)
	delReq.Set(meta.deleteRequestDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(10))
	if _, err := srv.handleDelete(ctx, meta, decodeInto(delReq)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := parentsInvalidated(); len(got) != 1 || got[0] != runtime.CompositeID(int64(2)) {
		t.Errorf("delete invalidated %v, want the parent %q. A removed child stays "+
			"in the parent's cached body until TTL.", got, runtime.CompositeID(int64(2)))
	}
}

// noopCache satisfies runtime.Cache without caching anything. CurrentVersion
// returning 0 is what the outbox path expects for a row nothing has cached.
type noopCache struct{}

func (noopCache) Get(context.Context, string) ([]byte, error) {
	return nil, runtime.ErrCacheMiss
}
func (noopCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (noopCache) CurrentVersion(context.Context, string, string) (int64, error) {
	return 0, nil
}

// A procedure-written entity must not be served from the cache even when a
// reader is present. Asserted behaviourally: change the row underneath the
// handler and Get again. A cached read returns the old value.
//
// Without this, ignoring meta.cacheable in handleGet leaves every test green
// while ten shipped procedures write entities that would then serve stale rows.
func TestProcedureWrittenEntityIsReadUncached(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the read path")
	}
	ctx := context.Background()

	pool, err := pg.New(ctx, pg.Config{
		URL: url, MaxConns: 4, MinConns: 1,
		MaxConnIdleTime: time.Minute, MaxConnLifetime: time.Hour,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	raw := pool.Raw()

	reset := func() { _, _ = raw.Exec(ctx, `DROP TABLE IF EXISTS atlantis.procr_widget`) }
	reset()
	t.Cleanup(reset)
	if _, err := raw.Exec(ctx, `CREATE TABLE atlantis.procr_widget (id BIGINT PRIMARY KEY, label TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := raw.Exec(ctx, `INSERT INTO atlantis.procr_widget VALUES (1, 'before')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const src = `
entity Widget in procr {
  id bigint primary
  label text
  cache { read_through ttl=5m }
}
procedure TouchWidget for Widget {
  input { id: bigint }
  steps {
    sql touches(procr.Widget) {
      UPDATE atlantis.procr_widget SET label = 'after' WHERE id = $id
    }
  }
}
`
	f, err := dsl.Parse("procr.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	reader, err := read.New(noopCache{}, read.Config{LRUSize: 16, DefaultTTL: time.Minute})
	if err != nil {
		t.Fatalf("read.New: %v", err)
	}
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, reader)
	if err := srv.Reload(ir, "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["procr.Widget"]
	if meta == nil {
		t.Fatal("Widget missing from the snapshot")
	}
	if meta.cacheable {
		t.Fatal("Widget is written by a procedure and is still marked cacheable")
	}

	get := func() string {
		req := dynamicpb.NewMessage(meta.getRequestDesc)
		req.Set(meta.getRequestDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(1))
		resp, err := srv.handleGet(ctx, meta, func(dst any) error {
			proto.Reset(dst.(proto.Message))
			proto.Merge(dst.(proto.Message), req)
			return nil
		})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		m := resp.(*dynamicpb.Message)
		ent := m.Get(meta.getResponseDesc.Fields().ByName("entity")).Message()
		return ent.Get(meta.msgDesc.Fields().ByName("label")).String()
	}

	if got := get(); got != "before" {
		t.Fatalf("first read = %q, want before", got)
	}
	// Change the row behind the handler's back, exactly as a procedure would.
	if _, err := raw.Exec(ctx, `UPDATE atlantis.procr_widget SET label = 'after' WHERE id = 1`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := get(); got != "after" {
		t.Errorf("second read = %q, want after. The entity is written by a "+
			"procedure that cannot invalidate row bodies, so it must not be "+
			"served from cache — this read came from a cached body.", got)
	}
}

// A real error from the pre-update read must propagate, not be swallowed.
//
// The swallow was justified as "invalidating one parent beats failing the
// write", which Postgres does not permit: a genuine error aborts the
// transaction, so the write fails anyway with "current transaction is aborted"
// and the root cause discarded.
func TestPreUpdateReadPropagatesRealErrors(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the pre-update read")
	}
	ctx := context.Background()

	pool, err := pg.New(ctx, pg.Config{
		URL: url, MaxConns: 2, MinConns: 1,
		MaxConnIdleTime: time.Minute, MaxConnLifetime: time.Hour,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ir := invIR(t)
	var child *dsl.Entity
	for i := range ir.Entities {
		if ir.Entities[i].Name == "InvChild" {
			child = &ir.Entities[i]
		}
	}
	meta := entityMetaFor(child, ir)
	// A statement that cannot succeed — the table does not exist.
	meta.sqlSelectInbound = `SELECT parent_id FROM atlantis.definitely_not_a_table WHERE id = $1 FOR UPDATE`

	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	tx, err := pool.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	_, rerr := srv.readInboundValues(ctx, tx, meta, []any{int64(1)})
	if rerr == nil {
		t.Error("a failing pre-update read returned no error. Swallowing it does " +
			"not let the write proceed — the transaction is already aborted — it " +
			"only discards the reason.")
	}
}
