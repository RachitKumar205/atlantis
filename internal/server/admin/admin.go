// Package admin implements the atlantis control-plane RPCs: schema
// lifecycle (plan, apply, adopt, rollback, history, lineage), caller
// identity management (register, revoke, cert-expiry tracking),
// declarative jobs and workflows (submit, status, dead/retry), and
// operational telemetry (entity owners, in-process log ring).
//
// Every RPC is served twice while clients migrate: over protobuf at
// atlantis.admin.v1.AdminService (grpcgen.go) and over the older JSON envelope
// codec at atlantis.admin.v1.Admin (grpc.go). Both paths run the same method
// bodies and both are governed by the capability interceptor.
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlvalidate"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/obs"
)

// Service is safe for concurrent use; one instance per process.
//
// Mutation gating, in the order a request meets it:
//
//  1. The capability interceptor (internal/server/authz) checks the caller's
//     grants against the requirement the method declares in admin.proto. This
//     is the authorization decision, and it is the only one; it happens before
//     any method body runs, so no handler can forget it.
//  2. `allowApplyMutation` — the deployment-wide off switch for the mutating
//     plane. Not per-caller and not authorization: an operator turning the
//     whole plane off during an incident.
//  3. `bindCallerIdentity` — `req.Caller` must equal the authenticated cert CN
//     on apply and backfill. This one has to live in the handler because an
//     interceptor cannot see request bodies, and without it CAPABILITY_SCHEMA_
//     APPLY would mean "may write to anyone's schema" rather than "may write to
//     mine".
//
// The per-CN allowlists that used to sit between (1) and (2) are gone. They
// expressed authorization as process configuration, which meant a grant could
// not be audited, could not be changed without a restart, and — because the
// operator list fell back to a global wildcard when unset, the shipped default
// — silently made every mutation-capable caller an operator.
type Service struct {
	pool               *pgxpool.Pool
	mirrorDir          string
	mirrorEnabled      bool
	allowApplyMutation bool
	callerFromContext  func(context.Context) string
	backfillEnabled    bool
	logRing            *obs.LogRing

	// proxyForwarded reports whether the request's identity was asserted by
	// a trusted front proxy (vs a direct mTLS client). nil when trusted-
	// proxy mode is off. The two trustedProxyMay* flags decide which gates
	// a forwarded identity may satisfy.
	proxyForwarded         func(context.Context) bool
	trustedProxyMayApply   bool
	trustedProxyMayOperate bool

	// dispatcher is the worker-poll dispatcher (set by SetDispatcher).
	// Nil when ATL_JOBS_DISPATCHER_ENABLED=false; the four worker-
	// admin RPCs return "not found" in that case.
	dispatcher WorkerDispatcher
}

// Config holds optional toggles; the zero value is read-only with no mirror.
type Config struct {
	// MirrorDir is the root path for mirrored files. Ignored when MirrorEnabled is false.
	MirrorDir string

	// MirrorEnabled, when true, mirrors applied files to MirrorDir.
	MirrorEnabled bool

	// AllowApplyMutation is the deployment-wide switch for the mutating
	// admin plane: job submission, retry, and workflow start refuse when it
	// is false. It is not authorization — who may mutate is a per-caller
	// capability read from atlantis.caller_capabilities and enforced by the
	// interceptor in internal/server/authz. This is the operator's off
	// switch for the whole plane, which a per-caller grant cannot express
	// and which stays useful during an incident.
	AllowApplyMutation bool

	// CallerFromContext extracts the authenticated cert CN from the
	// request context. The admin service uses it to enforce that
	// req.GetCaller() matches the connecting CN on apply/backfill so a
	// caller can't impersonate another caller's identity in the
	// request body. When nil the check is skipped (insecure dev mode).
	CallerFromContext func(context.Context) string

	// ProxyForwardedFromContext reports whether the request's identity came
	// from a trusted front proxy (a forwarded, re-validated client cert)
	// rather than a direct mTLS connection. nil disables the trusted-proxy
	// admin-plane gating (i.e. the mode is off). When it reports true, a
	// forwarded identity may satisfy self-apply only if TrustedProxyMayApply
	// and operator gates only if TrustedProxyMayOperate.
	ProxyForwardedFromContext func(context.Context) bool

	// TrustedProxyMayApply lets a proxy-forwarded identity satisfy the
	// self-apply gate (its own `tide plan`/`apply`). Default-on at the
	// config layer.
	TrustedProxyMayApply bool

	// TrustedProxyMayOperate lets a proxy-forwarded identity satisfy the
	// operator gate (cross-caller register/revoke/adopt/rollback/aliases).
	// Default-off: these manage the trust system, so they stay on direct
	// mTLS unless explicitly opted in.
	TrustedProxyMayOperate bool

	// BackfillEnabled gates the BeginBackfillPlan RPC. Default false so
	// a server running without the backfill worker can't accept plans
	// that would pile up unprocessed. Operator sets this to true after
	// canarying the feature.
	BackfillEnabled bool

	// LogRing is the in-process slog ring buffer the GetLogs RPC reads
	// from. nil disables the RPC (it returns an empty page). The ring
	// itself is populated by the slog handler installed in
	// cmd/server/main.go's buildLogger — see internal/obs/logring.go.
	LogRing *obs.LogRing
}

// New returns a Service backed by pool.
func New(pool *pgxpool.Pool, cfg Config) *Service {
	return &Service{
		pool:               pool,
		mirrorDir:          cfg.MirrorDir,
		mirrorEnabled:      cfg.MirrorEnabled,
		allowApplyMutation: cfg.AllowApplyMutation,
		callerFromContext:  cfg.CallerFromContext,
		backfillEnabled:    cfg.BackfillEnabled,
		logRing:            cfg.LogRing,

		proxyForwarded:         cfg.ProxyForwardedFromContext,
		trustedProxyMayApply:   cfg.TrustedProxyMayApply,
		trustedProxyMayOperate: cfg.TrustedProxyMayOperate,
	}
}

// forwardedDenied returns a non-nil error when the request's identity was
// asserted by a trusted front proxy but proxy-forwarded identities aren't
// permitted to satisfy this gate. The edge can carry a caller's own data and
// self-apply plane, but cross-caller operator authority stays on direct mTLS
// unless explicitly opted in.
func (s *Service) forwardedDenied(ctx context.Context, allowed bool, plane, envVar string) error {
	if allowed || s.proxyForwarded == nil || !s.proxyForwarded(ctx) {
		return nil
	}
	return fmt.Errorf("admin: %s is not permitted over a trusted front proxy; use a direct mTLS connection (or set %s=true)", plane, envVar)
}

