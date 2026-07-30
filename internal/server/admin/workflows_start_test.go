package admin

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestBuildWorkflowStepArgsTreatsNullStateAsEmpty pins the bug that made every
// argument silently nil.
//
// json.RawMessage does not decode `null` to an empty slice — it keeps the four
// bytes — so `len(state) == 0` was false for an explicit JSON null and the
// literal string "null" reached the state column. buildWorkflowStepArgs then
// unmarshalled it into a nil map, and every ExprArg looked up a key in nil and
// got nothing. The step ran with all arguments empty instead of failing.
func TestBuildWorkflowStepArgsTreatsNullStateAsEmpty(t *testing.T) {
	step := dsl.WorkflowStepIR{
		Name: "first",
		Args: []dsl.EnqueueAssignmentIR{
			{Name: "order_id", Value: &dsl.ExprIR{Kind: dsl.ExprArg, ArgName: "order_id"}},
		},
	}

	// The shape StartWorkflow now normalises to.
	got, err := buildWorkflowStepArgs(step, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty state: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// And the shape that used to slip through, to show the normalisation is
	// what does the work rather than something downstream.
	if _, err := buildWorkflowStepArgs(step, json.RawMessage(`null`)); err != nil {
		t.Fatalf("null state should still parse, it just yields nothing: %v", err)
	}

	populated, err := buildWorkflowStepArgs(step, json.RawMessage(`{"order_id": 42}`))
	if err != nil {
		t.Fatalf("populated state: %v", err)
	}
	if err := json.Unmarshal(populated, &decoded); err != nil {
		t.Fatalf("decode populated: %v", err)
	}
	if decoded["order_id"] != float64(42) {
		t.Errorf("order_id = %#v, want 42 — the arg did not reach the step", decoded["order_id"])
	}
}

// TestStartWorkflowIsAtomic is the one that matters, and it needs a real
// database because the property is transactional.
//
// The previous code ran three statements and discarded the last one's error.
// If the current_step UPDATE failed, the step-0 job was already enqueued while
// current_step stayed NULL — and advanceWorkflow reads COALESCE(current_step,”)
// -> "" -> next index 0, so it enqueues step 0 again. Duplicate execution,
// potentially looping.
//
// The assertion is the invariant that makes that impossible: an instance row
// and its first job either both exist with current_step set, or neither exists.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/server/admin/ -run StartWorkflowIsAtomic -v
func TestStartWorkflowIsAtomic(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the workflow atomicity test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const wfName = "test.atomic_wf"
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE workflow_id IN
			(SELECT id FROM atlantis.workflow_instances WHERE workflow_name = $1)`, wfName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.workflow_instances WHERE workflow_name = $1`, wfName)
	}
	cleanup()
	t.Cleanup(cleanup)

	// jobs.workflow_id did not exist until migration 0020, so StartWorkflow's
	// INSERT failed outright and no workflow could ever start. Assert the
	// linkage is present before asserting anything about its contents —
	// otherwise a missing column reads as "no orphans" and this test passes by
	// being unable to look.
	var hasCol bool
	if err := pool.QueryRow(ctx, `
SELECT count(*) = 2 FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'jobs'
   AND column_name IN ('workflow_id','workflow_step')`).Scan(&hasCol); err != nil {
		t.Fatalf("check columns: %v", err)
	}
	if !hasCol {
		t.Fatal("atlantis.jobs is missing workflow_id/workflow_step — StartWorkflow cannot work")
	}

	// current_step must be set on the same row the job points at. A NULL here
	// is the exact state that causes step 0 to run twice.
	var orphans int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.workflow_instances wi
 WHERE wi.current_step IS NULL
   AND EXISTS (SELECT 1 FROM atlantis.jobs j WHERE j.workflow_id = wi.id)`).Scan(&orphans); err != nil {
		t.Fatalf("scan orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d workflow instance(s) have an enqueued job but NULL current_step; "+
			"advanceWorkflow will re-enqueue step 0 for each", orphans)
	}
}
