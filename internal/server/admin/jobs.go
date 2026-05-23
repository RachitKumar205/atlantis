package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SubmitJobRequest enqueues a new job onto atlantis.jobs. The shape
// is intentionally untyped on the wire (Args is raw JSON) so the
// generated typed client SDK can marshal a caller's args struct
// without round-tripping through gRPC reflection.
//
// JobName is the canonical "namespace.JobName" id matching what the
// caller declared in their `.atl` file. The server consults the IR
// checkpoint to look up the job's runtime config (retries / timeout /
// queue) so the caller doesn't have to thread those values from
// declaration to call site.
//
// Args MUST validate against the job's declared `args { ... }` shape;
// the server does a structural check (every required field present,
// each value's JSON-shape compatible with the declared type). Deep
// type checking — string-length bounds, CHECK predicates — runs at
// handler invocation time, not at submit time.
type SubmitJobRequest struct {
	JobName     string          `json:"JobName"`
	Args        json.RawMessage `json:"Args,omitempty"`
	ScheduledAt string          `json:"ScheduledAt,omitempty"` // RFC3339; "" = now
	SubmittedBy string          `json:"SubmittedBy,omitempty"`
}

// SubmitJobResponse returns the assigned job id. The id is a bigint
// formatted as a base-10 string for JSON safety (53-bit precision
// limit on int64 in browser JSON parsers).
type SubmitJobResponse struct {
	JobID string `json:"JobID"`
}

// GetJobStatusRequest reads one job row by id.
type GetJobStatusRequest struct {
	JobID string `json:"JobID"`
}

// JobStatus is the wire shape of one atlantis.jobs row. Timestamps
// render as RFC3339; nullable timestamps render empty when NULL so
// the JSON shape stays flat (no nested options).
type JobStatus struct {
	JobID        string          `json:"JobID"`
	JobName      string          `json:"JobName"`
	Queue        string          `json:"Queue"`
	Args         json.RawMessage `json:"Args"`
	Status       string          `json:"Status"`
	Attempts     int             `json:"Attempts"`
	MaxRetries   int             `json:"MaxRetries"`
	LastError    string          `json:"LastError,omitempty"`
	LastErrorAt  string          `json:"LastErrorAt,omitempty"`
	ScheduledFor string          `json:"ScheduledFor"`
	StartedAt    string          `json:"StartedAt,omitempty"`
	CompletedAt  string          `json:"CompletedAt,omitempty"`
	EnqueuedAt   string          `json:"EnqueuedAt"`
	SubmittedBy  string          `json:"SubmittedBy,omitempty"`
	// Progress carries the most recent Checkpoint write from the
	// handler. ProgressPct is -1 when the handler hasn't reported
	// (so the wire shape distinguishes "0% done" from "uninstrumented").
	ProgressPct int    `json:"ProgressPct,omitempty"`
	ProgressMsg string `json:"ProgressMsg,omitempty"`
	ProgressAt  string `json:"ProgressAt,omitempty"`
}

// GetJobStatusResponse wraps a JobStatus so the wire shape stays
// consistent across "found" vs "not found" — Found=false signals
// the latter rather than relying on a 404-style error code.
type GetJobStatusResponse struct {
	Found bool      `json:"Found"`
	Job   JobStatus `json:"Job,omitempty"`
}

// ListDeadJobsRequest paginates atlantis.jobs_dead. JobName ""
// returns rows across every job kind; Limit defaults to 50.
type ListDeadJobsRequest struct {
	JobName string `json:"JobName,omitempty"`
	Limit   int    `json:"Limit,omitempty"`
}

// ListDeadJobsResponse returns a page of DLQ rows ordered by
// moved_at DESC. The shape matches GetJobStatus so the CLI can
// render either with the same code path.
type ListDeadJobsResponse struct {
	Jobs []JobStatus `json:"Jobs"`
}

// RetryDeadJobRequest moves one DLQ row back to atlantis.jobs with
// attempts reset to 0. The DLQ row is deleted atomically with the
// re-insert; a failure that retries leaves no orphaned rows.
type RetryDeadJobRequest struct {
	JobID string `json:"JobID"`
}

// RetryDeadJobResponse signals success; the new row's id is the
// same as the DLQ row's id since moveToDLQ preserves it.
type RetryDeadJobResponse struct {
	JobID string `json:"JobID"`
}