// guardOperatorTransport rejects an operator RPC that arrived over a trusted
// front proxy, unless the deployment opted in.
//
// The capability half of what this function used to do — "is this CN an
// operator" — now lives in internal/server/authz, enforced by an interceptor
// against the requirement each method declares in the proto. What remains is
// orthogonal: a restriction on how the request arrived, not on who sent it. An
// edge-terminated connection cannot be held to the same standard as direct
// mTLS, and that is true regardless of the caller's grants.
func (s *Service) guardOperatorTransport(ctx context.Context) error {
	return s.forwardedDenied(ctx, s.trustedProxyMayOperate, "operator mutation", "ATL_TRUSTED_PROXY_MAY_OPERATE")
}

// requireMutablePlane enforces the deployment-wide switch.
//
// This is not authorization and does not overlap with the capability check: it
// is one operator decision about whether this atlantis accepts schema change
// over the wire at all. ATL_ALLOW_APPLY_MUTATION=false is the regulated posture
// documented in docs/architecture/schema-flow.md — SQL is reviewed on a
// deployment-repo PR and applied by tidectl, and no grant, however broad, may
// route around that. It is also the switch to reach for during an incident,
// which a per-caller grant cannot express because it would have to be revoked
// caller by caller and restored the same way.
//
// It lives in the handler rather than the interceptor because it is not a
// property of the caller or the method — it is one bit of server state, and
// three RPCs in jobs.go and workflows.go already read it directly.
func (s *Service) requireMutablePlane(what string) error {
	if s.allowApplyMutation {
		return nil
	}
	return fmt.Errorf("admin: %s is disabled on this server (ATL_ALLOW_APPLY_MUTATION=false); "+
		"schema changes route through tidectl plan + approve against the deployment repo", what)
}

// bindCallerIdentity ties a mutating request to the connection that carried it:
// req.Caller must name the authenticated cert CN, so a caller permitted to
// mutate can only mutate its own namespace.
//
// This deliberately did NOT move to the interceptor along with the capability
// check. An interceptor sees the method name and the connection; it does not
// see request bodies. Deleting this in the name of "no handler does its own
// authz" would leave SCHEMA_APPLY meaning "may write to any caller's schema"
// rather than "may write to mine" — a privilege escalation dressed as cleanup.
//
// In insecure dev mode there is no cert identity to bind to, so the check is
// skipped. Nothing else covers it there either — cmd/server does not install
// the capability interceptor without TLS, for the same reason: a self-asserted
// header is not an identity. The admin plane is unprotected in that
// configuration by design, and the server says so at boot.
func (s *Service) bindCallerIdentity(ctx context.Context, reqCaller string) error {
	if err := s.forwardedDenied(ctx, s.trustedProxyMayApply, "schema apply", "ATL_TRUSTED_PROXY_MAY_APPLY"); err != nil {
		return err
	}
	var cn string
	if s.callerFromContext != nil {
		cn = s.callerFromContext(ctx)
	}
	if cn != "" && cn != "anonymous" && reqCaller != cn {
		return fmt.Errorf("admin: req.caller %q does not match authenticated identity %q", reqCaller, cn)
	}
	return nil
}

// SubmittedFile is one .atl file submitted by a caller; Path is repo-relative.
type SubmittedFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// PlanRequest is the input to PlanSchema.
type PlanRequest struct {
	Caller string
	Files  []SubmittedFile
}

// PlanResponse is the result of PlanSchema. PlanID is stable across calls
// with the same (caller, file-set, base-checkpoint) tuple; ApplyMigration
// re-derives it to detect drift.
type PlanResponse struct {
	PlanID         string        `json:"plan_id"`
	Class          ClassName     `json:"class"`
	UpSQL          string        `json:"up_sql"`
	DownSQL        string        `json:"down_sql"`
	ImpactReport   []ImpactEntry `json:"impact_report"`
	ParseErrors    []string      `json:"parse_errors"`
	BreakingDetail []string      `json:"breaking_detail"`

	// CheckpointHash is the content hash of the IR checkpoint at plan time.
	// Sent back in ApplyRequest for CAS conflict detection.
	CheckpointHash string `json:"checkpoint_hash"`

	// CustomSQLErrors lists pg_query_go validation failures for query/procedure blocks.
	// Empty if all custom SQL validates.
	CustomSQLErrors []string `json:"custom_sql_errors,omitempty"`

	// CustomCount tallies custom queries and procedures in the new IR.
	CustomCount CustomDeclCount `json:"custom_count"`

	// Phase-split outputs for `tide apply --backfill`. Empty for non-
	// backfill plans. BackfillFields drives the chunked-UPDATE worker:
	// one entry per field, with the user expression + PK column already
	// resolved against the new IR.
	PreBackfillUpSQL       string             `json:"pre_backfill_up_sql,omitempty"`
	PreBackfillIndexesSQL  string             `json:"pre_backfill_indexes_sql,omitempty"`
	PostBackfillUpSQL      string             `json:"post_backfill_up_sql,omitempty"`
	PostBackfillIndexesSQL string             `json:"post_backfill_indexes_sql,omitempty"`
	BackfillFields         []BackfillFieldRef `json:"backfill_fields,omitempty"`

	// Extensions lists the Postgres extensions the new IR requires, with
	// one of three actions per extension: "ok" (already enabled),
	// "enable" (atlantis will CREATE EXTENSION inside the apply tx),
	// "missing" (operator must install at the OS level — apply refuses).
	// Empty when the schema needs no extensions.
	Extensions []extensionStatus `json:"extensions,omitempty"`

	// IndexDrift lists live bare-UNIQUE indexes on declared columns that the
	// schema doesn't account for (e.g. a pre-adopt `CREATE UNIQUE INDEX`).
	// Surfaced as a plan-time warning; `tide apply` refuses on a non-empty
	// list unless ATLANTIS_ALLOW_INDEX_DRIFT=1. Best-effort at plan time —
	// IndexDriftError carries the message when the check itself couldn't run.
	IndexDrift      []introspect.UniqueIndexDrift `json:"index_drift,omitempty"`
	IndexDriftNotes []string                      `json:"index_drift_notes,omitempty"`
	IndexDriftError string                        `json:"index_drift_error,omitempty"`

	// CheckDrift lists CHECK constraints that diverge between the declared
	// schema and the live table — declared-not-enforced (the .atl's check
	// isn't on the table) or live-not-declared (the table enforces a check
	// the .atl doesn't). Same lifecycle as IndexDrift: a plan-time warning,
	// and `tide apply` refuses on a non-empty list unless
	// ATLANTIS_ALLOW_CHECK_DRIFT=1. CheckDriftError carries the message when
	// the check itself couldn't run.
	CheckDrift      []introspect.CheckConstraintDrift `json:"check_drift,omitempty"`
	CheckDriftNotes []string                          `json:"check_drift_notes,omitempty"`
	CheckDriftError string                            `json:"check_drift_error,omitempty"`

	// ColumnDrift lists columns whose live type/width differs from the
	// declared one (e.g. live varchar(10) vs declared varchar(255)). Same
	// lifecycle as the others: a plan-time warning, and `tide apply` refuses
	// on a non-empty list unless ATLANTIS_ALLOW_COLUMN_DRIFT=1.
	ColumnDrift      []introspect.ColumnTypeDrift `json:"column_drift,omitempty"`
	ColumnDriftNotes []string                     `json:"column_drift_notes,omitempty"`
	ColumnDriftError string                       `json:"column_drift_error,omitempty"`
}

