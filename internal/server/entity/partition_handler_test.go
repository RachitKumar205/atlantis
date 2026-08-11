package entity

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Every handler must bind the tenant. Driven through the handler, not through
// the helper it calls.
//
// This is the test the first version of this work did not have, and its absence
// is not a small gap. Two adversarial reviews disabled all eight partition call
// sites at once — three scoped reads, three write binds, the custom query and
// the procedure — and the ENTIRE repository suite stayed green. The structural
// tests passed because no `s.pool` call had been added and no BeginTx had lost
// its bind. The behavioural tests passed because none of them went through a
// handler.
//
// The cause was one thing: every existing test drives handlers with entities
// that declare no `partition by`, and for those the scoped path and the
// unscoped path are the same path. So the tests exercised the handlers and
// proved nothing about partitioning, while the partition tests exercised the
// helpers and proved nothing about the handlers.
//
// These tests close that by asserting on what reaches the database. That also
// makes them immune to the spelling games that defeated the structural suite —
// renaming the receiver, aliasing the pool to a local, hiding the read behind
// an accessor, or moving BeginTx into the exempt file. None of those change
// what the pool records.

const partitionedSchema = `
entity Doc in pt {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`

// partitionedServer returns a Server whose only entity is partitioned, backed
// by a pool that answers nothing and records everything.
func partitionedServer(t *testing.T) (*Server, *recordingPool, *entityMeta) {
	t.Helper()

	f, err := dsl.Parse("pt.atl", []byte(partitionedSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	pool := &recordingPool{}
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["pt.Doc"]
	if meta == nil {
		t.Fatal("pt.Doc missing from the snapshot")
	}
	// Without this the whole file proves nothing: every assertion below would
	// hold for an unpartitioned entity too.
	if !meta.partitioned {
		t.Fatal("pt.Doc declares `partition by` and was not marked partitioned, " +
			"so every assertion in this file would pass on the unscoped path")
	}
	return srv, pool, meta
}

// withEntity builds a request whose `entity` sub-message is present.
//
// Create and Update refuse an absent entity before they open a transaction, so
// a request without one never reaches the code under test. The first version of
// this file omitted it, and both cases reported "issued no set_partition" with
// an empty statement list — the test working, and pointing at the fixture.
func withEntity(t *testing.T, desc protoreflect.MessageDescriptor) *dynamicpb.Message {
	if t != nil {
		t.Helper()
	}
	req := dynamicpb.NewMessage(desc)
	if fd := desc.Fields().ByName("entity"); fd != nil {
		req.Mutable(fd)
	}
	return req
}

// fill returns a decoder that copies src into the handler's request message.
func fill(src *dynamicpb.Message) func(any) error {
	return func(dst any) error {
		proto.Reset(dst.(proto.Message))
		if src != nil {
			proto.Merge(dst.(proto.Message), src)
		}
		return nil
	}
}

func TestEveryHandlerBindsTheTenant(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error
	}{
		{"Get", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			req := dynamicpb.NewMessage(meta.getRequestDesc)
			_, err := srv.handleGet(ctx, meta, fill(req))
			return err
		}},
		{"BatchGet", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			req := dynamicpb.NewMessage(meta.batchGetRequestDesc)
			pkFD := meta.batchGetRequestDesc.Fields().Get(0)
			req.Mutable(pkFD).List().Append(protoreflect.ValueOfInt64(1))
			_, err := srv.handleBatchGet(ctx, meta, fill(req))
			return err
		}},
		{"Query", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			req := dynamicpb.NewMessage(meta.queryRequestDesc)
			_, err := srv.handleQuery(ctx, meta, fill(req))
			return err
		}},
		{"Create", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleCreate(ctx, meta, fill(withEntity(t, meta.createRequestDesc)))
			return err
		}},
		{"Update", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleUpdate(ctx, meta, fill(withEntity(t, meta.updateRequestDesc)))
			return err
		}},
		{"Delete", func(t *testing.T, srv *Server, meta *entityMeta, ctx context.Context) error {
			req := dynamicpb.NewMessage(meta.deleteRequestDesc)
			_, err := srv.handleDelete(ctx, meta, fill(req))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, pool, meta := partitionedServer(t)
			ctx := runtime.WithCallerPartition(context.Background(), "acme")

			// The handler is expected to fail: the recording pool serves no
			// rows. What matters is what it sent before it failed.
			_ = tc.call(t, srv, meta, ctx)

			pool.boundOnly(t, "acme")
			if !pool.sawBind() {
				t.Fatalf("%s issued no set_partition. Every statement it sent ran "+
					"with no tenant bound, so the row-level security policy had "+
					"nothing to compare against — returning no rows on a role the "+
					"policy applies to, and every tenant's rows on a role that "+
					"bypasses it. Statements sent:\n  %v", tc.name, pool.statements)
			}
			if len(pool.onPool) > 0 {
				t.Errorf("%s sent statements outside any transaction, where no "+
					"tenant can be bound:\n  %v", tc.name, pool.onPool)
			}
			// The bind must come first. A policy applies from the moment a
			// statement runs, so a bind issued after the first read or write
			// scopes nothing that came before it.
			for i, sql := range pool.statements {
				if strings.Contains(sql, "set_partition") {
					if i != 0 {
						t.Errorf("%s bound the tenant at statement %d, not first. "+
							"Statement 0 was %q and ran unscoped", tc.name, i, pool.statements[0])
					}
					break
				}
			}
		})
	}
}