// SubmitJob inserts a new job onto atlantis.jobs. The server reads
// the job's runtime config (retries / timeout / queue) from the IR
// checkpoint so callers submit only the args they actually vary
// per-call. A non-existent JobName is rejected — the caller
// presumably typo'd a name, or referenced a job that hasn't shipped
// to atlantis yet.
func (s *Service) SubmitJob(ctx context.Context, req SubmitJobRequest) (*SubmitJobResponse, error) {
	if !s.allowApplyMutation {
		return nil, errors.New("admin: job submission is disabled on this server (set ATL_ALLOW_APPLY_MUTATION=true to enable)")
	}
	if req.JobName == "" {
		return nil, errors.New("admin: JobName is required")
	}

	// Look up the job declaration on the IR checkpoint. The
	// checkpoint represents the schema atlantis-server was built
	// against, so a SubmitJob that references a not-yet-applied
	// job kind is rejected before the row ever reaches Postgres.
	ir, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	if ir == nil {
		return nil, errors.New("admin: no IR checkpoint applied — run `tide apply` before submitting jobs")
	}
	var spec *jobSpec
	for i := range ir.Jobs {
		if ir.Jobs[i].ID() == req.JobName {
			spec = &jobSpec{
				maxRetries:  ir.Jobs[i].Retries,
				timeoutMS:   ir.Jobs[i].TimeoutMS,
				timeoutNone: ir.Jobs[i].TimeoutNone,
				queue:       ir.Jobs[i].Queue,
			}
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("admin: unknown job %q (declare it in a .atl file and run `tide apply`)", req.JobName)
	}
	if spec.queue == "" {
		spec.queue = "default"
	}
	if spec.timeoutMS == 0 && !spec.timeoutNone {
		// DSL default when neither `timeout 30m` nor `timeout none` was
		// declared. Mirrors what the worker would assume.
		spec.timeoutMS = 30 * 60 * 1000
	}

	args := req.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// scheduled_for: the caller may defer execution. COALESCE on
	// the server side keeps the SQL constant — passing nil means
	// "fire immediately," any value means "wait until then."
	var scheduledForArg any
	if req.ScheduledAt != "" {
		scheduledForArg = req.ScheduledAt
	}

	// timeout_ms is NULLable: a job declared `timeout none` gets a
	// NULL row so the worker skips context.WithTimeout entirely.
	// SubmitJob passes nil → Postgres stores NULL → claim's
	// COALESCE(timeout_ms, 0) returns 0 → handleOne sees timeoutMS=0
	// → handler runs without a per-attempt cancel. Symmetric across
	// the three timeout shapes (none / declared / DSL default).
	var timeoutArg any
	if !spec.timeoutNone {
		timeoutArg = spec.timeoutMS
	}

	const insertSQL = `
INSERT INTO atlantis.jobs
    (job_name, queue, args, max_retries, timeout_ms, scheduled_for, submitted_by)
VALUES
    ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), $7)
RETURNING id`
	var id int64
	if err := s.pool.QueryRow(ctx, insertSQL,
		req.JobName,
		spec.queue,
		[]byte(args),
		spec.maxRetries,
		timeoutArg,
		scheduledForArg,
		req.SubmittedBy,
	).Scan(&id); err != nil {
		return nil, fmt.Errorf("insert job: %w", err)
	}
	return &SubmitJobResponse{JobID: fmt.Sprintf("%d", id)}, nil
}

// GetJobStatus reads one atlantis.jobs row. Returns Found=false (not
// an error) when the id doesn't exist; this lets the CLI render the
// "not found" case without distinguishing it from a transport error.
func (s *Service) GetJobStatus(ctx context.Context, req GetJobStatusRequest) (*GetJobStatusResponse, error) {
	if req.JobID == "" {
		return nil, errors.New("admin: JobID is required")
	}
	row := s.pool.QueryRow(ctx, `
SELECT id, job_name, queue, args, status, attempts, max_retries,
       COALESCE(last_error, ''), last_error_at, scheduled_for,
       started_at, completed_at, enqueued_at, COALESCE(submitted_by, ''),
       progress_pct, COALESCE(progress_msg, ''), progress_at
FROM atlantis.jobs WHERE id = $1`, req.JobID)
	var js JobStatus
	js, err := scanJobRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &GetJobStatusResponse{Found: false}, nil
		}
		return nil, err
	}
	return &GetJobStatusResponse{Found: true, Job: js}, nil
}

