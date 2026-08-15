package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// StartWorkflowRequest kicks off a workflow instance. The server
// validates that WorkflowName exists in the IR checkpoint, inserts
// an atlantis.workflow_instances row, and enqueues the first step's
// job. The workflow engine (internal/jobs/workflow.go) advances
// subsequent steps as jobs complete.
type StartWorkflowRequest struct {
	WorkflowName string          `json:"WorkflowName"`
	State        json.RawMessage `json:"State,omitempty"`
	SubmittedBy  string          `json:"SubmittedBy,omitempty"`
}

type StartWorkflowResponse struct {
	WorkflowID string `json:"WorkflowID"`
}

type WorkflowStatus struct {
	WorkflowID   string `json:"WorkflowID"`
	WorkflowName string `json:"WorkflowName"`
	Status       string `json:"Status"`
	CurrentStep  string `json:"CurrentStep,omitempty"`
	StartedAt    string `json:"StartedAt"`
	CompletedAt  string `json:"CompletedAt,omitempty"`
	ErrorMsg     string `json:"ErrorMsg,omitempty"`
	SubmittedBy  string `json:"SubmittedBy,omitempty"`
}

type GetWorkflowStatusResponse struct {
	Found    bool           `json:"Found"`
	Workflow WorkflowStatus `json:"Workflow,omitempty"`
}

