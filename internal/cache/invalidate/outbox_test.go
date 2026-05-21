package invalidate

import (
	"context"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// fakeTx implements runtime.Tx for unit-level shape tests.
type fakeTx struct {
	calls []fakeCall
	err   error
}

type fakeCall struct {
	sql  string
	args []any
}

func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return nil
}
func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	return nil, nil
}
func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	f.calls = append(f.calls, fakeCall{sql: sql, args: args})
	return nil, f.err
}
func (f *fakeTx) Commit(ctx context.Context) error   { return nil }
func (f *fakeTx) Rollback(ctx context.Context) error { return nil }

func TestOutbox_EnqueueShape(t *testing.T) {
	tx := &fakeTx{}
	ob := NewOutbox()
	if err := ob.Enqueue(context.Background(), tx, "x.A", "42", 7); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(tx.calls) != 1 {
		t.Fatalf("want 1 Exec call, got %d", len(tx.calls))
	}
	c := tx.calls[0]
	if !strings.Contains(c.sql, "atlantis.cache_invalidations") {
		t.Errorf("sql doesn't reference outbox table:\n%s", c.sql)
	}
	if !strings.Contains(c.sql, "INSERT INTO") {
		t.Errorf("sql isn't an INSERT:\n%s", c.sql)
	}
	if len(c.args) != 3 || c.args[0] != "x.A" || c.args[1] != "42" || c.args[2] != int64(7) {
		t.Errorf("args: %v", c.args)
	}
}

func TestOutbox_RejectsEmptyArgs(t *testing.T) {
	ob := NewOutbox()
	tx := &fakeTx{}
	if err := ob.Enqueue(context.Background(), tx, "", "1", 1); err == nil {
		t.Errorf("expected error on empty entity")
	}
	if err := ob.Enqueue(context.Background(), tx, "x.A", "", 1); err == nil {
		t.Errorf("expected error on empty id")
	}
}

func TestOutbox_CustomSchema(t *testing.T) {
	tx := &fakeTx{}
	ob := &Outbox{Schema: "alt_schema"}
	if err := ob.Enqueue(context.Background(), tx, "x.A", "1", 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !strings.Contains(tx.calls[0].sql, "alt_schema.cache_invalidations") {
		t.Errorf("custom schema not honored: %s", tx.calls[0].sql)
	}
}

func TestOutbox_SatisfiesRuntimeInterface(t *testing.T) {
	var _ runtime.Outbox = (*Outbox)(nil)
}

func TestOutbox_EnqueueWritesInvalidationKind(t *testing.T) {
	tx := &fakeTx{}
	ob := NewOutbox()
	_ = ob.Enqueue(context.Background(), tx, "x.A", "1", 1)
	if !strings.Contains(tx.calls[0].sql, "'invalidation'") {
		t.Errorf("expected kind = 'invalidation' in SQL:\n%s", tx.calls[0].sql)
	}
}

func TestOutbox_EnqueueGenerationBumpShape(t *testing.T) {
	tx := &fakeTx{}
	ob := NewOutbox()
	if err := ob.EnqueueGenerationBump(context.Background(), tx, "x.A"); err != nil {
		t.Fatalf("EnqueueGenerationBump: %v", err)
	}
	if len(tx.calls) != 1 {
		t.Fatalf("want 1 Exec call, got %d", len(tx.calls))
	}
	c := tx.calls[0]
	if !strings.Contains(c.sql, "'generation_bump'") {
		t.Errorf("expected kind = 'generation_bump' in SQL:\n%s", c.sql)
	}
	if len(c.args) != 1 || c.args[0] != "x.A" {
		t.Errorf("args should be just [entity]; got %v", c.args)
	}
}

func TestOutbox_EnqueueGenerationBumpRejectsEmpty(t *testing.T) {
	ob := NewOutbox()
	if err := ob.EnqueueGenerationBump(context.Background(), &fakeTx{}, ""); err == nil {
		t.Errorf("expected error on empty entity")
	}
}

func TestDefaultWorkerConfig_Shape(t *testing.T) {
	c := DefaultWorkerConfig()
	if c.Schema != "atlantis" {
		t.Errorf("schema: %s", c.Schema)
	}
	if c.DrainInterval <= 0 || c.BatchSize <= 0 {
		t.Errorf("zero-valued config: %+v", c)
	}
	if c.PointerTTL <= 0 {
		t.Errorf("PointerTTL should default to a positive duration, got %v", c.PointerTTL)
	}
	if c.AlertLag <= 0 {
		t.Errorf("AlertLag should default to a positive duration, got %v", c.AlertLag)
	}
}