// And with no tenant in context, no handler may reach the database at all.
//
// An unbound statement is only safe on a deployment whose database role obeys
// row-level security. That is not something this layer can see, so refusing is
// the only behaviour that is correct under both.
func TestNoHandlerReachesTheDatabaseWithoutATenant(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(srv *Server, meta *entityMeta, ctx context.Context) error
	}{
		{"Get", func(srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleGet(ctx, meta, fill(dynamicpb.NewMessage(meta.getRequestDesc)))
			return err
		}},
		{"Query", func(srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleQuery(ctx, meta, fill(dynamicpb.NewMessage(meta.queryRequestDesc)))
			return err
		}},
		{"Create", func(srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleCreate(ctx, meta, fill(withEntity(nil, meta.createRequestDesc)))
			return err
		}},
		{"Update", func(srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleUpdate(ctx, meta, fill(withEntity(nil, meta.updateRequestDesc)))
			return err
		}},
		{"Delete", func(srv *Server, meta *entityMeta, ctx context.Context) error {
			_, err := srv.handleDelete(ctx, meta, fill(dynamicpb.NewMessage(meta.deleteRequestDesc)))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, pool, meta := partitionedServer(t)

			if err := tc.call(srv, meta, context.Background()); err == nil {
				t.Errorf("%s succeeded with no tenant in context", tc.name)
			}
			for _, sql := range pool.statements {
				if strings.Contains(sql, "pt_doc") {
					t.Errorf("%s sent %q with no tenant bound", tc.name, sql)
				}
			}
		})
	}
}