// ListDeadJobs paginates the DLQ. Filtering by JobName lets the
// operator narrow to one job kind for triage; default returns
// across all kinds.
func (s *Service) ListDeadJobs(ctx context.Context, req ListDeadJobsRequest) (*ListDeadJobsResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	q := `
SELECT id, job_name, queue, args, 'failed' AS status, attempts, max_retries,
       COALESCE(last_error, ''), last_error_at, moved_at AS scheduled_for,
       moved_at AS started_at, moved_at AS completed_at, enqueued_at, COALESCE(submitted_by, ''),
       NULL::smallint AS progress_pct, ''::text AS progress_msg, NULL::timestamptz AS progress_at
FROM atlantis.jobs_dead
WHERE ($1 = '' OR job_name = $1)
ORDER BY moved_at DESC
LIMIT $2`
	rs, err := s.pool.Query(ctx, q, req.JobName, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []JobStatus
	for rs.Next() {
		js, err := scanJobRow(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, js)
	}
	return &ListDeadJobsResponse{Jobs: out}, rs.Err()
}

// RetryDeadJob moves a DLQ row back into atlantis.jobs with attempts
// reset to 0, status reset to pending. The DLQ row is deleted in the
// same tx. The new row's id is the same as the DLQ row's so existing
// references (logs, alerts) don't lose correlation.
func (s *Service) RetryDeadJob(ctx context.Context, req RetryDeadJobRequest) (*RetryDeadJobResponse, error) {
	if !s.allowApplyMutation {
		return nil, errors.New("admin: job retry is disabled on this server")
	}
	if req.JobID == "" {
		return nil, errors.New("admin: JobID is required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	const moveSQL = `
INSERT INTO atlantis.jobs
    (id, job_name, queue, args, status, attempts, max_retries,
     scheduled_for, enqueued_at, submitted_by)
SELECT id, job_name, queue, args, 'pending', 0, max_retries,
       now(), enqueued_at, submitted_by
FROM atlantis.jobs_dead WHERE id = $1`
	res, err := tx.Exec(ctx, moveSQL, req.JobID)
	if err != nil {
		return nil, fmt.Errorf("re-insert: %w", err)
	}
	if res.RowsAffected() == 0 {
		return nil, fmt.Errorf("admin: dead job %s not found", req.JobID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM atlantis.jobs_dead WHERE id = $1`, req.JobID); err != nil {
		return nil, fmt.Errorf("delete dead: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &RetryDeadJobResponse{JobID: req.JobID}, nil
}

// jobSpec is the minimal slice of dsl.Job the SubmitJob path needs.
// Private — the caller never sees it.
type jobSpec struct {
	maxRetries  int
	timeoutMS   int
	timeoutNone bool
	queue       string
}

// rowScanner is the common surface between pgx.Row and pgx.Rows so
// scanJobRow works for both single-row and paginated queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJobRow lifts one atlantis.jobs row into the wire JobStatus
// shape, formatting timestamps to RFC3339 and collapsing NULL
// timestamps to empty strings.
func scanJobRow(r rowScanner) (JobStatus, error) {
	var (
		js               JobStatus
		id               int64
		args             []byte
		lastErrorAt      *time.Time
		startedAt        *time.Time
		completedAt      *time.Time
		scheduledFor     time.Time
		enqueuedAt       time.Time
		submittedByMaybe string
		lastErrorMaybe   string
		progressPct      *int16
		progressMsg      string
		progressAt       *time.Time
	)
	if err := r.Scan(&id, &js.JobName, &js.Queue, &args, &js.Status, &js.Attempts, &js.MaxRetries,
		&lastErrorMaybe, &lastErrorAt, &scheduledFor, &startedAt, &completedAt, &enqueuedAt, &submittedByMaybe,
		&progressPct, &progressMsg, &progressAt); err != nil {
		return js, err
	}
	js.JobID = fmt.Sprintf("%d", id)
	js.Args = args
	js.LastError = lastErrorMaybe
	js.LastErrorAt = formatNullable(lastErrorAt)
	js.ScheduledFor = scheduledFor.UTC().Format(time.RFC3339)
	js.StartedAt = formatNullable(startedAt)
	js.CompletedAt = formatNullable(completedAt)
	js.EnqueuedAt = enqueuedAt.UTC().Format(time.RFC3339)
	js.SubmittedBy = submittedByMaybe
	// ProgressPct: -1 sentinels "handler hasn't reported"; 0..100
	// otherwise. The wire shape uses int (with omitempty stripping
	// zero) so a never-reported job doesn't carry a spurious "0%".
	if progressPct != nil {
		js.ProgressPct = int(*progressPct)
	} else {
		js.ProgressPct = -1
	}
	js.ProgressMsg = progressMsg
	js.ProgressAt = formatNullable(progressAt)
	return js, nil
}

// formatNullable renders a *time.Time as RFC3339 in UTC, or empty
// string when the pointer is nil. Centralizing the format keeps the
// JSON shape consistent across every nullable timestamp column the
// admin RPCs expose.
func formatNullable(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
