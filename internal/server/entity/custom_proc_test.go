package entity

import (
	"context"
	"testing"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// --- test doubles for the runtime tx/pool/outbox interfaces ----------

type fakeTag struct{ n int64 }

func (t fakeTag) RowsAffected() int64 { return t.n }

type fakeTx struct {
	execs     []string // SQL of each Exec, in order
	execArgs  [][]any
	rows      []int64 // RowsAffected to return per Exec, in order
	committed bool
	rolled    bool
	failOn    int // 1-based step index to fail on; 0 = never
}

func (tx *fakeTx) QueryRow(context.Context, string, ...any) runtime.Row { return nil }
func (tx *fakeTx) Query(context.Context, string, ...any) (runtime.Rows, error) {
	return nil, nil
}
func (tx *fakeTx) Exec(_ context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	idx := len(tx.execs)
	tx.execs = append(tx.execs, sql)
	tx.execArgs = append(tx.execArgs, args)
	if tx.failOn == idx+1 {
		return nil, context.DeadlineExceeded
	}
	n := int64(0)
	if idx < len(tx.rows) {
		n = tx.rows[idx]
	}
	return fakeTag{n: n}, nil
}
func (tx *fakeTx) Commit(context.Context) error { tx.committed = true; return nil }
func (tx *fakeTx) Rollback(context.Context) error {
	// Mirror pgx: Rollback after a successful Commit is a harmless no-op
	// (the executor always defers Rollback). Only a rollback of an
	// uncommitted tx counts as a real rollback.
	if !tx.committed {
		tx.rolled = true
	}
	return nil
}

type fakePool struct{ tx *fakeTx }

func (p *fakePool) QueryRow(context.Context, string, ...any) runtime.Row { return nil }
func (p *fakePool) Query(context.Context, string, ...any) (runtime.Rows, error) {
	return nil, nil
}
func (p *fakePool) Exec(context.Context, string, ...any) (runtime.CommandTag, error) {
	return fakeTag{}, nil
}
func (p *fakePool) BeginTx(context.Context) (runtime.Tx, error) { return p.tx, nil }

type fakeOutbox struct{ bumped []string }

func (o *fakeOutbox) Enqueue(context.Context, runtime.Tx, string, string, int64) error {
	return nil
}
func (o *fakeOutbox) EnqueueGenerationBump(_ context.Context, _ runtime.Tx, entity string) error {
	o.bumped = append(o.bumped, entity)
	return nil
}

// procServer builds a Server with the given snapshot wired to fake
// pool/outbox so executeCustomProcedureWithReq can run without PG.
func procServer(t *testing.T, snap *entitySnapshot, tx *fakeTx, ob *fakeOutbox) *Server {
	t.Helper()
	s := &Server{pool: &fakePool{tx: tx}, outbox: ob}
	s.snapshot.Store(snap)
	return s
}

// rawProcIR builds an IR with one raw-SQL procedure owned by `vendor`.
func rawProcIR(name string, inputs []dsl.QueryParam, steps ...dsl.ProcedureStepIR) *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Procedures: []dsl.CustomProcedure{{
			Name:   name,
			Owner:  "vendor.ShopifySyncJob",
			Inputs: inputs,
			Steps:  steps,
		}},
	}
}

func rawStep(sql string, touches ...string) dsl.ProcedureStepIR {
	return dsl.ProcedureStepIR{Raw: &dsl.RawSQLIR{SQL: sql, Touches: touches}}
}

// --- tests ----------------------------------------------------------

func TestBuildCustomProcedureDescs_ResponseIsRowsAffected(t *testing.T) {
	cp := &dsl.CustomProcedure{
		Name:   "FailStale",
		Owner:  "vendor.ShopifySyncJob",
		Inputs: []dsl.QueryParam{{Name: "stale_minutes", Type: dsl.FieldType{Name: "int"}}},
	}
	fd, err := buildCustomProcedureDescs(cp, "vendor")
	if err != nil {
		t.Fatalf("buildCustomProcedureDescs: %v", err)
	}
	req := fd.Messages().ByName("FailStaleRequest")
	if req == nil {
		t.Fatal("missing FailStaleRequest")
	}
	if f := req.Fields().ByName("stale_minutes"); f == nil {
		t.Error("request missing stale_minutes field")
	} else if f.Number() != 1 || f.Kind() != protoreflect.Int32Kind {
		t.Errorf("stale_minutes = #%d %s, want #1 int32", f.Number(), f.Kind())
	}
	resp := fd.Messages().ByName("FailStaleResponse")
	if resp == nil {
		t.Fatal("missing FailStaleResponse")
	}
	ra := resp.Fields().ByName("rows_affected")
	if ra == nil || ra.Number() != 1 || ra.Kind() != protoreflect.Int64Kind {
		t.Errorf("rows_affected field wrong: %+v", ra)
	}
	if resp.Fields().Len() != 1 {
		t.Errorf("response should have exactly 1 field, got %d", resp.Fields().Len())
	}
}

