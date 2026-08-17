package codegen

import (
	"strings"
	"testing"
)

// The code `tidectl codegen` emits is what callers deploy, and it has to bind
// the tenant on every path that touches a partitioned table.
//
// # Why this test exists
//
// atlantis serves entities from two places. The dynamic dispatcher in
// internal/server/entity was fixed to bind on all eight of its paths, tested
// end to end against a restricted role, and mutation-tested. The emitter was
// never touched. A review emitted a `partition by` entity and found:
//
// no handler called set_partition, and only Query carried a tenant predicate.
//
// Be precise about what that did and did not mean, because the first version
// of this comment overstated it and a review corrected me. On a single-PK
// entity, Get, List and BatchGet DELEGATE to Query, so they inherited its
// predicate and were scoped. What was genuinely unscoped: the primary-key
// fetch after a query-cache hit, the Create and Update read-backs, Delete,
// vector search, the include-attach helper, and every custom query and
// procedure. The cache key also omitted the tenant, so one tenant could be
// served another tenant's primary keys.
//
// A predicate is also not the same guarantee as a binding. It is what the
// emitter can forget, and it forgot on every path above.
//
// So this asserts the property in the terms a caller would state it: **no
// statement against a partitioned entity runs without the tenant bound.**
func TestEmitGoServer_PartitionedEntityBindsEveryRead(t *testing.T) {
	ir := lower(t, `
entity Doc in shop {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`)
	files, err := EmitGoServer(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	var src string
	for _, f := range files {
		parseAsGo(t, f.Content)
		if strings.Contains(f.Path, "doc_server.go") {
			src = f.Content
		}
	}
	if src == "" {
		t.Fatal("no doc_server.go emitted")
	}

	// Every read must go through the shared helper, and it must be told this
	// entity is partitioned. `ScopedRead(..., false, ...)` compiles, runs on
	// the bare pool, and is exactly the bug.
	if n := strings.Count(src, "runtime.ScopedRead(ctx, s.DB, true,"); n < 2 {
		t.Errorf("only %d read path(s) bind the tenant; want at least the "+
			"filtered query and the primary-key fetch. A path that does not bind "+
			"reads every tenant's rows on a role that bypasses row-level "+
			"security, and none on a role that does not:\n%s", n, src)
	}
	if strings.Contains(src, "runtime.ScopedRead(ctx, s.DB, false,") {
		t.Error("a read path on a partitioned entity was emitted as unpartitioned")
	}

	// The class assertion, not a list of instances: NO statement may reach the
	// bare pool. Enumerating the seven RPCs is what produced this defect in the
	// first place — the emitter scoped the one path someone thought of and left
	// six unscoped, and a test naming those seven would pass the moment an
	// eighth was added.
	if n := strings.Count(src, "s.DB.Query") + strings.Count(src, "s.DB.Exec"); n != 0 {
		t.Errorf("%d statement(s) on a partitioned entity run on the bare pool, "+
			"outside any bound transaction:\n%s", n, src)
	}
	if n := strings.Count(src, "runtime.BindWrite(ctx, true, tx)"); n < 3 {
		t.Errorf("only %d write transaction(s) bind; want Create, Update and "+
			"Delete. An unbound write lets a caller stamp a row with another "+
			"tenant, which the read policy then hides from its real owner", n)
	}

	// The primary-key fetch is the one that matters most and reads least like a
	// risk: a cache hit arrives with PKs only, and the fetch carries no filter
	// and no tenant predicate.
	// Anchored on the FUNCTION DEFINITION, not on the name. The first
	// occurrence of the name is the cache-hit call site inside Query, and a
	// window measured from there can reach the binding in Query's own body —
	// so the assertion would pass with the fetch itself unbound.
	def := strings.Index(src, "FromPKs(ctx context.Context, pks []string")
	if def < 0 {
		t.Fatal("no primary-key fetch function emitted")
	}
	body := src[def:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "runtime.ScopedRead(ctx, s.DB, true,") {
		t.Error("the primary-key fetch after a cache hit does not bind. It runs " +
			"`WHERE id = ANY($1)` with no filter and no tenant predicate, so it " +
			"returns whatever keys the cache held — including another tenant's:\n" + body)
	}

	// And the cache key must carry the tenant, or tenant B hits tenant A's
	// entry and fetches A's keys.
	if !strings.Contains(src, "queryresult.Hash(\n\t\t\"shop.Doc\",\n\t\tpartitionVal,") {
		t.Errorf("the query cache key does not include the tenant. Tenant A runs "+
			"a filter, its primary keys are stored; tenant B runs the identical "+
			"filter and hits that entry:\n%s", src)
	}
}

// An entity without `partition by` must be untouched: no transaction per read,
// and an empty tenant segment in the cache key.
//
// The counterpart to the test above. A binding imposed on every entity would
// cost a transaction per read for the overwhelming majority that declare no
// partition, to serve a clause they do not use.
func TestEmitGoServer_UnpartitionedEntityIsUnchanged(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id    bigint primary
  email text not null unique
}
`)
	files, err := EmitGoServer(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	// Without this, a rename makes the loop body run zero times and the test
	// passes having asserted nothing. Two of the three tests in this file were
	// vacuous in exactly that way.
	checked := false
	for _, f := range files {
		if !strings.Contains(f.Path, "account_server.go") {
			continue
		}
		checked = true
		if strings.Contains(f.Content, "runtime.ScopedRead(ctx, s.DB, true,") {
			t.Error("an unpartitioned entity opens a transaction per read")
		}
		if strings.Contains(f.Content, "runtime.CallerPartition(ctx)") {
			t.Error("an unpartitioned entity requires a tenant, so every request " +
				"to it would fail on a deployment that sends no tenant header")
		}
		if !strings.Contains(f.Content, "queryresult.Hash(\n\t\t\"consumer.Account\",\n\t\t\"\",") {
			t.Errorf("the cache key for an unpartitioned entity should carry an "+
				"empty tenant segment:\n%s", f.Content)
		}
	}
	if !checked {
		t.Fatal("no account_server.go was emitted, so this test asserted nothing")
	}
}

// The composite-PK entity takes a DIFFERENT BatchGet branch, and it has to bind
// too.
//
// The emitter has three BatchGet forms — a Query-wrapped one, a single-PK
// direct fallback, and a composite-PK loop — and the test above exercises only
// the first. Unbinding the composite branch survived that test with everything
// green, because no entity in it has a composite key. A gate proven on one
// shape says nothing about the shape beside it.
func TestEmitGoServer_CompositePKPartitionedEntityBinds(t *testing.T) {
	ir := lower(t, `
entity Line in shop {
  order_id bigint
  line_no  int
  tenant   varchar(16) not null
  note     text
  primary by order_id, line_no
  partition by tenant
}
`)
	files, err := EmitGoServer(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	checked := false
	for _, f := range files {
		if !strings.Contains(f.Path, "line_server.go") {
			continue
		}
		checked = true
		parseAsGo(t, f.Content)
		if n := strings.Count(f.Content, "s.DB.Query") + strings.Count(f.Content, "s.DB.Exec"); n != 0 {
			t.Errorf("%d statement(s) on a composite-PK partitioned entity run on "+
				"the bare pool:\n%s", n, f.Content)
		}
		if strings.Contains(f.Content, "runtime.ScopedRead(ctx, s.DB, false,") {
			t.Error("the composite-PK batch read was emitted as unpartitioned")
		}
	}
	if !checked {
		t.Fatal("no line_server.go was emitted, so this test asserted nothing")
	}
}

// EVERY emitted shape, for a partitioned entity, must reach the database only
// through a bound transaction.
//
// # Why this test replaces a narrower one
//
// The first version of the "no statement on the bare pool" assertion ran
// against a single entity with one primary key and no inbound references. It
// read like a class assertion and was not one: the fixture simply never caused
// the leaking code to be emitted. A review added an entity with an inbound
// reference to that same fixture and the count went from 0 to 1 — the
// include-attach helper read the child table unbound, and returned another
// tenant's rows in a live database.
//
// Two more shapes had a bind that no test could kill: the fallback BatchGet
// (chosen when the primary-key type has no typed in-list arm) and vector
// search. No fixture in the repository declared `index hnsw` together with
// `partition by`.
//
// So the fixture below declares every shape the emitter branches on. A shape
// added later still needs a line here — but the assertion is now over the
// whole emitted package rather than one file, so a new leaking helper in an
// EXISTING shape is caught without anyone remembering.
func TestEmitGoServer_EveryShapeBindsOrDoesNotTouchTheDatabase(t *testing.T) {
	ir := lower(t, `
entity Doc in shop {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}

entity Note in shop {
  id      bigint primary
  doc_id  bigint references shop.Doc.id
  tenant  varchar(16) not null
  txt     text
  partition by tenant
}

entity Line in shop {
  order_id bigint
  line_no  int
  tenant   varchar(16) not null
  note     text
  primary by order_id, line_no
  partition by tenant
}

entity Reading in shop {
  taken_at timestamptz primary
  tenant   varchar(16) not null
  value    float8
  partition by tenant
}

entity Soft in shop {
  id         bigint primary
  tenant     varchar(16) not null
  body       text
  deleted_at timestamptz
  soft_delete by deleted_at
  partition by tenant
}

entity Vec in shop {
  id     bigint primary
  tenant varchar(16) not null
  vec    vector(8)
  index hnsw on vec ops cosine
  partition by tenant
}
`)
	files, err := EmitGoServer(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	if len(files) < 6 {
		t.Fatalf("expected a server file per entity plus register.go, got %d", len(files))
	}
	for _, f := range files {
		parseAsGo(t, f.Content)
		assertNoBarePoolStatements(t, f.Path, f.Content)
	}
}

// Custom queries and procedures are emitted by a DIFFERENT entry point, so the
// assertion above cannot see them.
//
// Both ran on the bare pool. The custom query read with no transaction; the
// custom procedure opened a transaction and never bound it, so an UPDATE
// against a partitioned table either wrote another tenant's row or silently
// affected nothing and was reported as success.
//
// This is the surface the whole design exists to protect: a custom body is
// opaque author text with nowhere to inject a tenant predicate, which is the
// stated reason for delegating to row-level security in the first place.
func TestEmitCustomServer_BindsWhenItTouchesAPartitionedEntity(t *testing.T) {
	ir := lower(t, `
entity Doc in shop {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}

query DocsByTenant for Doc {
  input { min_id: bigint }
  output as Doc
  sql touches(Doc) {
    SELECT id, tenant, body FROM shop_doc WHERE id > $min_id
  }
}

procedure BlankDoc for Doc {
  input { doc_id: bigint }
  steps {
    sql touches(Doc) {
      UPDATE shop_doc SET body = '' WHERE id = $doc_id
    }
  }
}
`)
	files, err := EmitCustomServer(ir, GenConfig{})
	if err != nil {
		t.Fatalf("EmitCustomServer: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no custom server emitted")
	}
	for _, f := range files {
		parseAsGo(t, f.Content)
		assertNoBarePoolStatements(t, f.Path, f.Content)
	}
}

// assertNoBarePoolStatements fails if any statement reaches s.DB directly.
//
// The allowed exception is BeginTx: a write path owns its transaction and binds
// it with runtime.BindWrite as the first statement, and ScopedRead/ScopedQuerier
// call BeginTx themselves.
//
// Matching on "s.DB." rather than on a list of method names is deliberate. A
// list of Query/Exec misses QueryRow, SendBatch, CopyFrom and anything added to
// the interface later, and this defect was created twice by reasoning about a
// list of call sites instead of the property.
func assertNoBarePoolStatements(t *testing.T, path, src string) {
	t.Helper()
	// "s.DB", not "s.DB." — the trailing dot lets an ALIAS through:
	//
	//	db := s.DB
	//	scanIntoDoc(db.QueryRow(ctx, sqlGetDoc, id...), out)
	//
	// A review used exactly that to read a partitioned table unbound with this
	// assertion still reporting zero. The dispatcher's own guard already
	// defends against the same alias and says a review "defeated the string
	// form three ways in a minute"; this one was the weaker of the two.
	//
	// So every mention of the pool is a finding unless it is one of the three
	// forms that hand it to code which binds.
	for i, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "s.DB") {
			continue
		}
		if strings.Contains(line, "runtime.ScopedRead(ctx, s.DB,") ||
			strings.Contains(line, "runtime.ScopedQuerier(ctx, s.DB,") ||
			strings.Contains(line, "s.DB.BeginTx(ctx)") {
			continue
		}
		t.Errorf("%s:%d reaches the database outside a bound transaction:\n\t%s\n"+
			"On a role that bypasses row-level security this returns every "+
			"tenant's rows; on a role that does not it returns none.",
			path, i+1, strings.TrimSpace(line))
	}
	// A scope that is told `false` is not a scope.
	//
	// Every caller above passes the pool through an ALLOWED form, so the loop
	// skips the line — and `ScopedRead(ctx, s.DB, false, ...)` runs straight on
	// the pool. Five bindings could be turned off with the whole suite green
	// this way: vector search, the fallback BatchGet, the include-attach
	// helper, and both custom-SQL handlers.
	//
	// Every entity in this test's fixture declares `partition by`, so no
	// `false` is legitimate in these files.
	for _, form := range []string{
		"runtime.ScopedRead(ctx, s.DB, false,",
		"runtime.ScopedQuerier(ctx, s.DB, false)",
		"runtime.BindWrite(ctx, false, tx)",
	} {
		if n := strings.Count(src, form); n > 0 {
			t.Errorf("%s has %d occurrence(s) of %q. The scope is present and "+
				"switched off, which runs on the bare pool while reading like a "+
				"binding", path, n, form)
		}
	}

	// And every BeginTx must be followed by a bind.
	if n, b := strings.Count(src, "s.DB.BeginTx(ctx)"), strings.Count(src, "runtime.BindWrite(ctx, "); n > b {
		t.Errorf("%s opens %d transaction(s) and binds %d. An unbound write "+
			"transaction lets a caller stamp a row with another tenant", path, n, b)
	}
}
