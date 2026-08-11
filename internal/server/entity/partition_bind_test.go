package entity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The structural tests in partition_wiring_test.go prove the call sites exist.
// They cannot prove the call sites do anything: making bindWrite a no-op,
// making scopedReadIf skip the bind, or making entityMeta.partitioned always
// false all leave every call site in place, and all three survived the
// structural suite. What follows records what actually reaches the database.

// recordingPool is a runtime.Pool that answers nothing and remembers
// everything.
type recordingPool struct {
	mu         sync.Mutex
	statements []string // every SQL string, pool and transaction alike
	// boundTenants holds the ARGUMENT of every set_partition call.
	//
	// The first version of this fake recorded only SQL strings and discarded
	// args, so every assertion in this file reduced to "a set_partition
	// happened". A review then made the dispatcher bind the literal tenant
	// "attacker" for every caller — a total cross-tenant read and write — and
	// the entire repository suite passed. Recording which tenant is bound is
	// the difference between testing that a lock was turned and testing that
	// it was the right key.
	boundTenants []string
	// stmtTx and boundTx record WHICH transaction each statement and each bind
	// belongs to. 0 means the bare pool.
	stmtTx   []int
	boundTx  []int
	txOpened int
	onPool   []string // statements issued outside any transaction
}

func (p *recordingPool) note(sql string, txID int, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statements = append(p.statements, sql)
	p.stmtTx = append(p.stmtTx, txID)
	if txID == 0 {
		p.onPool = append(p.onPool, sql)
	}
	if strings.Contains(sql, "set_partition") && len(args) > 0 {
		p.boundTenants = append(p.boundTenants, fmt.Sprint(args[0]))
		p.boundTx = append(p.boundTx, txID)
	}
}

// boundInSameTransaction reports whether every statement ran in a transaction
// that had been bound.
//
// The flat statement list cannot answer this, and a review used exactly that
// gap: it changed bindWrite to open a SECOND transaction, bind that one, and
// leave the real one unscoped. Every write path then ran unscoped while
// `set_partition` executed in a throwaway — and the whole repository suite
// passed, because the bind was recorded, the tenant was right, and it came
// first. Recording WHICH transaction each statement belongs to is what turns
// "a bind happened" into "this transaction is bound".
func (p *recordingPool) boundInSameTransaction() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	bound := map[int]bool{}
	for _, id := range p.boundTx {
		bound[id] = true
	}
	for i, id := range p.stmtTx {
		if id == 0 || strings.Contains(p.statements[i], "set_partition") {
			continue
		}
		if !bound[id] {
			return false
		}
	}
	return true
}

func (p *recordingPool) sawBind() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.statements {
		if strings.Contains(s, "set_partition") {
			return true
		}
	}
	return false
}

// bound returns every tenant this pool was asked to bind.
func (p *recordingPool) bound() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.boundTenants...)
}

// boundOnly asserts the pool bound exactly the expected tenant, once.
func (p *recordingPool) boundOnly(t *testing.T, want string) {
	t.Helper()
	if !p.boundInSameTransaction() {
		t.Errorf("a statement ran in a transaction that was never bound. A bind "+
			"in some OTHER transaction leaves this one unscoped, and every "+
			"assertion about a bind having happened still passes:\n  statements %v\n"+
			"  statement tx ids %v\n  bound tx ids %v",
			p.statements, p.stmtTx, p.boundTx)
	}
	got := p.bound()
	if len(got) != 1 || got[0] != want {
		t.Errorf("bound %v, want exactly [%s]. Binding a tenant is not the same "+
			"as binding the CALLER'S tenant: a dispatcher that binds a constant "+
			"serves every caller as that tenant, and every assertion about "+
			"set_partition merely happening would still pass", got, want)
	}
}

func (p *recordingPool) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	p.note(sql, 0, args...)
	return errRow{}
}

func (p *recordingPool) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	p.note(sql, 0, args...)
	return nil, errProbe
}

func (p *recordingPool) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	p.note(sql, 0, args...)
	return nil, errProbe
}

func (p *recordingPool) BeginTx(ctx context.Context) (runtime.Tx, error) {
	p.mu.Lock()
	p.txOpened++
	id := p.txOpened
	p.mu.Unlock()
	return &recordingTx{pool: p, id: id}, nil
}

type recordingTx struct {
	pool *recordingPool
	id   int
}

func (t *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	t.pool.note(sql, t.id, args...)
	return errRow{}
}

func (t *recordingTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	t.pool.note(sql, t.id, args...)
	return nil, errProbe
}

func (t *recordingTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	t.pool.note(sql, t.id, args...)
	return fakeTag{}, nil
}