// An entity with no `partition by` must still reach the database, and must not
// pay for a transaction.
//
// Asserted positively. The earlier version of this check asserted only
// absences — no transaction, no bind — which a scoped read that did nothing at
// all would also satisfy: replacing `return fn(s.pool)` with `return nil` left
// it green while every unpartitioned read in the system silently returned
// nothing.
func TestUnpartitionedHandlerStillReadsAndDoesNotBind(t *testing.T) {
	f, err := dsl.Parse("pt.atl", []byte(`
entity Plain in pt {
  id   bigint primary
  body text
}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	pool := &recordingPool{}
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["pt.Plain"]
	if meta == nil {
		t.Fatal("pt.Plain missing from the snapshot")
	}

	_, _ = srv.handleGet(context.Background(), meta, fill(dynamicpb.NewMessage(meta.getRequestDesc)))

	if len(pool.statements) == 0 {
		t.Fatal("the read reached the database not at all. An unpartitioned Get " +
			"must still run its query; a scoped read that returns without calling " +
			"fn would look exactly like this")
	}
	if pool.txOpened != 0 {
		t.Errorf("opened %d transactions for an unpartitioned entity, want 0",
			pool.txOpened)
	}
	if pool.sawBind() {
		t.Error("bound a tenant for an entity that declares no partition")
	}
}

// Custom queries and procedures must bind too, and they were the two sites a
// review neutered individually while every test stayed green.
//
// They take their flag from the entities they touch, not from an entity of
// their own, so they need their own coverage: `cqm.partitioned` and
// `pm.partitioned` are resolved in buildSnapshot, and nothing in the entity
// tests reaches either.
func TestCustomSQLBindsTheTenant(t *testing.T) {
	src := partitionedSchema + `
query DocsByTenant for Doc {
  input { id: bigint }
  output { body: text }
  sql touches(Doc) {
    SELECT body FROM pt_doc WHERE id = $id
  }
}

procedure BlankBody for Doc {
  input { id: bigint }
  steps {
    sql touches(Doc) {
      UPDATE pt_doc SET body = '' WHERE id = $id
    }
  }
}
`
	f, err := dsl.Parse("pt.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	newServer := func(t *testing.T) (*Server, *recordingPool, *entitySnapshot) {
		t.Helper()
		pool := &recordingPool{}
		srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
		if err := srv.Reload(ir, "test"); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		return srv, pool, srv.snapshot.Load()
	}

	t.Run("custom query", func(t *testing.T) {
		srv, pool, snap := newServer(t)
		cqm := snap.customMeta["pt:DocsByTenant"]
		if cqm == nil {
			t.Fatalf("query missing from the snapshot: %v", snap.customMeta)
		}
		if !cqm.partitioned {
			t.Fatal("the query touches a partitioned entity and was not marked " +
				"partitioned, so it runs on the bare pool with no tenant bound")
		}
		ctx := runtime.WithCallerPartition(context.Background(), "acme")
		_, _ = srv.executeCustomQueryWithReq(ctx, cqm, dynamicpb.NewMessage(cqm.requestDesc))

		pool.boundOnly(t, "acme")
		if !pool.sawBind() {
			t.Errorf("the custom query issued no set_partition. Its body is opaque "+
				"author text with nowhere to inject a tenant predicate, which is "+
				"exactly the case the policy exists to cover:\n  %v", pool.statements)
		}
		if len(pool.onPool) > 0 {
			t.Errorf("statements ran outside a transaction: %v", pool.onPool)
		}
	})

	t.Run("procedure", func(t *testing.T) {
		srv, pool, snap := newServer(t)
		pm := snap.procMeta["pt:BlankBody"]
		if pm == nil {
			t.Fatalf("procedure missing from the snapshot: %v", snap.procMeta)
		}
		if !pm.partitioned {
			t.Fatal("the procedure touches a partitioned entity and was not marked " +
				"partitioned, so every step runs unscoped")
		}
		ctx := runtime.WithCallerPartition(context.Background(), "acme")
		_, _ = srv.executeCustomProcedureWithReq(ctx, pm, dynamicpb.NewMessage(pm.requestDesc))

		pool.boundOnly(t, "acme")
		if !pool.sawBind() {
			t.Errorf("the procedure issued no set_partition, so every step wrote "+
				"and read unscoped:\n  %v", pool.statements)
		}
		if len(pool.statements) > 0 && !strings.Contains(pool.statements[0], "set_partition") {
			t.Errorf("the procedure's first statement was %q, not the bind. A step "+
				"that runs before the bind is unscoped", pool.statements[0])
		}
	})
}

// The procedure path must fail closed too.
//
// It is the one of the eight call sites that does not route through
// scopedReadIf, so the "fails closed" property the shared helper enforces is
// not enforced for it by construction. A review replaced its bind with
// `_ = runtime.BindPartition(ctx, tx)` and the whole package stayed green,
// because every procedure test supplied a tenant.
func TestProcedureFailsClosedWithoutATenant(t *testing.T) {
	src := partitionedSchema + `
procedure BlankBody for Doc {
  input { id: bigint }
  steps {
    sql touches(Doc) {
      UPDATE pt_doc SET body = '' WHERE id = $id
    }
  }
}
`
	f, err := dsl.Parse("pt.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	pool := &recordingPool{}
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "test"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	pm := srv.snapshot.Load().procMeta["pt:BlankBody"]
	if pm == nil || !pm.partitioned {
		t.Fatal("the procedure is missing or not marked partitioned")
	}

	if _, err := srv.executeCustomProcedureWithReq(
		context.Background(), pm, dynamicpb.NewMessage(pm.requestDesc)); err == nil {
		t.Error("the procedure ran with no tenant in context. Every step is " +
			"caller-authored SQL against a partitioned entity, running unscoped")
	}
	for _, sql := range pool.statements {
		if strings.Contains(sql, "pt_doc") {
			t.Errorf("a step reached the database unscoped: %q", sql)
		}
	}
}