func TestBuildSnapshot_PopulatesProcMeta(t *testing.T) {
	ir := rawProcIR("DeleteStaging",
		[]dsl.QueryParam{{Name: "sync_job_id", Type: dsl.FieldType{Name: "bigint"}}},
		rawStep("DELETE FROM vendor.staging WHERE sync_job_id = $sync_job_id", "vendor.ShopifyStagingProduct"),
	)
	snap, err := buildSnapshot(ir, "h")
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	pm, ok := snap.procMeta["vendor:DeleteStaging"]
	if !ok {
		t.Fatal("procMeta missing vendor:DeleteStaging")
	}
	if len(pm.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(pm.steps))
	}
	if pm.steps[0].sql != "DELETE FROM vendor.staging WHERE sync_job_id = $1" {
		t.Errorf("normalized SQL = %q", pm.steps[0].sql)
	}
	if len(pm.steps[0].argOrder) != 1 || pm.steps[0].argOrder[0] != "sync_job_id" {
		t.Errorf("argOrder = %v", pm.steps[0].argOrder)
	}
	if len(pm.touched) != 1 || pm.touched[0] != "vendor.ShopifyStagingProduct" {
		t.Errorf("touched = %v", pm.touched)
	}
}

func TestExecuteCustomProcedure_TxFlowAndRowsAffected(t *testing.T) {
	ir := rawProcIR("Cleanup",
		[]dsl.QueryParam{{Name: "sync_job_id", Type: dsl.FieldType{Name: "bigint"}}},
		rawStep("DELETE FROM a WHERE sync_job_id = $sync_job_id", "vendor.A"),
		rawStep("DELETE FROM b WHERE sync_job_id = $sync_job_id", "vendor.B"),
	)
	snap, err := buildSnapshot(ir, "h")
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	pm := snap.procMeta["vendor:Cleanup"]
	tx := &fakeTx{rows: []int64{3, 5}}
	ob := &fakeOutbox{}
	s := procServer(t, snap, tx, ob)

	req := dynamicpb.NewMessage(pm.requestDesc)
	req.Set(pm.requestDesc.Fields().ByName("sync_job_id"), protoreflect.ValueOfInt64(42))

	resp, err := s.executeCustomProcedureWithReq(context.Background(), pm, req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Two Exec calls, each bound with the sync_job_id arg.
	if len(tx.execs) != 2 {
		t.Fatalf("exec count = %d, want 2", len(tx.execs))
	}
	for i, a := range tx.execArgs {
		if len(a) != 1 || a[0] != int64(42) {
			t.Errorf("step %d args = %v, want [42]", i, a)
		}
	}
	// rows_affected = 3 + 5.
	msg := resp.(*dynamicpb.Message)
	got := msg.Get(pm.responseDesc.Fields().ByName("rows_affected")).Int()
	if got != 8 {
		t.Errorf("rows_affected = %d, want 8", got)
	}
	// Both touched entities bumped, then commit (not rollback).
	if len(ob.bumped) != 2 || ob.bumped[0] != "vendor.A" || ob.bumped[1] != "vendor.B" {
		t.Errorf("bumped = %v, want [vendor.A vendor.B]", ob.bumped)
	}
	if !tx.committed || tx.rolled {
		t.Errorf("tx state: committed=%v rolled=%v, want committed", tx.committed, tx.rolled)
	}
}

func TestExecuteCustomProcedure_StepErrorRollsBack(t *testing.T) {
	ir := rawProcIR("Cleanup",
		[]dsl.QueryParam{{Name: "sync_job_id", Type: dsl.FieldType{Name: "bigint"}}},
		rawStep("DELETE FROM a WHERE sync_job_id = $sync_job_id", "vendor.A"),
		rawStep("DELETE FROM b WHERE sync_job_id = $sync_job_id", "vendor.B"),
	)
	snap, _ := buildSnapshot(ir, "h")
	pm := snap.procMeta["vendor:Cleanup"]
	tx := &fakeTx{rows: []int64{3, 5}, failOn: 2}
	ob := &fakeOutbox{}
	s := procServer(t, snap, tx, ob)

	req := dynamicpb.NewMessage(pm.requestDesc)
	req.Set(pm.requestDesc.Fields().ByName("sync_job_id"), protoreflect.ValueOfInt64(7))

	if _, err := s.executeCustomProcedureWithReq(context.Background(), pm, req); err == nil {
		t.Fatal("expected error on step 2")
	}
	if tx.committed {
		t.Error("tx must not commit after a step error")
	}
	if !tx.rolled {
		t.Error("tx must roll back after a step error")
	}
	if len(ob.bumped) != 0 {
		t.Errorf("no invalidation should fire on failure, got %v", ob.bumped)
	}
}

func TestExecuteCustomProcedure_UnsupportedStepUnimplemented(t *testing.T) {
	ir := &dsl.IR{
		Version: 1,
		Procedures: []dsl.CustomProcedure{{
			Name:   "Enq",
			Owner:  "vendor.ShopifySyncJob",
			Inputs: []dsl.QueryParam{{Name: "x", Type: dsl.FieldType{Name: "bigint"}}},
			Steps:  []dsl.ProcedureStepIR{{Enqueue: &dsl.EnqueueStepIR{TargetJobID: "vendor.J"}}},
		}},
	}
	snap, err := buildSnapshot(ir, "h")
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	pm := snap.procMeta["vendor:Enq"]
	if pm.unsupported == "" {
		t.Fatal("enqueue step should mark procedure unsupported")
	}
	tx := &fakeTx{}
	s := procServer(t, snap, tx, &fakeOutbox{})
	req := dynamicpb.NewMessage(pm.requestDesc)

	_, err = s.executeCustomProcedureWithReq(context.Background(), pm, req)
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("want Unimplemented, got %v", err)
	}
	if len(tx.execs) != 0 {
		t.Error("no SQL should run for an unsupported procedure")
	}
}