func (t *recordingTx) Commit(ctx context.Context) error   { return nil }
func (t *recordingTx) Rollback(ctx context.Context) error { return nil }

var errProbe = errors.New("recording pool: no rows are served")

type errRow struct{}

func (errRow) Scan(dest ...any) error { return errProbe }

// A read of a partitioned entity must open a transaction and bind the tenant
// before it issues the read.
//
// Order matters and is asserted: a row-level security policy applies from the
// moment a statement runs, so a bind issued after the SELECT scopes nothing.
func TestScopedReadBindsBeforeReading(t *testing.T) {
	pool := &recordingPool{}
	s := &Server{pool: pool}
	ctx := runtime.WithCallerPartition(context.Background(), "acme")

	meta := &entityMeta{partitioned: true}
	err := s.scopedRead(ctx, meta, func(q querier) error {
		_, _ = q.Query(ctx, "SELECT 1 FROM atlantis.doc")
		return nil
	})
	if err != nil {
		t.Fatalf("scopedRead: %v", err)
	}

	if pool.txOpened != 1 {
		t.Errorf("opened %d transactions, want 1. A statement on the bare pool "+
			"has nothing to bind the tenant to", pool.txOpened)
	}
	if !pool.sawBind() {
		t.Fatalf("no set_partition was issued. The read ran with no tenant bound, "+
			"so the policy had nothing to compare against:\n  %v", pool.statements)
	}
	if len(pool.onPool) > 0 {
		t.Errorf("statements were issued outside the transaction: %v", pool.onPool)
	}
	bindAt, readAt := -1, -1
	for i, sql := range pool.statements {
		if strings.Contains(sql, "set_partition") && bindAt < 0 {
			bindAt = i
		}
		if strings.Contains(sql, "atlantis.doc") && readAt < 0 {
			readAt = i
		}
	}
	if bindAt < 0 || readAt < 0 || bindAt > readAt {
		t.Errorf("the tenant was bound at position %d and the read ran at %d. "+
			"The policy applies from the first statement, so a bind after the "+
			"read scopes nothing:\n  %v", bindAt, readAt, pool.statements)
	}
}

// An entity with no `partition by` must not pay for a transaction it does not
// need — otherwise the cheap path quietly becomes the expensive one for every
// entity in the schema.
func TestUnpartitionedReadStaysOnThePool(t *testing.T) {
	pool := &recordingPool{}
	s := &Server{pool: pool}

	meta := &entityMeta{partitioned: false}
	err := s.scopedRead(context.Background(), meta, func(q querier) error {
		_, _ = q.Query(context.Background(), "SELECT 1 FROM atlantis.doc")
		return nil
	})
	if err != nil {
		t.Fatalf("scopedRead: %v", err)
	}
	if pool.txOpened != 0 {
		t.Errorf("opened %d transactions for an unpartitioned entity, want 0", pool.txOpened)
	}
	if pool.sawBind() {
		t.Error("bound a tenant for an entity that declares no partition")
	}
}

// With no tenant in context, a partitioned read must refuse and issue nothing.
//
// Failing closed is the whole design: an unbound read returns nothing on a role
// row-level security applies to, but everything on a role that bypasses it, and
// the deployment's role is not something this layer can see.
func TestScopedReadRefusesWithNoTenant(t *testing.T) {
	pool := &recordingPool{}
	s := &Server{pool: pool}

	meta := &entityMeta{partitioned: true}
	ran := false
	err := s.scopedRead(context.Background(), meta, func(q querier) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("a partitioned read with no tenant in context was allowed to proceed")
	}
	if ran {
		t.Error("the read body ran despite there being no tenant to scope it to")
	}
	for _, sql := range pool.statements {
		if strings.Contains(sql, "atlantis.doc") {
			t.Errorf("a statement reached the database unscoped: %q", sql)
		}
	}
}

// bindWrite must actually bind, and must refuse when it cannot.
func TestBindWrite(t *testing.T) {
	t.Run("binds a partitioned entity", func(t *testing.T) {
		pool := &recordingPool{}
		s := &Server{pool: pool}
		tx, _ := pool.BeginTx(context.Background())
		ctx := runtime.WithCallerPartition(context.Background(), "acme")

		if err := s.bindWrite(ctx, &entityMeta{partitioned: true}, tx); err != nil {
			t.Fatalf("bindWrite: %v", err)
		}
		if !pool.sawBind() {
			t.Error("bindWrite issued no set_partition, so every statement in this " +
				"transaction writes and reads unscoped")
		}
	})

	t.Run("refuses when there is no tenant", func(t *testing.T) {
		pool := &recordingPool{}
		s := &Server{pool: pool}
		tx, _ := pool.BeginTx(context.Background())

		if err := s.bindWrite(context.Background(), &entityMeta{partitioned: true}, tx); err == nil {
			t.Error("bindWrite accepted a request with no tenant, so the write " +
				"proceeds unscoped")
		}
	})

	t.Run("leaves an unpartitioned entity alone", func(t *testing.T) {
		pool := &recordingPool{}
		s := &Server{pool: pool}
		tx, _ := pool.BeginTx(context.Background())

		if err := s.bindWrite(context.Background(), &entityMeta{partitioned: false}, tx); err != nil {
			t.Fatalf("bindWrite on an unpartitioned entity: %v", err)
		}
		if pool.sawBind() {
			t.Error("bound a tenant for an entity that declares no partition")
		}
	})
}

