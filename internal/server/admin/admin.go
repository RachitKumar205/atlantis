// Package admin implements the atlantis control-plane RPCs: schema
// lifecycle (plan, apply, adopt, rollback, history, lineage), caller
// identity management (register, revoke, cert-expiry tracking),
// declarative jobs and workflows (submit, status, dead/retry), and
// operational telemetry (entity owners, in-process log ring).
//
// RPCs are served over protobuf at atlantis.admin.v1.AdminService (grpcgen.go),
// governed by the capability interceptor in internal/server/authz.
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlvalidate"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/obs"
	"github.com/rachitkumar205/atlantis/internal/secrets"
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
// No per-CN allowlist sits between (1) and (2). Authorization as process
// configuration cannot be audited or changed without a restart, and an operator
// list falling back to a global wildcard when unset — the shipped default —
// makes every mutation-capable caller an operator.
type Service struct {
	// pool is the control database: everything under the atlantis schema —
	// the checkpoint, schema versions, lineage, plans, callers, capabilities,
	// jobs and workflows. It is the pod's own Postgres.
	pool *pgxpool.Pool

	// managed is the database whose schema this server manages: what
	// introspection reads and what DDL is applied to. Equal to pool unless a
	// managed DSN was configured, which is how every deployment predating the
	// split keeps working.
	//
	// The two are separate connections, so an apply cannot be one transaction
	// across both. DDL commits to managed first and the checkpoint is written
	// to pool after, which fails toward the record lagging the database —
	// the state the drift checks already detect and report on the next plan.
	// The other order would have atlantis claim a change that did not happen.
	managed *pgxpool.Pool

	// managedFromEnv records that `managed` came from ATL_MANAGED_PG_URL
	// rather than the table. An environment DSN pins the managed database and
	// is never reopened from atlantis.managed_database.
	managedFromEnv bool

	// managedState caches the pool opened from the stored DSN, and the version
	// it was opened at. See manageddb.go.
	managedState managedState

	// keys seals the managed database's DSN. Nil leaves it unsettable, which
	// is every deployment that configures no keyring.
	keys secrets.Keyring

	mirrorDir          string
	mirrorEnabled      bool
	allowApplyMutation bool
	callerFromContext  func(context.Context) string
	backfillEnabled    bool
	logRing            *obs.LogRing

	// hasCapability answers "is this request an operator" for the job RPCs.
	// See Config.HasCapability for why nil narrows.
	hasCapability func(ctx context.Context, c adminpb.Capability) (bool, error)

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

	// applyPolicyFloor caps every caller's apply-policy tier. '' means no
	// floor. Validated at boot by ParseApplyPolicyFloor.
	applyPolicyFloor string

	// rehearsalTargetDSN pins where rehearsal clones are created. '' falls
	// through to the sealed row, then the managed cluster itself.
	rehearsalTargetDSN string
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

	// HasCapability reports whether the calling identity holds one
	// capability. The job RPCs use it for a single decision: an operator
	// reads and retries across every caller, and everybody else is scoped to
	// the jobs they own.
	//
	// A function rather than an authz.Grants, matching CallerFromContext
	// beside it, and a predicate rather than a list because a list is a
	// second thing to keep in step — this package needs one answer, not an
	// inventory it would have to search itself.
	//
	// When nil, no request is treated as an operator, so every caller sees
	// only its own jobs. That is the safe direction for a misconfiguration:
	// forgetting to wire this narrows what is visible rather than widening
	// it, and the symptom is an operator's console showing an empty DLQ
	// rather than a caller reading another caller's args.
	HasCapability func(ctx context.Context, c adminpb.Capability) (bool, error)

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

	// ApplyPolicyFloor caps every caller's apply-policy tier. '' means no
	// floor. Pass a value ParseApplyPolicyFloor accepted; New stores it
	// verbatim.
	ApplyPolicyFloor string

	// RehearsalTargetDSN pins where rehearsal clones are created
	// (ATL_REHEARSAL_PG_URL). '' falls through to the sealed
	// atlantis.rehearsal_database row, then the managed cluster itself.
	RehearsalTargetDSN string

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

	// Keys seals the managed database's DSN in atlantis.managed_database.
	// Nil leaves it unsettable and the server manages its own database.
	Keys secrets.Keyring

	// Managed is the database whose schema this server manages. Nil means the
	// control database manages itself.
	//
	// Set from ATL_MANAGED_PG_URL. An organisation that adopted an existing
	// database points this at it: the tables are already there, so introspection
	// reads them and apply emits no DDL for what already matches.
	Managed *pgxpool.Pool
}