// CustomDeclCount tallies custom queries and procedures.
type CustomDeclCount struct {
	Queries    int `json:"queries"`
	Procedures int `json:"procedures"`
}

// ClassName is the wire-side enum mirroring codegen.ChangeClass.
type ClassName string

const (
	ClassAdditive ClassName = "additive"
	ClassBackfill ClassName = "backfill_required"
	ClassBreaking ClassName = "cross_caller_breaking"
	ClassUnclean  ClassName = "unparseable" // returned when DSL itself doesn't parse
)

// ImpactEntry describes how one caller is affected by a plan; includes the plan's own caller.
type ImpactEntry struct {
	Caller   string `json:"caller"`
	Affected bool   `json:"affected"`
	Detail   string `json:"detail,omitempty"`
}

// ApplyRequest is the input to ApplyMigration.
type ApplyRequest struct {
	Caller         string
	PlanID         string
	UpSQL          string // accepted for wire compatibility; never read — the server recomputes from Files. Deleted with this struct.
	Files          []SubmittedFile
	CheckpointHash string // CAS token from PlanResponse; empty for pre-CAS clients
}

// ApplyResponse is returned on a successful apply.
type ApplyResponse struct {
	AppliedAt   string `json:"applied_at"`
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
}

// GetMergedSchemaRequest asks for the union of every caller's registered files.
// SinceVersion is the last Version the client observed; the server omits Files
// when it matches the current version.
type GetMergedSchemaRequest struct {
	SinceVersion string
}

// GetMergedSchemaResponse carries the merged file set. Files is empty when
// SinceVersion equals Version.
type GetMergedSchemaResponse struct {
	Version string          `json:"version"`
	Files   []SubmittedFile `json:"files"`
}

// GetCallerFilesRequest identifies a single caller whose registered files
// should be returned.
type GetCallerFilesRequest struct {
	Caller string
}

// GetCallerFilesResponse carries the named caller's registered .atl files in
// file_path order. Empty if the caller has never applied.
type GetCallerFilesResponse struct {
	Files []SubmittedFile `json:"files"`
}