// buildEntityMeta must resolve `partition by` into the flag every call site
// branches on. If it never sets it, every scope and every bind turns into a
// no-op with all their call sites intact.
func TestBuildEntityMetaResolvesPartitioned(t *testing.T) {
	e, ir := cacheIR("tenant")
	if meta := buildEntityMeta(e, ir, nil, nil); !meta.partitioned {
		t.Error("an entity declaring `partition by` was not marked partitioned, " +
			"so scopedRead runs it on the bare pool and bindWrite does nothing")
	}
	e, ir = cacheIR("")
	if meta := buildEntityMeta(e, ir, nil, nil); meta.partitioned {
		t.Error("an entity with no `partition by` was marked partitioned")
	}
}

// Update must not resurrect a soft-deleted row.
//
// The soft-delete column is in the UPDATE's SET list, so a caller holding the
// primary key could clear it. That became reachable when the post-write
// read-back moved inside the transaction and dropped the soft-delete filter:
// before, the filtered read-back returned nothing and the transaction rolled
// back, so the row stayed deleted by accident rather than by rule. A review
// executed the resurrection end to end.
func TestUpdateSQLSkipsSoftDeletedRows(t *testing.T) {
	e := &dsl.Entity{
		Name: "Doc", Namespace: "sd",
		SoftDeleteField: "deleted_at",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
			{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}},
		},
	}
	got := buildUpdateSQL(e, nil)
	if !strings.Contains(got, `"deleted_at" IS NULL`) {
		t.Errorf("UPDATE does not exclude soft-deleted rows, so a caller holding "+
			"the primary key can clear the soft-delete column and undo a Delete:\n  %s", got)
	}

	// And an entity without soft delete must not grow a filter on a column it
	// does not have.
	plain := &dsl.Entity{
		Name: "Plain", Namespace: "sd",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}
	if strings.Contains(buildUpdateSQL(plain, nil), "IS NULL") {
		t.Errorf("added a soft-delete filter to an entity that declares none:\n  %s",
			buildUpdateSQL(plain, nil))
	}
}

// A reload must be refusable.
//
// The partition-policy check runs at boot, but the schema it exists to catch —
// `partition by` added to an entity that already exists, which emits no DDL —
// arrives at a RUNNING server through LISTEN/NOTIFY. A review delivered exactly
// that and read another tenant's rows while the server reported healthy.
//
// So Reload consults the hook BEFORE swapping the snapshot, and a refusal
// leaves the old schema serving rather than turning on enforcement the database
// is not providing.
func TestReloadCanBeRefused(t *testing.T) {
	f, err := dsl.Parse("pt.atl", []byte(partitionedSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	srv := NewServer(&recordingPool{}, noopCache{}, invalidate.NewOutbox(), nil, nil)

	if err := srv.Reload(ir, "first"); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got := srv.ContentHash(); got != "first" {
		t.Fatalf("content hash is %q after the initial load", got)
	}

	called := false
	srv.SetOnReload(func(*dsl.IR) error {
		called = true
		return errors.New("the new schema declares partition by on a table with no policy")
	})

	if err := srv.Reload(ir, "second"); err == nil {
		t.Error("a reload the hook refused was applied anyway")
	}
	if !called {
		t.Error("Reload never consulted the hook, so a schema that turns on " +
			"enforcement the database is not providing becomes live unchecked")
	}
	if got := srv.ContentHash(); got != "first" {
		t.Errorf("the refused snapshot became live: content hash is %q, want "+
			"%q. A refusal has to leave the old schema serving", got, "first")
	}

	// And a hook that approves must not block a legitimate reload.
	srv.SetOnReload(func(*dsl.IR) error { return nil })
	if err := srv.Reload(ir, "third"); err != nil {
		t.Fatalf("an approved reload was rejected: %v", err)
	}
	if got := srv.ContentHash(); got != "third" {
		t.Errorf("an approved reload did not become live: %q", got)
	}
}