// New returns a Service backed by pool.
//
// cfg.Managed names the database whose schema is managed. Nil means the
// control database manages itself, which is what every deployment before the
// split did and still does.
func New(pool *pgxpool.Pool, cfg Config) *Service {
	managed := cfg.Managed
	fromEnv := managed != nil
	if managed == nil {
		managed = pool
	}
	return &Service{
		pool:               pool,
		managed:            managed,
		managedFromEnv:     fromEnv,
		keys:               cfg.Keys,
		mirrorDir:          cfg.MirrorDir,
		mirrorEnabled:      cfg.MirrorEnabled,
		allowApplyMutation: cfg.AllowApplyMutation,
		callerFromContext:  cfg.CallerFromContext,
		backfillEnabled:    cfg.BackfillEnabled,
		logRing:            cfg.LogRing,

		hasCapability: cfg.HasCapability,

		proxyForwarded:         cfg.ProxyForwardedFromContext,
		trustedProxyMayApply:   cfg.TrustedProxyMayApply,
		trustedProxyMayOperate: cfg.TrustedProxyMayOperate,

		applyPolicyFloor:   cfg.ApplyPolicyFloor,
		rehearsalTargetDSN: cfg.RehearsalTargetDSN,
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
// Not authorization, and no overlap with the capability check: one operator
// decision about whether this atlantis accepts schema change over the wire at
// all. ATL_ALLOW_APPLY_MUTATION=false is the regulated posture documented in
// ops/architecture/schema-flow.md, where tidectl materialises the SQL into
// files reviewed in the operator's own version control — a process atlantis
// neither opens nor observes — and no grant routes around it. It is also the
// switch for an incident, which a per-caller grant cannot express without being
// revoked caller by caller and restored the same way.
//
// In the handler rather than the interceptor because it is a property of
// neither the caller nor the method: one bit of server state, which three RPCs
// in jobs.go and workflows.go already read directly.
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
// It did not move to the interceptor along with the capability check: an
// interceptor sees the method name and the connection, not request bodies.
// Without it, SCHEMA_APPLY means "may write to any caller's schema" rather than
// "may write to mine".
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

	// CheckpointHash is the compare-and-swap token, sent back in ApplyRequest.
	// It covers the part of the checkpoint this caller depends on rather than
	// the whole of it — see callerDependencyHash.
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
	ClassAdditive    ClassName = "additive"
	ClassBackfill    ClassName = "backfill_required"
	ClassBreaking    ClassName = "cross_caller_breaking"
	ClassDestructive ClassName = "destructive"
	ClassUnclean     ClassName = "unparseable" // returned when the schema fails validation
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
// caller_registrations is not written here; only a successful ApplyMigration
// writes it. PlanSchema is read-only.
func (s *Service) PlanSchema(ctx context.Context, req *adminpb.PlanSchemaRequest) (*adminpb.PlanSchemaResponse, error) {
	reqFiles := submittedFilesFromPB(req.GetFiles())
	if req.GetCaller() == "" {
		return nil, errors.New("admin: caller identity is required")
	}

	// Pass 1: parse the caller's submitted files into one File set, so DSL
	// errors surface before anything is merged.
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

	// The database the plan describes.
	mpool, err := s.managedPool(ctx)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	// Build caller-ownership context so the diff engine can downgrade
	// removals that only affect the submitting caller.
	ownership := buildEntityOwnership(req.GetCaller(), callerFiles, others)
	crossRefs := buildCrossCallerRefs(others)
	d, parkConflicts, err := diffWithParked(ctx, mpool, s.pool, prior, newIR,
		codegen.WithCallerContext(req.GetCaller(), ownership, crossRefs))
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if len(parkConflicts) > 0 {
		return &adminpb.PlanSchemaResponse{
			Class:       adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE,
			ParseErrors: parkConflicts,
		}, nil
	}

	// The token the apply is checked against. Scoped to what this caller reads
	// so an unrelated caller's apply does not invalidate this plan.
	depHash, err := callerDependencyHash(req.GetCaller(), prior, ownership, callerFiles, d)
	if err != nil {
		return nil, err
	}

	// Validate every custom query/procedure with pg_query_go. Lowering catches
	// dep-free rules; this catches syntax and unresolved table refs.
	// Scoped to the submitting caller's content — stored content from other
	// callers is loaded into newIR for cross-caller table lookup but isn't
	// re-validated here. It was already validated when its owning caller
	// submitted it; re-validating under whatever rules are in force now
	// would block this caller's apply on drift in some unrelated caller.
	customSQLErrs := validateCustomSQL(newIR, req.GetCaller(), ownership)

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
	extStatuses, _ := inspectExtensions(ctx, mpool, newIR)

	// Surface live drift so `tide plan` warns before apply. Best-effort and
	// read-only (no lock); the apply re-checks after its DDL and refuses. An
	// error is recorded rather than swallowed as extensions are, so a check
	// that could not run does not read as clean.
	//
	// Read against the checkpoint plus the entities this plan adds: the
	// database has not run this plan's DDL, so against newIR every object the
	// plan changes would read as drift.
	driftIR := withAddedEntities(prior, newIR)
	indexDrift, driftNotes, driftErr := introspect.DetectUniqueIndexDrift(ctx, mpool, driftIR)
	var driftErrMsg string
	if driftErr != nil {
		driftErrMsg = driftErr.Error()
	}
	checkDrift, checkNotes, checkErr := introspect.DetectCheckConstraintDrift(ctx, mpool, driftIR)
	if checkErr == nil {
		checkDrift, checkErr = withoutParkedChecks(ctx, mpool, checkDrift)
	}
	var checkErrMsg string
	if checkErr != nil {
		checkErrMsg = checkErr.Error()
	}
	columnDrift, columnNotes, columnErr := introspect.DetectColumnTypeDrift(ctx, mpool, driftIR)
	var columnErrMsg string
	if columnErr != nil {
		columnErrMsg = columnErr.Error()
	}

	resp := &adminpb.PlanSchemaResponse{
		PlanId:          computePlanID(req.GetCaller(), reqFiles, depHash),
		Class:           planClassToPB(translateClass(d.HighestClass())),
		UpSql:           scripts.Up,
		DownSql:         scripts.Down,
		ImpactReport:    impactToPB(buildImpactReport(req.GetCaller(), others, d, newIR)),
		CheckpointHash:  depHash,
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
	// Destructive changes are listed here too. They are parked rather than
	// dropped and so are recoverable, but a plan that says nothing about the
	// rows it is about to move is a plan the reviewer cannot assess.
	for _, ch := range d.Destructive {
		resp.BreakingDetail = append(resp.BreakingDetail,
			fmt.Sprintf("%s/%s: %s (parked, reaped after the retention window)",
				ch.EntityID, ch.Field, ch.Detail))
	}
	// Custom-SQL failures mark the plan unparseable; nothing can apply until fixed.
	if len(customSQLErrs) > 0 {
		resp.Class = adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE
	}

	// What the change policy says about this class. Read after the class is
	// final, so an unparseable plan is not reported as awaiting an approval it
	// will never be offered.
	//
	// A failed read is not fatal here. The plan is a read-only preview and the
	// gate lives at apply time; refusing to plan because a policy row could not
	// be fetched would take away the operator's ability to see what is wrong.
	if policyClassIsSettable(resp.Class) && s.pool != nil {
		if stored, _, err := loadChangePolicy(ctx, s.pool); err == nil {
			p := effectiveChangePolicy(stored, resp.Class)
			resp.RequiresApproval = p.RequireApproval
			if p.RequireApproval {
				resp.ApproverRole = p.ApproverRole
			}
		}
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
// Scoped: stored content from other callers was validated when its owning
// caller submitted it. Re-validating it here under whatever rules are in force
// now would let drift in an unrelated caller's stored content block this
// caller's apply —
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
func validateCustomSQL(ir *dsl.IR, caller string, owns map[string]string) []string {
	prefix := caller + ":"
	var msgs []string
	// Entity-level SQL first: CHECK expressions and partial-index predicates
	// are author-written and emitted verbatim into DDL, and a CHECK is not
	// required to be IMMUTABLE — so a call planted there runs on every INSERT
	// and UPDATE. Gating only query and procedure bodies left that open.
	//
	// Scoped through the ownership map rather than SourcePath, because
	// dsl.Entity does not carry one. Same rationale as the query loop below:
	// another caller's stored content was validated when its owner submitted
	// it, and re-judging it here would let unrelated drift block this apply.
	//
	// A nil or empty map means ownership could not be established, and the gate
	// then checks everything rather than nothing. Scoping is an availability
	// concession, keeping one caller's drift from blocking another's apply, and
	// an unknown owner is not a reason to skip the check. The other way round,
	// passing nil disables the gate silently.
	for i := range ir.Entities {
		if len(owns) > 0 && owns[ir.Entities[i].ID()] != caller {
			continue
		}
		if err := sqlvalidate.ValidateEntityExpressions(&ir.Entities[i]); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
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

	// The transaction the schema is read from and the DDL is applied to.
	//
	// The same transaction as the control one when no managed database is
	// configured, which is every deployment predating the split: one
	// connection, one commit, exactly as before.
	//
	// Two separate databases cannot share a transaction, so mtx commits before
	// the checkpoint is written and the two are consistent only across that
	// gap. Ordered so a crash inside it leaves the database ahead of the
	// record — what the drift checks detect — rather than a
	// record of a change that did not happen.
	mpool, err := s.managedPool(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	mtx := tx
	if mpool != s.pool {
		m, err := mpool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return nil, fmt.Errorf("apply: begin on the managed database: %w", err)
		}
		defer func() { _ = m.Rollback(context.Background()) }()
		// The same lock, on the database the DDL lands in: two servers sharing
		// a managed database must not apply over each other.
		if _, err := m.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
			return nil, fmt.Errorf("advisory lock on the managed database: %w", err)
		}
		mtx = m
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
	// Read through mtx after the advisory lock, so no other apply moves a
	// parked column before the DDL runs. The reaper takes no lock; a restore
	// whose column it drops in between raises in the DDL.
	d, parkConflicts, err := diffWithParked(ctx, mtx, tx, prior, newIR,
		codegen.WithCallerContext(req.GetCaller(), applyOwnership, applyCrossRefs))
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	if len(parkConflicts) > 0 {
		return nil, status.Error(codes.FailedPrecondition,
			"admin: "+strings.Join(parkConflicts, "; "))
	}

	// Re-validate inside the lock: another caller's apply between plan and apply
	// can change which tables are visible. Same caller-scoping rationale as
	// the PlanSchema call site above.
	if msgs := validateCustomSQL(newIR, req.GetCaller(), applyOwnership); len(msgs) > 0 {
		return nil, fmt.Errorf("admin: custom SQL validation failed: %v", msgs)
	}

	depHash, err := callerDependencyHash(req.GetCaller(), prior, applyOwnership, parsed, d)
	if err != nil {
		return nil, err
	}
	gotPlanID := computePlanID(req.GetCaller(), reqFiles, depHash)
	if gotPlanID != req.GetPlanId() {
		return nil, fmt.Errorf("admin: plan %s is stale; current plan is %s — re-run tide apply",
			req.GetPlanId(), gotPlanID)
	}
	// The compare-and-swap.
	//
	// It adds less than it looks over the check above: the plan id is hashed
	// over this same token, so any drift that would fail here has already
	// failed there. What is left is a client sending the plan id from one plan
	// and the token from another — a mixed-up retry wrapper, not schema drift.
	// The token travels on the wire regardless, because an approval is recorded
	// against the ground the approver saw and this field is that ground.
	//
	// Checked here rather than at the end of the transaction beside the
	// checkpoint write. That is after the DDL, so a caller working from moved
	// ground executes its whole migration before anything looks at the token.
	// The transaction rolls back either way, but the DDL has taken its locks by
	// then — on a large table, minutes of ACCESS EXCLUSIVE spent on an apply
	// that was always going to be refused.
	//
	// An empty token skips the check. That is the first apply into an empty
	// database, where there is no checkpoint to have moved.
	if want := req.GetCheckpointHash(); want != "" && want != depHash {
		return nil, fmt.Errorf("admin: the schema this plan depends on has moved (planned against %s, now %s) — re-plan and retry",
			want[:min(12, len(want))], depHash[:min(12, len(depHash))])
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

	// Whether this class of change may apply without a human.
	//
	// This is the gate that replaced a flat refusal of cross-caller-breaking
	// and destructive plans. That refusal existed HERE rather than in the CLI's
	// switch statement for a reason worth restating, because it is the reason
	// this is here too: anything that skipped the switch — a script, a retry
	// wrapper, an older or newer tide, any direct gRPC client holding
	// CAPABILITY_SCHEMA_APPLY — could pass a valid plan id and the DROP ran. A
	// gate that lives in the client is not a gate.
	//
	// It runs after emit because it compares against the SQL that would
	// actually execute, and before the drift checks and the DDL because a
	// change waiting on a human should not first spend the migration's locks.
	gate, err := s.evaluateApplyGates(ctx, tx, gateRequest{
		Caller:      req.GetCaller(),
		PlanID:      gotPlanID,
		RequestedBy: req.GetCaller(),
		Files:       reqFiles,
		FilesHash:   filesHash(reqFiles),
		BaseHash:    depHash,
		Diff:        d,
		UpSQL:       scripts.Up,
		DownSQL:     scripts.Down,
		Now:         time.Now().UTC(),
		Actor:       req.GetActor(),
	})
	if err != nil {
		return nil, err
	}

	// Auto-enable extensions required by the new IR but not yet enabled
	// in this database. Refuses with a structured error if any required
	// extension is missing at OS level. Runs INSIDE the apply tx so the
	// enable + DDL commit atomically — no half-applied state.
	if _, err := prepareExtensions(ctx, tx, newIR); err != nil {
		return nil, err
	}

	// Refuse to turn on tenant isolation over rows nobody will be able to read.
	//
	// `''` is a legal value for a NOT NULL text column and is what a legacy
	// discriminator carries after an ADD COLUMN with a default. Once the
	// boundary exists, no caller can bind to it — set_partition refuses '' —
	// so those rows become unreachable silently, with no error anywhere,
	// because the policy is doing exactly what it says.
	//
	// Read inside the locked tx so the verdict is authoritative. It is the
	// same kind of question as the drift checks: something true of the live
	// DATA that the declaration cannot see. The operator assigns the rows a
	// tenant, deletes them, or sets the override knowingly.
	if os.Getenv("ATLANTIS_ALLOW_UNREACHABLE_TENANT") != "1" {
		stranded, uerr := detectUnreachableTenantRows(ctx, tx, d, newIR)
		if uerr != nil {
			return nil, fmt.Errorf("apply: unreachable-tenant check failed: %w", uerr)
		}
		if len(stranded) > 0 {
			return nil, unreachableTenantError(stranded)
		}
	}

	// Refuse a declaration whose expiry the platform cannot perform.
	//
	// `partition by` + `ttl_field` on a table the sweeper cannot chunk-drop is
	// a promise atlantis will not keep: the sweeper binds no tenant, so its
	// DELETE matches nothing and expired rows accumulate forever. #48 made that
	// state loud with a counter; this makes it unreachable, which is the half a
	// counter cannot do.
	//
	// Refused rather than warned, and with no override env var, because unlike
	// the checks around it there is nothing an operator can knowingly accept —
	// the outcome is not "risky", it is "does not work". The fix is in the
	// schema and the message names all three forms of it.
	//
	// A pure declaration check, so it needs no database read and sits before
	// the ones that do.
	if bad := unexpirableEntities(newIR); len(bad) > 0 {
		return nil, unexpirableExpiryError(bad)
	}

	if err := refuseStaleTypeChanges(ctx, mtx, prior, newIR, d); err != nil {
		return nil, err
	}
	// Rendered before the DDL, so the comparison after it holds the
	// migration's locks for catalog reads, and for rendering again only what
	// referred to a type the DDL creates.
	declared, err := renderForDrift(ctx, mtx, newIR)
	if err != nil {
		return nil, err
	}

	if _, err := mtx.Exec(ctx, scripts.Up); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	// After the DDL and before either commit: the database is compared with
	// the declaration in the state this migration leaves it, and a refusal
	// rolls the DDL back.
	if err := refuseMissingTables(ctx, mtx, newIR); err != nil {
		return nil, err
	}
	if err := refuseUnresolvedDrift(ctx, mtx, declared); err != nil {
		return nil, err
	}

	// The DDL lands before the record of it. See the note on mtx above for
	// why this order and not the other.
	if mtx != tx {
		if err := mtx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("apply: commit to the managed database: %w", err)
		}
	}

	meta := versionMeta{
		Caller:     req.GetCaller(),
		PlanClass:  d.HighestClass().String(),
		Diff:       d,
		UpSQL:      scripts.Up,
		DownSQL:    scripts.Down,
		PlanID:     gotPlanID,
		EventType:  "apply",
		Actor:      req.GetActor(),
		ActorEmail: req.GetActorEmail(),
		ActorName:  req.GetActorName(),

		AppliedUnderPolicy: gate.Policy,
		AppliedVerdict:     gate.Verdict,
	}
	version, err := s.persistCheckpoint(ctx, tx, newIR, meta)
	if err != nil {
		return nil, err
	}

	// Close out this plan, and retire the caller's other outstanding ones.
	//
	// Both run whether or not this apply went through the gate: a plan row
	// exists only when one was required, and the UPDATEs match nothing
	// otherwise. Doing it unconditionally means there is no second condition
	// to get wrong later, when a class moves from ungated to gated.
	//
	// Superseding matters more than it looks. The plans left behind were
	// computed against a checkpoint this apply has just moved, so approving one
	// afterwards would be approving a diff that no longer describes anything —
	// and the gate would then refuse it on the SQL comparison, which reads to
	// the approver as the product malfunctioning rather than as their decision
	// having gone stale.
	if err := markPlanApplied(ctx, tx, gotPlanID, version); err != nil {
		return nil, fmt.Errorf("close out plan %s: %w", gotPlanID, err)
	}

	// Read before superseding, because superseding now depends on it: a plan
	// is only stale if the checkpoint it was computed against has actually
	// moved. An apply that changes no schema — a comment-only edit lowers to
	// an identical IR and an empty diff — leaves the checkpoint where it was,
	// and retiring a pending destructive request on the strength of it was
	// retiring a decision somebody was waiting on for no reason.
	newHash, _ := loadCheckpointHashTx(ctx, tx)

	if err := supersedePlansFor(ctx, tx, req.GetCaller(), gotPlanID, newHash); err != nil {
		return nil, fmt.Errorf("supersede outstanding plans for %s: %w", req.GetCaller(), err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// The verdict this apply consumed is reconciled with what happened —
	// the row that says a rehearsal's promise was kept.
	if gate.Verdict != "" {
		s.markRehearsalOutcome(ctx, gotPlanID, "applied")
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
// DELETE then INSERT rather than ON CONFLICT, so a file dropped from the
// submission is removed from storage too; otherwise it stays as an orphan
// registration.
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
			// Another caller's submission is broken. Reported as an error
			// rather than dropping the file, so a broken caller does not let
			// this caller plan around them.
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

// versionMeta holds the metadata for one schema_versions row. Passed to
// persistCheckpoint so the caller can supply the diff, SQL, plan ID, and
// event type without persistCheckpoint needing to know how they were
// produced.
//
// It carries no compare-and-swap token. There was one, and three of its four
// call sites read the current checkpoint hash inside the transaction and
// handed it straight back as the value to compare against — a check on a
// value against itself, which no drift could fail. The fourth, ApplyMigration,
// is where the real token lives, and it now checks it beside the plan-id check
// rather than here: both ask "did the world move since the plan", and keeping
// them apart is what let one of them decay into a self-comparison unnoticed.
type versionMeta struct {
	Caller    string
	PlanClass string
	Diff      *codegen.Diff
	UpSQL     string
	DownSQL   string
	PlanID    string
	EventType string // "apply", "rollback", "adopt"
	ParentVer *int64

	// Actor is the human the event is attributed to, beside the caller that
	// carried it. Nothing authorizes against it: this server authenticates
	// callers and cannot authenticate people. Empty is a legitimate value and
	// says no human was named, which is what an unattended apply is.
	Actor      string
	ActorEmail string
	ActorName  string

	// AppliedUnderPolicy and AppliedVerdict record what authorized an
	// unattended apply: the tier that let it through and the rehearsal
	// verdict it consumed. Both empty when a human approved — the plan row
	// is that record — and for every event type but "apply".
	AppliedUnderPolicy string
	AppliedVerdict     string

	// Lineage replaces Diff as the source of blame rows when set. adopt is
	// the event that needs it — see seedEntityLineage.
	Lineage *lineageSeed
}

func (s *Service) persistCheckpoint(ctx context.Context, tx pgx.Tx, ir *dsl.IR, meta versionMeta) (int64, error) {
	// Every IR reaching the checkpoint is numbered here rather than by its
	// caller. adopt numbers the declaration and persists the introspection,
	// which arrives with every field at 0; fields sharing a number build no
	// descriptor, and a server loading that checkpoint serves no entity.
	//
	// prior is the row the upsert below replaces, which is the IR the other
	// callers already number against — so this repeats their result rather
	// than changing it.
	prior, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("load prior checkpoint: %w", err)
	}
	codegen.AssignProtoNumbers(prior, ir)

	raw, err := ir.EncodeJSON()
	if err != nil {
		return 0, err
	}

	h := sha256.Sum256(raw)
	irHash := hex.EncodeToString(h[:])

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
		// Marshalled from an empty Diff rather than written out as a literal.
		//
		// The literal here named three buckets and codegen.Diff has had four
		// since park-and-reap, so the shape this wrote for a no-diff event had
		// quietly stopped matching the shape the branch above writes for every
		// other event. Nothing failed, because every reader treats a missing
		// bucket as empty — which is why it went unnoticed, and why the fix is
		// to stop hand-writing the shape rather than to add the fourth key.
		diffJSON, err = json.Marshal(&codegen.Diff{})
		if err != nil {
			return 0, fmt.Errorf("marshal empty diff: %w", err)
		}
	}

	var version int64
	err = tx.QueryRow(ctx, `
INSERT INTO atlantis.schema_versions
    (caller, plan_class, diff, up_sql, down_sql, ir_snapshot, ir_hash, plan_id, parent_version, event_type, actor, actor_email, actor_name, applied_under_policy, applied_verdict)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING version`,
		meta.Caller, meta.PlanClass, diffJSON, meta.UpSQL, meta.DownSQL,
		raw, irHash, meta.PlanID, meta.ParentVer, meta.EventType,
		normalizeActor(meta.Actor), meta.ActorEmail, meta.ActorName,
		meta.AppliedUnderPolicy, meta.AppliedVerdict,
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("insert schema_versions: %w", err)
	}

	// An event carrying a baseline names its own rows; everything else reads
	// them off the diff it applied. One or the other, never both: an adopt's
	// diff describes what the database is missing, so running it here as well
	// would file blame against tables the checkpoint does not contain.
	lineageErr := func() error {
		if meta.Lineage != nil {
			return seedEntityLineage(ctx, tx, version, meta.Lineage.IR, meta.Lineage.Ownership)
		}
		return updateEntityLineage(ctx, tx, version, meta.Caller, meta.Diff)
	}()
	if err := lineageErr; err != nil {
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

// filesHash identifies one submitted file set by what is in it.
//
// Each file contributes sha256(path) and sha256(content) — fixed 32-byte
// fields, so no path or content can be crafted to run into the next one and
// produce the digest of a different file set. Sorted by path, because the
// order files arrive in is not part of what was submitted.
func filesHash(files []SubmittedFile) string {
	sorted := make([]SubmittedFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return string(sorted[i].Content) < string(sorted[j].Content)
	})

	h := sha256.New()
	for _, f := range sorted {
		p := sha256.Sum256([]byte(f.Path))
		c := sha256.Sum256(f.Content)
		h.Write(p[:])
		h.Write(c[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// computePlanID hashes (caller, files, dependency hash) so applies can detect
// drift since planning. Stable across reruns of the same plan.
//
// It hashes file CONTENTS, not paths. An approval is recorded against a plan
// id, so a path-only id would let a caller approved for "drop
// users.legacy_flag" submit different content at the same paths, against the
// same checkpoint, match the same id, and inherit the approval. The gate would
// have been passed by a change nobody looked at.
//
// depHash is callerDependencyHash's output, not the whole prior checkpoint.
// Hashing the checkpoint moves the plan id whenever any caller applies
// anything, so the id-mismatch refusal fires before the CAS runs. Both
// staleness guards are fed the same token so they cannot disagree about what
// counts as a change.
func computePlanID(caller string, files []SubmittedFile, depHash string) string {
	h := sha256.New()
	h.Write([]byte(caller))
	h.Write([]byte{0})
	h.Write([]byte(filesHash(files)))
	h.Write([]byte{0})
	h.Write([]byte(depHash))
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
	case codegen.ClassDestructive:
		return ClassDestructive
	}
	// The fall-through means "the DSL did not compile", so a class landing here
	// is reported to the caller as a parse failure. ClassDestructive had no arm
	// and fell here: `tide apply` printed "plan is unparseable — custom-query
	// SQL validation failed" with both error lists empty, for a plan that had
	// parsed fine. There was no CLI path that could apply a destructive change,
	// and the message named a subsystem that was not involved.
	//
	// TestEveryCodegenClassReachesADistinctPlanClass loops over every
	// codegen.ChangeClass so a future class cannot arrive here silently.
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
	for _, ch := range d.Destructive {
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

	// The entity IDs declared in each other caller's files, which is what
	// decides whether a caller is affected by the diff.
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