// GetCallerFiles returns all registered .atl files for a single caller,
// ordered by file_path. Read-only.
func (s *Service) GetCallerFiles(ctx context.Context, req *adminpb.GetCallerFilesRequest) (*adminpb.GetCallerFilesResponse, error) {
	if req.GetCaller() == "" {
		return nil, fmt.Errorf("caller is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT file_path, content
FROM atlantis.caller_registrations
WHERE caller = $1
ORDER BY file_path`, req.GetCaller())
	if err != nil {
		return nil, fmt.Errorf("load caller files: %w", err)
	}
	defer rows.Close()

	var files []SubmittedFile
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return nil, err
		}
		files = append(files, SubmittedFile{Path: path, Content: []byte(content)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &adminpb.GetCallerFilesResponse{Files: submittedFilesToPB(files)}, nil
}

// GetCanonicalIRRequest is the input to GetCanonicalIR. No fields today;
// the caller always wants the current checkpoint IR.
type GetCanonicalIRRequest struct{}

// GetCanonicalIRResponse carries the canonical IR exactly as stored in the
// checkpoint — proto field numbers included — so a caller generating a
// typed client produces wire-identical messages. ContentHash lets the
// caller pin the schema version it generated against.
type GetCanonicalIRResponse struct {
	IR          json.RawMessage `json:"ir"`
	ContentHash string          `json:"content_hash"`
}

// PlanSchema is the workhorse. The flow:
//
//  1. Validate the caller's submitted files parse.
//  2. Load every other caller's current files from caller_registrations.
//  3. Load the last applied IR checkpoint (or nil for an empty DB).
//  4. Lower the union of (caller's new files + everyone else's existing
//     files) into a new IR.
//  5. Diff new IR against the checkpoint.
//  6. Classify; emit SQL; produce impact report.
//
// We do NOT write to caller_registrations here — that happens only when
// ApplyMigration succeeds. PlanSchema is read-only.
func (s *Service) PlanSchema(ctx context.Context, req *adminpb.PlanSchemaRequest) (*adminpb.PlanSchemaResponse, error) {
	reqFiles := submittedFilesFromPB(req.GetFiles())
	if req.GetCaller() == "" {
		return nil, errors.New("admin: caller identity is required")
	}

	// Pass 1: parse the caller's submitted files into one big File set so
	// we can detect DSL errors before merging with anything.
	callerFiles, parseErrs := parseSubmitted(req.GetCaller(), reqFiles)
	if len(parseErrs) > 0 {
		// Surface parse errors up front. The plan is "unclean" — no apply
		// is possible until the caller fixes its own DSL.
		return &adminpb.PlanSchemaResponse{
			Class:       adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE,
			ParseErrors: parseErrs,
		}, nil
	}

	// Load every other caller's stored files.
	others, err := s.loadOtherCallers(ctx, req.GetCaller())
	if err != nil {
		return nil, fmt.Errorf("load other callers: %w", err)
	}

	// Load prior IR checkpoint; nil on first apply.
	prior, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}

	// Lower the union; errors on duplicate entities or unresolved FKs.
	allFiles := append(callerFiles, others...)
	newIR, err := dsl.Lower(allFiles)
	if err != nil {
		return &adminpb.PlanSchemaResponse{
			Class:       adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE,
			ParseErrors: []string{err.Error()},
		}, nil
	}

	// Assign proto numbers before diffing so both sides see stable IDs.
	codegen.AssignProtoNumbers(prior, newIR)

	// Build caller-ownership context so the diff engine can downgrade
	// removals that only affect the submitting caller.
	ownership := buildEntityOwnership(req.GetCaller(), callerFiles, others)
	crossRefs := buildCrossCallerRefs(others)
	d := codegen.ComputeDiff(prior, newIR,
		codegen.WithCallerContext(req.GetCaller(), ownership, crossRefs))

	// Validate every custom query/procedure with pg_query_go. Lowering catches
	// dep-free rules; this catches syntax and unresolved table refs.
	// Scoped to the submitting caller's content — stored content from other
	// callers is loaded into newIR for cross-caller table lookup but isn't
	// re-validated here. It was already validated when its owning caller
	// submitted it; re-validating under whatever rules are in force now
	// would block this caller's apply on drift in some unrelated caller.
	customSQLErrs := validateCustomSQL(newIR, req.GetCaller())

	// Emit SQL and build the impact report.
	var scripts codegen.SQLScripts
	if prior == nil {
		scripts, err = codegen.EmitInitial(newIR)
	} else {
		scripts, err = codegen.EmitSQL(prior, newIR, d)
	}
	if err != nil {
		return nil, fmt.Errorf("emit sql: %w", err)
	}

	// Surface the extension state so `tide plan` can warn before any
	// apply runs. Read-only: pg_available_extensions + pg_extension.
	// Errors here don't fail the plan — the extension check is best-
	// effort, and the apply path will hard-refuse if anything's missing.
	extStatuses, _ := inspectExtensions(ctx, s.pool, newIR)

	// Surface live unique-index drift so `tide plan` warns before apply.
	// Best-effort and read-only (no lock); the apply path re-checks inside
	// the locked tx and refuses. Unlike extensions we DON'T swallow the
	// error silently — a check that couldn't run must not read as "clean,"
	// so we record it and the operator sees the apply-time surprise coming.
	indexDrift, driftNotes, driftErr := introspect.DetectUniqueIndexDrift(ctx, s.pool, newIR)
	var driftErrMsg string
	if driftErr != nil {
		driftErrMsg = driftErr.Error()
	}

	// Same for CHECK-constraint drift: the differ never manages CHECKs, so a
	// constraint that diverged from the .atl at adoption stays divergent and
	// only surfaces as a runtime 23514. Read-only here; the apply path
	// re-checks inside the locked tx and refuses.
	checkDrift, checkNotes, checkErr := introspect.DetectCheckConstraintDrift(ctx, s.pool, newIR)
	var checkErrMsg string
	if checkErr != nil {
		checkErrMsg = checkErr.Error()
	}

	// And column type/width drift (live column type ≠ declared) — the
	// checkpoint→live half of the varchar-length gap. Read-only here.
	columnDrift, columnNotes, columnErr := introspect.DetectColumnTypeDrift(ctx, s.pool, newIR)
	var columnErrMsg string
	if columnErr != nil {
		columnErrMsg = columnErr.Error()
	}

	resp := &adminpb.PlanSchemaResponse{
		PlanId:          computePlanID(req.GetCaller(), callerFiles, prior),
		Class:           planClassToPB(translateClass(d.HighestClass())),
		UpSql:           scripts.Up,
		DownSql:         scripts.Down,
		ImpactReport:    impactToPB(buildImpactReport(req.GetCaller(), others, d, newIR)),
		CheckpointHash:  s.loadCheckpointHash(ctx),
		CustomSqlErrors: customSQLErrs,
		CustomCount: &adminpb.CustomDeclCount{
			Queries:    int32(len(newIR.Queries)),
			Procedures: int32(len(newIR.Procedures)),
		},
		PreBackfillUpSql:       scripts.PreBackfillUp,
		PreBackfillIndexesSql:  scripts.PreBackfillIndexes,
		PostBackfillUpSql:      scripts.PostBackfillUp,
		PostBackfillIndexesSql: scripts.PostBackfillIndexes,
		BackfillFields:         backfillFieldsToPB(translateBackfillFields(scripts.BackfillFields)),
		Extensions:             extensionsToPB(extStatuses),
		IndexDrift:             indexDriftToPB(indexDrift),
		IndexDriftNotes:        driftNotes,
		IndexDriftError:        driftErrMsg,
		CheckDrift:             checkDriftToPB(checkDrift),
		CheckDriftNotes:        checkNotes,
		CheckDriftError:        checkErrMsg,
		ColumnDrift:            columnDriftToPB(columnDrift),
		ColumnDriftNotes:       columnNotes,
		ColumnDriftError:       columnErrMsg,
	}
	for _, ch := range d.Breaking {
		resp.BreakingDetail = append(resp.BreakingDetail,
			fmt.Sprintf("%s/%s: %s", ch.EntityID, ch.Field, ch.Detail))
	}
	// Custom-SQL failures mark the plan unparseable; nothing can apply until fixed.
	if len(customSQLErrs) > 0 {
		resp.Class = adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE
	}
	return resp, nil
}

// translateBackfillFields converts the codegen-side BackfillField slice
// to the admin-wire BackfillFieldRef slice so callers don't import
// internal/codegen.
func translateBackfillFields(in []codegen.BackfillField) []BackfillFieldRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]BackfillFieldRef, len(in))
	for i, f := range in {
		out[i] = BackfillFieldRef{
			EntityID:   f.EntityID,
			Field:      f.Field,
			Expression: f.Expression,
			PKColumn:   f.PKColumn,
			TableName:  f.TableName,
		}
	}
	return out
}

// validateCustomSQL runs pg_query_go validation over the submitting
// caller's custom queries and procedures, using the full IR's entity
// set so cross-caller table references still resolve.
//
// Scope is intentional: stored content from other callers was already
// validated when its owning caller submitted it. If we re-validated it
// here under whatever rules are in force at this moment, drift in some
// unrelated caller's stored content would block this caller's apply —
// for example, a caller that hasn't re-applied since a `table "..."`
// override was added on one of its entities would have stale references
// in its stored procedures, and every other caller would be unable to
// plan until that caller cleaned up. Validating only the submitting
// caller's content keeps each caller responsible for its own SQL while
// still letting the planner see the full schema for type lookup.
//
// caller is the SubmittingCaller; SourcePath on every IR decl is
// formatted as "<caller>:<file-path>" by parseSubmitted / loadOtherCallers,
// so a strings.HasPrefix on "<caller>:" is the right ownership test.
func validateCustomSQL(ir *dsl.IR, caller string) []string {
	prefix := caller + ":"
	var msgs []string
	for i := range ir.Queries {
		if !strings.HasPrefix(ir.Queries[i].SourcePath, prefix) {
			continue
		}
		if err := sqlvalidate.ValidateCustomQuery(ir, &ir.Queries[i]); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	for i := range ir.Procedures {
		if !strings.HasPrefix(ir.Procedures[i].SourcePath, prefix) {
			continue
		}
		if err := sqlvalidate.ValidateCustomProcedure(ir, &ir.Procedures[i]); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	return msgs
}

// ApplyMigration runs the planned SQL in a tx, upserts the caller's files,
// and writes a new IR checkpoint. Serialized by a cluster-wide advisory lock.
// A stale PlanID is rejected; any failure rolls back.
func (s *Service) ApplyMigration(ctx context.Context, req *adminpb.ApplyMigrationRequest) (*adminpb.ApplyMigrationResponse, error) {
	reqFiles := submittedFilesFromPB(req.GetFiles())
	if req.GetCaller() == "" {
		return nil, errors.New("admin: caller identity is required")
	}
	if err := s.requireMutablePlane("schema apply"); err != nil {
		return nil, err
	}
	// req.GetCaller() must match the connecting cert CN, so a leaked cert can
	// only push schema for ITS OWN namespace. Whether this caller may apply at
	// all is CAPABILITY_SCHEMA_APPLY, already checked by the interceptor.
	if err := s.bindCallerIdentity(ctx, req.GetCaller()); err != nil {
		return nil, err
	}
	if req.GetPlanId() == "" {
		return nil, errors.New("admin: plan_id is required")
	}

	// Refuse to apply on top of an in-flight backfill — between Phase 1
	// and Phase 3 the schema is in a partially-applied state and a
	// concurrent unrelated apply can leave it corrupted.
	if planHash, inflight, err := hasInflightBackfill(ctx, s.pool, req.GetCaller()); err == nil && inflight {
		return nil, fmt.Errorf("admin: backfill %s is in flight for this caller — wait for it to complete (or fail) before applying", planHash)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Advisory lock id is a stable 64-bit hash; same value across pods.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	if err := s.upsertCallerFiles(ctx, tx, req.GetCaller(), reqFiles); err != nil {
		return nil, err
	}

	// Re-plan from the persisted state to detect drift.
	parsed, parseErrs := parseSubmitted(req.GetCaller(), reqFiles)
	if len(parseErrs) > 0 {
		return nil, fmt.Errorf("admin: parse failed during apply: %v", parseErrs)
	}
	others, err := s.loadOtherCallersTx(ctx, tx, req.GetCaller())
	if err != nil {
		return nil, err
	}
	newIR, err := dsl.Lower(append(parsed, others...))
	if err != nil {
		return nil, fmt.Errorf("admin: lower failed during apply: %w", err)
	}
	prior, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	codegen.AssignProtoNumbers(prior, newIR)

	// Build caller-ownership context for the diff (same as PlanSchema).
	applyOwnership := buildEntityOwnership(req.GetCaller(), parsed, others)
	applyCrossRefs := buildCrossCallerRefs(others)
	d := codegen.ComputeDiff(prior, newIR,
		codegen.WithCallerContext(req.GetCaller(), applyOwnership, applyCrossRefs))

	// Re-validate inside the lock: another caller's apply between plan and apply
	// can change which tables are visible. Same caller-scoping rationale as
	// the PlanSchema call site above.
	if msgs := validateCustomSQL(newIR, req.GetCaller()); len(msgs) > 0 {
		return nil, fmt.Errorf("admin: custom SQL validation failed: %v", msgs)
	}

	gotPlanID := computePlanID(req.GetCaller(), parsed, prior)
	if gotPlanID != req.GetPlanId() {
		return nil, fmt.Errorf("admin: plan %s is stale; current plan is %s — re-run tide apply",
			req.GetPlanId(), gotPlanID)
	}
	if d.HighestClass() == codegen.ClassCrossCallerBreaking {
		return nil, fmt.Errorf("admin: plan is breaking and cannot be auto-applied")
	}

	var scripts codegen.SQLScripts
	if prior == nil {
		scripts, err = codegen.EmitInitial(newIR)
	} else {
		scripts, err = codegen.EmitSQL(prior, newIR, d)
	}
	if err != nil {
		return nil, fmt.Errorf("emit sql: %w", err)
	}

	// Auto-enable extensions required by the new IR but not yet enabled
	// in this database. Refuses with a structured error if any required
	// extension is missing at OS level. Runs INSIDE the apply tx so the
	// enable + DDL commit atomically — no half-applied state.
	if _, err := prepareExtensions(ctx, tx, newIR); err != nil {
		return nil, err
	}

	// Refuse to apply over a live UNIQUE index the schema doesn't account
	// for — applying would leave a hidden constraint silently rejecting
	// legitimate writes. Read inside the locked tx so the verdict is
	// authoritative. The operator either drops the index, declares the
	// uniqueness, or sets ATLANTIS_ALLOW_INDEX_DRIFT=1 to proceed knowingly.
	if os.Getenv("ATLANTIS_ALLOW_INDEX_DRIFT") != "1" {
		drift, _, derr := introspect.DetectUniqueIndexDrift(ctx, tx, newIR)
		if derr != nil {
			return nil, fmt.Errorf("apply: index-drift check failed: %w", derr)
		}
		if len(drift) > 0 {
			return nil, indexDriftError(drift)
		}
	}

	// Refuse to apply while CHECK constraints diverge between the .atl and
	// the live table — the differ doesn't manage CHECKs, so applying would
	// silently leave the divergence in place (the carts `awaiting_checkout`
	// outage). Read inside the locked tx so the verdict is authoritative.
	// The operator reconciles the constraint out-of-band, or sets
	// ATLANTIS_ALLOW_CHECK_DRIFT=1 to proceed knowingly (e.g. a cosmetic
	// `col IS NULL OR ...` difference).
	if os.Getenv("ATLANTIS_ALLOW_CHECK_DRIFT") != "1" {
		drift, _, derr := introspect.DetectCheckConstraintDrift(ctx, tx, newIR)
		if derr != nil {
			return nil, fmt.Errorf("apply: check-drift check failed: %w", derr)
		}
		if len(drift) > 0 {
			return nil, checkDriftError(drift)
		}
	}

	// Refuse to apply while a column's live type/width diverges from the
	// declaration — the diff path compares against the checkpoint, not live,
	// so applying would leave e.g. a varchar(10) in place under a varchar(255)
	// declaration (the value-too-long outage). Read inside the locked tx. The
	// operator reconciles the column out-of-band, or sets
	// ATLANTIS_ALLOW_COLUMN_DRIFT=1 to proceed knowingly.
	if os.Getenv("ATLANTIS_ALLOW_COLUMN_DRIFT") != "1" {
		drift, _, derr := introspect.DetectColumnTypeDrift(ctx, tx, newIR)
		if derr != nil {
			return nil, fmt.Errorf("apply: column-drift check failed: %w", derr)
		}
		if len(drift) > 0 {
			return nil, columnDriftError(drift)
		}
	}

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	// Use client-provided hash when available (what they planned against);
	// fall back to reading it server-side inside the advisory-locked tx.
	expectedHash := req.GetCheckpointHash()
	if expectedHash == "" {
		expectedHash, _ = loadCheckpointHashTx(ctx, tx)
	}
	meta := versionMeta{
		Caller:       req.GetCaller(),
		PlanClass:    d.HighestClass().String(),
		Diff:         d,
		UpSQL:        scripts.Up,
		DownSQL:      scripts.Down,
		PlanID:       gotPlanID,
		EventType:    "apply",
		ExpectedHash: expectedHash,
	}
	version, err := s.persistCheckpoint(ctx, tx, newIR, meta)
	if err != nil {
		return nil, err
	}

	// Read the newly written content hash for the response.
	newHash, _ := loadCheckpointHashTx(ctx, tx)

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	if s.mirrorEnabled {
		if err := mirrorFiles(s.mirrorDir, req.GetCaller(), reqFiles); err != nil {
			fmt.Fprintf(os.Stderr, "admin: mirror after apply (caller=%s): %v\n", req.GetCaller(), err)
		}
	}

	return &adminpb.ApplyMigrationResponse{AppliedAt: nowUTC(), Version: version, ContentHash: newHash}, nil
}

// mirrorFiles writes each file atomically to <root>/<caller>/<path>.
// Identical content is skipped to avoid mtime churn.
func mirrorFiles(root, caller string, files []SubmittedFile) error {
	if root == "" {
		return errors.New("mirror dir is empty")
	}
	if caller == "" {
		return errors.New("caller is empty")
	}
	// Per-caller subdir prevents path collisions across callers.
	callerRoot := filepath.Join(root, caller)
	for _, f := range files {
		// Reject paths that escape the caller root; the wire input is untrusted.
		clean := filepath.Clean(f.Path)
		if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") || filepath.IsAbs(clean) {
			return fmt.Errorf("invalid file path %q", f.Path)
		}
		dst := filepath.Join(callerRoot, clean)

		if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, f.Content) {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".atl-mirror-*")
		if err != nil {
			return fmt.Errorf("temp file: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(f.Content); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", tmpPath, err)
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("fsync %s: %w", tmpPath, err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("close %s: %w", tmpPath, err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename %s -> %s: %w", tmpPath, dst, err)
		}
	}
	return nil
}

// GetMergedSchema returns the union of every caller's registered files.
// When req.GetSinceVersion() equals the current version, Files is omitted.
func (s *Service) GetMergedSchema(ctx context.Context, req *adminpb.GetMergedSchemaRequest) (*adminpb.GetMergedSchemaResponse, error) {
	// No-PG path. GetMergedSchema and GetCanonicalIR are the two RPCs in this
	// file that reach the pool without an earlier guard rejecting the request,
	// so TestMigratedRPCsAreTracked — which dials a pool-less Service — panics
	// without these. The version must match what a fresh database returns
	// rather than being empty: computeMergedSchemaVersion(nil) is the sha256 of
	// no files, and "" is a version no real path can produce.
	if s.pool == nil {
		return &adminpb.GetMergedSchemaResponse{Version: computeMergedSchemaVersion(nil)}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
ORDER BY caller, file_path`)
	if err != nil {
		return nil, fmt.Errorf("load registrations: %w", err)
	}
	defer rows.Close()

	var raw []mergedEntry
	for rows.Next() {
		var e mergedEntry
		if err := rows.Scan(&e.caller, &e.path, &e.content); err != nil {
			return nil, err
		}
		raw = append(raw, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	version := computeMergedSchemaVersion(raw)
	resp := &adminpb.GetMergedSchemaResponse{Version: version}
	if req.GetSinceVersion() == version {
		// Client is up to date — return version only.
		return resp, nil
	}
	for _, e := range raw {
		resp.Files = append(resp.Files, &adminpb.SubmittedFile{
			Path:    e.path,
			Content: []byte(e.content),
		})
	}
	return resp, nil
}

// GetCanonicalIR returns the checkpoint IR verbatim — the raw bytes stored
// at apply time, with proto field numbers intact. Callers generate their
// typed client from this so wire encoding matches the server exactly;
// re-lowering the .atl files locally could assign different field numbers.
// Read-only. Returns an empty IR on a fresh database.
func (s *Service) GetCanonicalIR(ctx context.Context, _ *adminpb.GetCanonicalIRRequest) (*adminpb.GetCanonicalIRResponse, error) {
	// No-PG path; see the note in GetMergedSchema. Returns the same
	// empty-checkpoint shape a fresh database does, so `tide generate` hits its
	// existing "server has no schema yet" branch rather than a new one.
	if s.pool == nil {
		return &adminpb.GetCanonicalIRResponse{Ir: []byte("null")}, nil
	}
	var raw []byte
	var contentHash string
	err := s.pool.QueryRow(ctx,
		`SELECT ir, content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw, &contentHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &adminpb.GetCanonicalIRResponse{Ir: []byte("null")}, nil
		}
		return nil, fmt.Errorf("load canonical IR: %w", err)
	}
	return &adminpb.GetCanonicalIRResponse{Ir: raw, ContentHash: contentHash}, nil
}

// mergedEntry is the in-memory shape of one caller_registrations row used
// only by GetMergedSchema. Kept private — the wire shape is SubmittedFile.
type mergedEntry struct {
	caller, path, content string
}

// computeMergedSchemaVersion hashes (caller, path, content) in their query
// order so any byte-level change to any registered file shifts the version.
// Truncated to 16 hex chars — still 64 bits of collision resistance, with
// a compact value clients can log without overwhelming the line.
func computeMergedSchemaVersion(entries []mergedEntry) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.caller))
		h.Write([]byte{0})
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(e.content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// upsertCallerFiles replaces this caller's full submission set inside the tx.
// We DELETE then INSERT (vs ON CONFLICT) so files that the caller dropped
// from their submission are removed from storage too — otherwise a caller
// could leave orphan registrations behind.
func (s *Service) upsertCallerFiles(ctx context.Context, tx pgx.Tx, caller string, files []SubmittedFile) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM atlantis.caller_registrations WHERE caller = $1`, caller); err != nil {
		return fmt.Errorf("upsert: clear prior: %w", err)
	}
	for _, f := range files {
		h := sha256.Sum256(f.Content)
		_, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
VALUES ($1, $2, $3, $4)`,
			caller, f.Path, string(f.Content), hex.EncodeToString(h[:]))
		if err != nil {
			return fmt.Errorf("upsert: insert %s: %w", f.Path, err)
		}
	}
	return nil
}

func parseSubmitted(caller string, files []SubmittedFile) ([]*dsl.File, []string) {
	var out []*dsl.File
	var errs []string
	for _, f := range files {
		parsed, err := dsl.Parse(caller+":"+f.Path, f.Content)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		out = append(out, parsed)
	}
	return out, errs
}

func (s *Service) loadOtherCallers(ctx context.Context, exclude string) ([]*dsl.File, error) {
	rows, err := s.pool.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
WHERE caller != $1
ORDER BY caller, file_path`, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dsl.File
	for rows.Next() {
		var caller, path, content string
		if err := rows.Scan(&caller, &path, &content); err != nil {
			return nil, err
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			// Another caller's submission is broken. We surface this as an
			// error rather than silently dropping the file — a broken
			// caller shouldn't permit this caller to plan around them.
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) loadOtherCallersTx(ctx context.Context, tx pgx.Tx, exclude string) ([]*dsl.File, error) {
	rows, err := tx.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
WHERE caller != $1
ORDER BY caller, file_path`, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dsl.File
	for rows.Next() {
		var caller, path, content string
		if err := rows.Scan(&caller, &path, &content); err != nil {
			return nil, err
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) loadCheckpoint(ctx context.Context) (*dsl.IR, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return dsl.DecodeJSONIR(raw)
}

func (s *Service) loadCheckpointTx(ctx context.Context, tx pgx.Tx) (*dsl.IR, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return dsl.DecodeJSONIR(raw)
}

func (s *Service) loadCheckpointHash(ctx context.Context) string {
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&hash)
	if err != nil {
		return ""
	}
	return hash
}

// versionMeta holds the metadata for one schema_versions row. Passed to
// persistCheckpoint so the caller can supply the diff, SQL, plan ID, and
// event type without persistCheckpoint needing to know how they were
// produced.
type versionMeta struct {
	Caller       string
	PlanClass    string
	Diff         *codegen.Diff
	UpSQL        string
	DownSQL      string
	PlanID       string
	EventType    string // "apply", "rollback", "adopt"
	ParentVer    *int64
	ExpectedHash string // CAS token — if set, reject when current checkpoint hash differs
}

func (s *Service) persistCheckpoint(ctx context.Context, tx pgx.Tx, ir *dsl.IR, meta versionMeta) (int64, error) {
	raw, err := ir.EncodeJSON()
	if err != nil {
		return 0, err
	}

	h := sha256.Sum256(raw)
	irHash := hex.EncodeToString(h[:])

	// CAS: reject if the checkpoint has moved since the caller planned.
	if meta.ExpectedHash != "" {
		got, err := loadCheckpointHashTx(ctx, tx)
		if err != nil {
			return 0, fmt.Errorf("cas: %w", err)
		}
		if got != "" && got != meta.ExpectedHash {
			return 0, fmt.Errorf("admin: checkpoint has moved (expected %s, got %s) — re-plan and retry",
				meta.ExpectedHash[:min(12, len(meta.ExpectedHash))],
				got[:min(12, len(got))])
		}
	}

	_, err = tx.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by, content_hash) VALUES (1, $1, $2, $3)
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir, applied_at = now(), applied_by = EXCLUDED.applied_by, content_hash = EXCLUDED.content_hash`,
		raw, meta.Caller, irHash)
	if err != nil {
		return 0, fmt.Errorf("upsert ir_checkpoint: %w", err)
	}

	var diffJSON []byte
	if meta.Diff != nil {
		diffJSON, err = json.Marshal(meta.Diff)
		if err != nil {
			return 0, fmt.Errorf("marshal diff: %w", err)
		}
	} else {
		diffJSON = []byte(`{"additive":[],"backfill_required":[],"breaking":[]}`)
	}

	var version int64
	err = tx.QueryRow(ctx, `
INSERT INTO atlantis.schema_versions
    (caller, plan_class, diff, up_sql, down_sql, ir_snapshot, ir_hash, plan_id, parent_version, event_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING version`,
		meta.Caller, meta.PlanClass, diffJSON, meta.UpSQL, meta.DownSQL,
		raw, irHash, meta.PlanID, meta.ParentVer, meta.EventType,
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("insert schema_versions: %w", err)
	}

	if err := updateEntityLineage(ctx, tx, version, meta.Caller, meta.Diff); err != nil {
		return 0, fmt.Errorf("update entity lineage: %w", err)
	}

	return version, nil
}

// loadCheckpointHashTx reads the current content hash from ir_checkpoint
// within an existing transaction. Returns "" if no checkpoint exists.
func loadCheckpointHashTx(ctx context.Context, tx pgx.Tx) (string, error) {
	var hash string
	err := tx.QueryRow(ctx, `SELECT content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return hash, nil
}

// computePlanID hashes (caller, files, prior checkpoint hash) so applies can
// detect drift since planning. Stable across reruns of the same plan.
func computePlanID(caller string, files []*dsl.File, prior *dsl.IR) string {
	h := sha256.New()
	h.Write([]byte(caller))
	h.Write([]byte{0})
	// Sort files by path for determinism.
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	if prior != nil {
		b, _ := prior.EncodeJSON()
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func translateClass(c codegen.ChangeClass) ClassName {
	switch c {
	case codegen.ClassAdditive:
		return ClassAdditive
	case codegen.ClassBackfillRequired:
		return ClassBackfill
	case codegen.ClassCrossCallerBreaking:
		return ClassBreaking
	}
	return ClassUnclean
}

// buildImpactReport summarizes how each known caller is affected.
//
// "Affected" = at least one Change in the diff names an entity this caller
// has files for. The plan's own caller is included (with their own caller-
// scoped detail) so the CLI can render a single unified table.
func buildImpactReport(planCaller string, others []*dsl.File, d *codegen.Diff, _ *dsl.IR) []ImpactEntry {
	// Bucket every entity ID touched in the diff.
	touched := map[string]bool{}
	count := 0
	for _, ch := range d.Additive {
		touched[ch.EntityID] = true
		count++
	}
	for _, ch := range d.BackfillRequired {
		touched[ch.EntityID] = true
		count++
	}
	for _, ch := range d.Breaking {
		touched[ch.EntityID] = true
		count++
	}

	// Group other callers by name and tally how many of their entities are touched.
	otherByCaller := map[string][]string{}
	for _, f := range others {
		// Path is "caller:filename"; split on the first colon.
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		otherByCaller[c] = append(otherByCaller[c], f.Path)
	}

	// Build a set of entity IDs declared in each other caller's files so
	// we can determine whether a caller is actually affected by the diff.
	callerEntities := map[string]map[string]bool{} // caller → set of entityIDs
	for _, f := range others {
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		if callerEntities[c] == nil {
			callerEntities[c] = map[string]bool{}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				callerEntities[c][d.Namespace+"."+d.Name] = true
			case *dsl.HypertableDecl:
				callerEntities[c][d.Namespace+"."+d.Name] = true
			}
		}
	}

	var report []ImpactEntry
	for caller := range otherByCaller {
		affected := false
		if ents, ok := callerEntities[caller]; ok {
			for entID := range touched {
				if entID != "" && ents[entID] {
					affected = true
					break
				}
			}
		}
		detail := fmt.Sprintf("%d change(s) touched the union", count)
		if affected {
			detail = fmt.Sprintf("%d change(s) touch entities this caller declares", count)
		}
		report = append(report, ImpactEntry{
			Caller:   caller,
			Affected: affected,
			Detail:   detail,
		})
	}
	report = append(report, ImpactEntry{
		Caller:   planCaller,
		Affected: true,
		Detail:   fmt.Sprintf("%d change(s) in this plan", count),
	})
	sort.Slice(report, func(i, j int) bool { return report[i].Caller < report[j].Caller })
	return report
}

// indexOf returns the first index of c in s, or -1.
func indexOf(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// buildEntityOwnership returns a map of entityID → caller for every entity
// declared across the submitting caller's files and all other callers' files.
// The caller name is extracted from the file path prefix ("caller:path").
func buildEntityOwnership(callerName string, callerFiles []*dsl.File, otherFiles []*dsl.File) map[string]string {
	out := map[string]string{}

	// Helper: walk one file's decls and attribute entities to the given caller.
	register := func(caller string, f *dsl.File) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				out[d.Namespace+"."+d.Name] = caller
			case *dsl.HypertableDecl:
				out[d.Namespace+"."+d.Name] = caller
			}
		}
	}

	for _, f := range callerFiles {
		register(callerName, f)
	}
	for _, f := range otherFiles {
		c := f.Path
		if i := indexOf(c, ':'); i >= 0 {
			c = c[:i]
		}
		register(c, f)
	}
	return out
}

// buildCrossCallerRefs scans other callers' parsed files and returns a set
// of entity IDs and "entityID.fieldName" strings that are referenced by
// callers other than the submitter. A reference is any `references
// ns.Entity.field` modifier on a FieldDecl in another caller's file.
//
// This is a conservative scan over the AST — it does not need the lowered IR.
func buildCrossCallerRefs(otherFiles []*dsl.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range otherFiles {
		for _, decl := range f.Decls {
			var members []dsl.EntityMember
			switch d := decl.(type) {
			case *dsl.EntityDecl:
				members = d.Members
			case *dsl.HypertableDecl:
				members = d.Members
			default:
				// QueryDecl / ProcedureDecl: scan touches() for entity refs.
				scanDeclTouches(decl, out)
				continue
			}
			for _, m := range members {
				fd, ok := m.(*dsl.FieldDecl)
				if !ok {
					continue
				}
				for _, mod := range fd.Modifiers {
					ref, ok := mod.(*dsl.ModReferencesDecl)
					if !ok {
						continue
					}
					targetID := ref.TargetNS + "." + ref.TargetEntity
					out[targetID] = true
					out[targetID+"."+ref.TargetField] = true
				}
			}
		}
	}
	return out
}

// scanDeclTouches extracts entity references from touches() clauses on
// QueryDecl and ProcedureDecl and adds them to the refs set. This ensures
// that entities mentioned in another caller's custom queries/procedures are
// treated as cross-referenced.
func scanDeclTouches(decl dsl.Decl, refs map[string]bool) {
	switch d := decl.(type) {
	case *dsl.QueryDecl:
		if d.SQL != nil {
			for _, t := range d.SQL.Touches {
				if t.Namespace != "" {
					refs[t.Namespace+"."+t.Name] = true
				}
			}
		}
	case *dsl.ProcedureDecl:
		for _, step := range d.Steps {
			if step.Raw != nil {
				for _, t := range step.Raw.Touches {
					if t.Namespace != "" {
						refs[t.Namespace+"."+t.Name] = true
					}
				}
			}
			if step.Typed != nil && step.Typed.Target.Namespace != "" {
				refs[step.Typed.Target.Namespace+"."+step.Typed.Target.Name] = true
			}
		}
	}
}

// nowUTC returns the current UTC time formatted as RFC3339. Broken out so
// tests can swap it via a build-tag override.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