// StartWorkflow creates a workflow instance and enqueues the first
// step's job. Returns the instance id for monitoring.
func (s *Service) StartWorkflow(ctx context.Context, req *adminpb.StartWorkflowRequest) (*adminpb.StartWorkflowResponse, error) {
	if !s.allowApplyMutation {
		return nil, errors.New("admin: workflow submission disabled")
	}
	if req.GetWorkflowName() == "" {
		return nil, errors.New("admin: WorkflowName is required")
	}

	ir, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	if ir == nil {
		return nil, errors.New("admin: no IR checkpoint applied")
	}

	var target *dsl.Workflow
	for i := range ir.Workflows {
		if ir.Workflows[i].ID() == req.GetWorkflowName() {
			target = &ir.Workflows[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("admin: unknown workflow %q", req.GetWorkflowName())
	}
	// A stepless workflow used to be accepted: the instance row was written and
	// the enqueue loop skipped it, leaving a row that could never advance and a
	// caller holding an id that would never complete. Refusing is the honest
	// answer — there is nothing to start.
	if len(target.Steps) == 0 {
		return nil, fmt.Errorf("admin: workflow %q declares no steps", req.GetWorkflowName())
	}

	// Absent and explicitly-null state both mean "no state". json.RawMessage
	// does not decode `null` to an empty slice — it keeps the four bytes — so
	// without the second test the literal string "null" reaches the state
	// column, and buildWorkflowStepArgs then unmarshals it to a nil map and
	// yields nil for every ExprArg. The step runs with every argument silently
	// empty rather than failing.
	state := json.RawMessage(req.GetState())
	if len(state) == 0 || string(state) == "null" {
		state = json.RawMessage("{}")
	}

	step := target.Steps[0]
	argsJSON, err := buildWorkflowStepArgs(step, state)
	if err != nil {
		return nil, fmt.Errorf("build first step args: %w", err)
	}

	// Instance row, first job, and current_step in one transaction.
	//
	// These were three separate statements, and the last one discarded its
	// error. The failure that produces is specific and bad: the job for step 0
	// is enqueued while current_step stays NULL, so when it completes
	// advanceWorkflow reads COALESCE(current_step,'') -> "" -> next index 0
	// (jobs/workflow.go:110-112) and enqueues step 0 again — duplicate
	// execution, potentially looping.
	//
	// Checking the error instead of ignoring it would not have been enough:
	// the job is already visible to the dispatcher by then, so there is a
	// window no error handling can close. Atomicity is the fix; the error check
	// is a consequence of it.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin start-workflow tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var id int64
	if err := tx.QueryRow(ctx, `
INSERT INTO atlantis.workflow_instances (workflow_name, state, submitted_by, current_step)
VALUES ($1, $2, $3, $4)
RETURNING id`, req.GetWorkflowName(), []byte(state), req.GetSubmittedBy(), step.Name).Scan(&id); err != nil {
		return nil, fmt.Errorf("insert workflow: %w", err)
	}

	// Enqueued inline because there is no post-insert hook to rely on: this
	// package cannot import internal/jobs without an import cycle, so the
	// engine never learns about the instance until a job for it exists.
	//
	// owner_caller is stamped here for the same reason SubmitJob stamps it:
	// this row lands in atlantis.jobs and is read back through GetJobStatus
	// and the dead-letter queue, both of which scope by owner. Left unset it
	// would default to '', so a caller could start a workflow and then not be
	// able to see the job it created — visible to operators and to nobody
	// else. The submitted_by value beside it stays "workflow:<name>", which
	// is provenance for a human and not an identity.
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, timeout_ms, submitted_by, workflow_id, workflow_step, owner_caller)
VALUES ($1, 'default', $2, 3, 1800000, $3, $4, $5, $6)`,
		step.TargetJobID, argsJSON, "workflow:"+req.GetWorkflowName(), id, step.Name,
		s.jobOwner(ctx)); err != nil {
		return nil, fmt.Errorf("enqueue first step: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit start-workflow: %w", err)
	}
	return &adminpb.StartWorkflowResponse{WorkflowId: fmt.Sprintf("%d", id)}, nil
}

func buildWorkflowStepArgs(step dsl.WorkflowStepIR, stateJSON json.RawMessage) ([]byte, error) {
	var state map[string]any
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(step.Args))
	for _, a := range step.Args {
		if a.Value == nil {
			continue
		}
		switch a.Value.Kind {
		case dsl.ExprArg:
			out[a.Name] = state[a.Value.ArgName]
		case dsl.ExprLiteralStr:
			out[a.Name] = a.Value.LitStr
		case dsl.ExprLiteralInt:
			out[a.Name] = a.Value.LitInt
		case dsl.ExprLiteralBool:
			out[a.Name] = a.Value.LitBool
		}
	}
	return json.Marshal(out)
}

// GetWorkflowStatus reads one workflow instance.
func (s *Service) GetWorkflowStatus(ctx context.Context, req *adminpb.GetWorkflowStatusRequest) (*adminpb.GetWorkflowStatusResponse, error) {
	if req.GetWorkflowId() == "" {
		return nil, errors.New("admin: WorkflowID is required")
	}
	// The column is bigint, so binding a non-numeric string surfaces
	// "invalid input syntax for type bigint" — a driver message about our
	// schema, for what is simply a workflow that does not exist. Parse first
	// and answer the question that was asked.
	wfID, perr := strconv.ParseInt(req.GetWorkflowId(), 10, 64)
	if perr != nil {
		return &adminpb.GetWorkflowStatusResponse{Found: false}, nil
	}
	var (
		ws          WorkflowStatus
		id          int64
		startedAt   time.Time
		completedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, workflow_name, status, COALESCE(current_step, ''), started_at, completed_at,
       COALESCE(error_msg, ''), COALESCE(submitted_by, '')
FROM atlantis.workflow_instances WHERE id = $1`, wfID).Scan(
		&id, &ws.WorkflowName, &ws.Status, &ws.CurrentStep,
		&startedAt, &completedAt, &ws.ErrorMsg, &ws.SubmittedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &adminpb.GetWorkflowStatusResponse{Found: false}, nil
		}
		return nil, err
	}
	ws.WorkflowID = fmt.Sprintf("%d", id)
	ws.StartedAt = startedAt.UTC().Format(time.RFC3339)
	ws.CompletedAt = formatNullable(completedAt)
	return &adminpb.GetWorkflowStatusResponse{Found: true, Workflow: workflowStatusToPB(ws)}, nil
}

// --- Wire conversion ---
//
// The proto models Workflow as a nil message when the instance is not found.
// What a consumer sees for that nil is decided at the JSON boundary by
// clients/go/adminjson, not here.

func workflowStatusToPB(w WorkflowStatus) *adminpb.WorkflowStatus {
	return &adminpb.WorkflowStatus{
		WorkflowId:   w.WorkflowID,
		WorkflowName: w.WorkflowName,
		Status:       w.Status,
		CurrentStep:  w.CurrentStep,
		StartedAt:    w.StartedAt,
		CompletedAt:  w.CompletedAt,
		ErrorMsg:     w.ErrorMsg,
		SubmittedBy:  w.SubmittedBy,
	}
}
