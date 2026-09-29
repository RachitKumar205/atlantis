/**
 * Typed API client for the atlantis console BFF.
 *
 * All endpoints return typed responses or throw ApiError on non-2xx.
 * Query factories are compatible with TanStack Query v5 queryOptions().
 */

// ---------------------------------------------------------------------------
// Error type
// ---------------------------------------------------------------------------

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    // The server's machine-readable reason, where it sends one. Present only
    // for refusals a caller is expected to act on rather than print.
    public readonly code?: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

// TLS_REQUIRED marks the database that offers no TLS. The request carries
// allow_insecure and is sent again.
export const TLS_REQUIRED = 'tls_required'

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

export type UserRole = 'admin' | 'developer' | 'viewer'

// Who is signed in, as asserted by Atlantis Cloud and copied onto the session.
//
// There is no local account behind this. `subject` is Cloud's identifier and
// is what audit rows record; `org` is the organisation the session is acting
// in. Both are fixed for the life of the session — a change at Cloud takes
// effect at the next sign-in.
export interface MeResult {
  subject: string
  org: string
  email: string
  role: UserRole
  name: string

  // Whether this organisation has already answered the onboarding flow.
  //
  // On the organisation, not the browser: the flow offers to read an existing
  // database once, and a record kept in this browser asks the second admin
  // again. A server that cannot read the column answers true, so a database
  // problem shows the dialog to nobody rather than to everybody.
  onboarded: boolean

  // Where to send the browser to present a second factor before a destructive
  // action. Built by the server from its own CLOUD_ISSUER and the session's
  // org, so the page never assembles a URL or decides which organisation it is
  // asking about.
  step_up_url: string

  // False only on a local console started with CONSOLE_DEV_SKIP_STEP_UP, where
  // confirming an admin action asks for no second factor.
  step_up: boolean

  // Cloud itself. Following it keeps both sessions, so coming back is one
  // click.
  cloud_url: string

  // Where sign-out goes after this console drops its own session. Clearing the
  // console cookie ends nothing at Cloud, which is a separate origin.
  cloud_signout_url: string

  // What the current organisation calls itself, when that is not its name.
  //
  // Absent when the two are the same, so the rail draws one line rather than
  // the same word twice. Reaches here from the assertion's org_names claim by
  // way of the session — no call to Cloud per page.
  org_display_name?: string

  // Every organisation this account belongs to, each with the URL that
  // switches to it, including the one currently in use.
  //
  // A snapshot taken when the session opened — a membership revoked since then
  // still appears here. That is safe because the URL goes to Cloud, which
  // re-reads the membership and refuses: the list decides what to draw, never
  // what is allowed. Nothing here may gate anything.
  //
  // Empty for a session opened before this existed, which renders no switcher.
  orgs: OrgTarget[]
}

// What the SPA reads before it holds a session.
export interface ConfigResult {
  // Cloud's sign-in address, built by the server from its own CLOUD_ISSUER.
  // The page assembles no part of it, as with step_up_url.
  cloud_signin_url: string
}

export interface OrgTarget {
  name: string
  url: string

  // Present only where it differs from name. The switcher falls back to name,
  // so an entry without one draws as it always did.
  display_name?: string
}

export interface SubmittedFile {
  path: string
  content: string
}

export interface MergedSchemaResponse {
  version: string
  files: SubmittedFile[]
}

export interface CanonicalIRResponse {
  ir: unknown
  content_hash: string
}

export interface SchemaVersionSummary {
  version: number
  caller: string
  plan_class: string
  event_type: string
  change_count: number
  created_at: string
  ir_hash: string
  actor?: string
  actor_email?: string
  actor_name?: string
  entity_count?: number
  /** The tier that let an unattended apply through, and the rehearsal
   * verdict it consumed. Both absent for human-approved applies. */
  applied_under_policy?: string
  applied_verdict?: string
}

export interface SchemaHistoryResponse {
  versions: SchemaVersionSummary[]
  has_more: boolean
}

export interface SchemaVersionDetail {
  version: number
  caller: string
  plan_class: string
  event_type: string
  diff: DiffPayload
  up_sql: string
  down_sql: string
  ir_snapshot: unknown
  created_at: string
  parent_version?: number
  ir_hash: string
}

export interface DiffChange {
  entity_id: string
  field: string
  detail: string
  kind?: string
}

// One array of changes per change class, keyed by the class name the server
// uses — "additive", "backfill_required", "breaking", "destructive".
//
// Keyed rather than a fixed set of three fields. It WAS three fields, and the
// server has emitted a fourth since park-and-reap, so a version whose only
// change dropped a column rendered in the console as no changes at all. A
// record type means a class the console has not been taught about still
// arrives, still counts, and renders under its own name instead of vanishing.
//
// CHANGE_CLASS_DISPLAY (pages/History.tsx) gives the known classes their order
// and badge; anything outside it falls back to a plain badge rather than being
// dropped.
export type DiffPayload = Record<string, DiffChange[]>

// Keeps every array-valued key, rather than copying named ones across.
//
// Lives here, beside the type it produces, rather than in the page that renders
// it. It was a local helper in History.tsx that named three buckets and
// silently dropped "destructive", which the server has emitted since
// park-and-reap: a version whose only change dropped a column arrived with its
// changes intact and rendered as an empty diff.
//
// Reading the keys off the payload means a class the console does not know
// about is still counted and still listed, under whatever name the server gave
// it. Non-array values are dropped so an unexpected scalar key cannot become a
// bucket full of junk.
export function parseRawDiff(raw: unknown): DiffPayload {
  if (!raw || typeof raw !== 'object') return {}
  const out: DiffPayload = {}
  for (const [bucket, changes] of Object.entries(raw as Record<string, unknown>)) {
    if (Array.isArray(changes)) out[bucket] = changes as DiffChange[]
  }
  return out
}

// Buckets in display order: the known classes first, in severity order, then
// anything the server sent that this console predates. Sorting the unknowns
// last keeps the familiar ones where the eye expects them.
export function orderedBuckets(
  diff: DiffPayload,
): { bucket: string; glyph: string; badge: string }[] {
  const known = new Set(CHANGE_CLASS_DISPLAY.map(c => c.bucket))
  return [
    ...CHANGE_CLASS_DISPLAY.filter(c => (diff[c.bucket]?.length ?? 0) > 0),
    ...Object.keys(diff)
      .filter(b => !known.has(b) && diff[b].length > 0)
      .sort()
      .map(bucket => ({ bucket, glyph: '?', badge: 'plain' })),
  ]
}

// The order the console lists change classes in, least to most severe, with
// the badge modifier each one carries. Kept beside the type it describes so a
// new class is one edit, not a hunt through three pages.
export const CHANGE_CLASS_DISPLAY: ReadonlyArray<{
  bucket: string
  glyph: string
  badge: string
}> = [
  { bucket: 'additive', glyph: '+', badge: 'add' },
  { bucket: 'backfill_required', glyph: '~', badge: 'back' },
  { bucket: 'breaking', glyph: '!', badge: 'break' },
  { bucket: 'destructive', glyph: '✗', badge: 'destroy' },
]

// Plan classes arrive HYPHENATED, and the lookup tables below are keyed with
// underscores. Normalising at lookup is what bridges them.
//
// Every plan_class the BFF serves is proxied from the admin server, and every
// one of those is written by codegen.ChangeClass.String(): "additive",
// "backfill-required", "cross-caller-breaking", "destructive". The pages used
// to compare against the underscored spelling with ===, so every breaking and
// backfill-required version in the history timeline rendered with the grey
// "plain" badge — the styling that means nothing notable happened. Nothing
// failed loudly, because the label beside the badge is the class name itself:
// the text was right and only the colour was wrong.
//
// The underscored spelling was real until 2026-08-13, when the schema-edit
// preview — the one response the BFF assembled itself, via shortPlanClass —
// was removed with the GitHub flow. Accepting both is kept deliberately: these
// strings cross a process boundary this code does not control, the
// schema_versions column holds rows in the hyphenated form indefinitely, and a
// lookup that silently returns nothing is the failure this whole comment is
// about.
function normalizeClass(planClass: string): string {
  return planClass.replace(/-/g, '_')
}

export function planClassBadge(planClass: string): string {
  return PLAN_CLASS_BADGE[normalizeClass(planClass)] ?? 'plain'
}

// planClassLabel turns the proto enum name into the spelling every other
// surface uses: PLAN_CLASS_CROSS_CALLER_BREAKING -> cross-caller-breaking.
//
// Here rather than in a page for the same reason planClassBadge is: the pages
// that show a class must show the same one. It also has to run BEFORE
// planClassBadge on any value that came off the wire as an enum name, because
// the badge table is keyed without the prefix and an unstripped name misses it
// — falling back to the grey "plain" badge, which is the styling that means
// nothing notable happened. That is the exact failure the comment above
// describes, and a destructive change is the value it would hide.
export function planClassLabel(planClass: string): string {
  return planClass.replace(/^PLAN_CLASS_/, '').toLowerCase().replace(/_/g, '-')
}

// plan_class values mapped to badge modifiers. Keyed in the underscored
// spelling; reach it through planClassBadge, which accepts either.
//
// A separate map from CHANGE_CLASS_DISPLAY because this keys on the PLAN's
// class — one value for the whole version — while that one keys on the bucket
// an individual change sits in. The names differ: a plan is
// "cross_caller_breaking" where its changes are in "breaking".
export const PLAN_CLASS_BADGE: Readonly<Record<string, string>> = {
  additive: 'add',
  backfill_required: 'back',
  cross_caller_breaking: 'break',
  destructive: 'destroy',
  unparseable: 'break',
}

// PLAN_CLASS_PREVIEW lived here — the one-line verdict rendered above a schema
// preview on the Schema page. That page no longer previews anything: the
// console does not author schema, so it has nothing to plan.
//
// Deleted rather than kept for step 5's Approvals page, which will want
// something like it. A five-entry lookup is cheap to write when there is a
// consumer, and an exported table with tests and no caller is the shape that
// reads as covered while proving nothing.

export interface DiffVersionsResponse {
  from_version: number
  to_version: number
  diff: DiffPayload
  from_ir?: unknown
  to_ir?: unknown
}

/**
 * Who and when, for one lineage event.
 *
 * `caller` is the machine identity the change arrived under and the server
 * verified it. `actor` is the human recorded beside it and nothing verifies
 * that — empty means none was named, which is what an unattended apply is.
 *
 * Optional because protojson omits a nil message even under EmitDefaultValues.
 */
export interface Blame {
  caller: string
  version: number
  /** RFC3339. */
  at: string
  actor: string
  actor_email: string
  /** The name as it stood when the event was recorded. */
  actor_name: string
}

export interface EntityLineageEntry {
  entity_id: string
  field_name: string
  introduced_by: string
  introduced_at: number
  last_modified_by: string
  last_modified_at: number
  removed_at?: number
  introduced?: Blame
  last_modified?: Blame
}

export interface EntityLineageResponse {
  entries: EntityLineageEntry[]
}

export interface EntityOwnerEntry {
  entity_id: string
  introduced_by: string
  introduced_at: number
  field_count: number
  introduced?: Blame
  last_modified?: Blame
}

export interface EntityOwnersResponse {
  owners: EntityOwnerEntry[]
}

// ---------------------------------------------------------------------------
// Parked objects — what a destructive migration renamed out of the way instead
// of dropping, and how long it stays recoverable.
// ---------------------------------------------------------------------------

export interface ParkedObject {
  /** int64, so protojson sends it as a string — see MergedSchemaResponse.version. */
  id: string
  /** "table" or "column". */
  kind: string
  /** Where the object lives now: the tombstone schema for a table. */
  schema_name: string
  object_name: string
  /** The table holding it, for a column. */
  parent_table: string
  /** Where it came from, and where a restore puts it back. */
  original_schema: string
  original_name: string
  parked_at: string
  /** RFC3339. After this instant the reaper may drop it. */
  reap_after: string
  /** Set once dropped. A row with this set is an audit record, not recoverable. */
  reaped_at: string
  /** Failed reap attempts, and why the last one failed. */
  attempts: number
  last_error: string
  next_attempt_after: string
}

/** One generated declaration, from reading an existing database. */
export interface ImportedEntity {
  /** The physical table it describes, schema-qualified. */
  table: string
  /** The proposed entity name. A proposal: renaming it later is breaking. */
  entity: string
  /** The namespace it was read into, which is the Postgres schema's name. */
  namespace: string
  /** The declaration, ready to review and commit. */
  atl: string
}

/** One change worth making to a discovered table. */
export interface ImportSuggestion {
  entity: string
  table: string
  /** tenant-isolation, no-primary-key or unindexed-foreign-key. */
  kind: string
  detail: string
  /** The .atl to add, empty where the remedy is not one line. */
  line: string
}

/** What POST /api/schema/import answers: the identifier of what it stored.
 *
 * The declarations are read back from the import's own URL, so the review is a
 * screen somebody can reload, link to and come back to. */
export interface SchemaImportStarted {
  import_id: string
}

/** One namespace an import read into, and how many tables it found there. */
export interface ImportNamespace {
  name: string
  tables: number
}

/** An import's header and its counts, without the payload behind them.
 *
 * The declarations and the findings run to hundreds of kilobytes together, so
 * the screen draws its header from this and fetches each section on its own. */
export interface SchemaImportOverview {
  import_id: string
  /** Host and port. The connection string is never returned. */
  source: string
  actor: string
  created_at: string
  entities: number
  namespaces: ImportNamespace[]
  suggestions: number
  skipped: number
  warnings: number
}

/** What planning an import's declarations would do.
 *
 * class is the plan class — additive, backfill_required, cross_caller_breaking,
 * destructive — and decides how the confirmation reads. up_sql is the DDL. */
export interface ImportPlan {
  plan_id?: string
  class?: string
  up_sql?: string
  down_sql?: string
  parse_errors?: string[]
  breaking_detail?: string[]
  custom_sql_errors?: string[]
  checkpoint_hash?: string
}

/** One difference between the declarations and the managed database.
 *
 * Field names mirror atlantis.admin.v1.AdoptDriftItem: entity_id and field are
 * what say which column a row is about, and a report without them is a list of
 * identical verbs. */
export interface AdoptDrift {
  entity_id?: string
  field?: string
  kind?: string
  /** "addition", "removal" or "mismatch". */
  severity?: string
  detail?: string
}

export interface ImportApplied {
  checkpoint_written?: boolean
  already_adopted?: boolean
  drift?: AdoptDrift[]
  warnings?: string[]
}

/** What a pass found, without the declarations. */
export interface SchemaImportNotes {
  suggestions: ImportSuggestion[]
  /** Tables that were found and not declared, each with a reason. */
  skipped: string[]
  /** Facts introspection did not verify, and indexes it could not spell. */
  warnings: string[]
}

/** A stored import, without its declarations. */
export interface SchemaImportSummary {
  import_id: string
  source: string
  entities: number
  actor: string
  created_at: string
}

export interface ParkedObjectsResponse {
  objects: ParkedObject[]
  /** True when the server withheld rows, so the page can say so. */
  has_more: boolean
}

// ---------------------------------------------------------------------------
// Schema editing types
// ---------------------------------------------------------------------------

// The schema-edit and caller→repo types lived here.
//
// They described a flow where the console composed a field edit against a
// caller's .atl source and opened a GitHub pull request with the result, with
// console.caller_repos holding each caller's owner/repo/branch. Both are gone:
// schema files live in the customer's own git repo and reach the database
// through `tide apply`. The console observes, approves and audits.

// ---------------------------------------------------------------------------
// Callers types
// ---------------------------------------------------------------------------

export interface CallerInfo {
  caller: string
  file_count: number
  last_applied_at?: string
  schema_version?: number
  registered: boolean
  can_mutate: boolean
  cert_expires_at?: string // RFC3339; absent when no cert has been issued through the console
  /** Stored apply-policy tier; absent or '' means the server default. */
  apply_policy?: string
  /** The tier the gate uses: resolved against the default and clamped to the
   * deployment floor. */
  effective_apply_policy?: string
  /** RFC3339 when the caller is revoked; '' or absent when live. */
  revoked_at?: string
}

/** One protected entity: an approval floor keyed by pattern. */
export interface ProtectedEntity {
  /** Exact `namespace.Entity`, or `namespace.*`. */
  pattern: string
  /** 'require_approval' | 'admin_only'. */
  floor: string
  reason?: string
  created_by?: string
  created_at?: string
}

/** One freeze window: an absolute interval during which matching classes do
 * not apply. Approving stays possible; the apply queues behind the window. */
export interface FreezeWindow {
  id: number
  starts_at: string
  ends_at: string
  display_tz?: string
  /** codegen class names; empty freezes every class. */
  classes?: string[]
  reason?: string
  created_by?: string
  created_at?: string
}

export interface ApplyPolicyResponse {
  caller: string
  apply_policy?: string
  effective_apply_policy: string
  /** Deployment floor; absent when none is set. */
  floor?: string
  /** Whether this caller may rehearse — run migrations against a clone of
   * the real data. A deliberate grant beside the tier. */
  rehearsal_enabled?: boolean
}

export interface RehearsalResult {
  rehearsal_id: string
  verdict: string
  reason?: string
  sqlstate?: string
  error?: string
  diagnostics?: Record<string, number>
  remediation?: string
  clone_ms?: number
  execute_ms?: number
}

export interface RehearsalSummary {
  rehearsal_id: string
  caller: string
  files_hash: string
  verdict: string
  reason?: string
  sqlstate?: string
  error?: string
  /** '' | applied | apply_failed — what happened when the verdict was consumed. */
  outcome?: string
  created_at: string
  expires_at: string
  clone_ms?: number
  execute_ms?: number
}

export interface RegisterCallerResponse {
  caller: string
  can_mutate: boolean
}

export interface GetCallersResponse {
  callers: CallerInfo[]
}

/** One class's rule in the deployment's change policy.
 *
 * `change_class` is the proto enum's name, which is also how the server stores
 * it — the console never translates it to a number, so a class this build has
 * never heard of round-trips unchanged rather than becoming 0.
 */
export interface ChangePolicyEntry {
  change_class: string
  require_approval: boolean
  approver_role: string
  updated_at?: string
  updated_by?: string
}

export interface ChangePolicyResponse {
  entries: ChangePolicyEntry[]
}

/** One change waiting on, or already carrying, a decision. */
export interface SchemaPlanSummary {
  plan_id: string
  caller: string
  change_class: string
  state: string
  requested_by: string
  created_at: string
  expires_at?: string
  decided_by?: string
  decided_by_role?: string
  decided_at?: string
  decision_reason?: string
  /** The role the class requires, resolved from the policy as it stands now. */
  approver_role: string
  expired?: boolean
  /** The human attributed to the apply that filed this plan. Untrusted, and
   * empty for an unattended pipeline — the "unattributed request" case. */
  requested_by_actor?: string
  /** '' | 'override' | 'self_approval_override'. */
  decided_via?: string
  /** The latest rehearsal of this exact content, when one ran. */
  rehearsal_id?: string
  rehearsal_verdict?: string
  /**
   * The entities this plan touches, derived server-side from the stored diff.
   *
   * The BFF marshals with EmitDefaultValues, so this arrives as `[]` rather
   * than absent when the server derived nothing. Every real plan touches at
   * least one entity — a diff with no changes never becomes a plan — so an
   * empty list means the server could not read that plan's stored diff, and
   * the plan is simply not filed under any entity. It still appears in the
   * approval queue, which is the surface that must not lose it.
   *
   * Optional in the type anyway: this crosses a process boundary, and code
   * that assumes a field is always there is how a rename becomes a crash.
   */
  entity_ids?: string[]
}

export interface SchemaPlansResponse {
  plans: SchemaPlanSummary[]
}

/**
 * Group plans by the entities they touch, for the Schema page's per-entity
 * pending strip.
 *
 * One plan lands under every entity it changes, so a plan that renames a field
 * on two entities appears on both — the approval is on the plan, and an
 * operator looking at either entity has to see the same one waiting.
 *
 * Here rather than inside the component so it can be tested: the console has
 * vitest but no DOM harness, and this is the part that can be wrong in a way
 * nobody sees. A bug here does not throw — it renders an entity as having
 * nothing pending, which looks exactly like an entity that has nothing pending.
 */
export function indexPlansByEntity(
  plans: readonly SchemaPlanSummary[] | undefined,
): Map<string, SchemaPlanSummary[]> {
  const idx = new Map<string, SchemaPlanSummary[]>()
  for (const p of plans ?? []) {
    for (const id of p.entity_ids ?? []) {
      const at = idx.get(id)
      if (at) at.push(p)
      else idx.set(id, [p])
    }
  }
  return idx
}

export interface SchemaPlanDetail {
  summary: SchemaPlanSummary
  /** The proposed .atl source, stored as text so it can be shown as written. */
  files: { path: string; content: string }[]
  up_sql?: string
  down_sql?: string
  base_checkpoint_hash?: string
}

export interface SchemaPlanResponse {
  plan: SchemaPlanDetail
}

export interface CallerAliasesResponse {
  caller: string
  aliases: string[]
}

export interface RevokeCallerResponse {
  files_removed: number
}

export interface RestoreCallerResponse {
  restored: boolean
}

/** A single-use enrolment token, to be carried to the machine that will hold
 * the certificate.
 *
 * There is no key here, and no certificate. The console used to generate the
 * private key, hand it to this page, and offer it for download — so it crossed
 * the network, sat in this tab's memory and landed in a Downloads folder, for
 * no reason: the signer has only ever wanted a CSR. The machine generates its
 * own key now and this token is the only thing that travels.
 */
/** One workload-identity binding: which CI runs may obtain this caller's
 * certificate. */
export interface FederationRule {
  id: number
  issuer_url: string
  audience: string
  subject_pattern: string
  renewal_budget: number
  created_by: string
  created_at: string
}

export interface EnrollTokenResponse {
  token: string
  caller: string
  org: string
  expires_at: string

  /** Where the machine redeems this token.
   *
   * The console cannot derive it — the enrolment listener's bind address says
   * nothing about how anything outside reaches it, and reading the Host header
   * would mean trusting an attacker-controlled value on a page that prints a
   * live credential. It comes from CONSOLE_ENROLL_PUBLIC_URL, which the console
   * refuses to start without once enrolment is on.
   */
  enroll_url: string
}

/** What this console has enrolled, per caller.
 *
 * The console's own record, not atlantis's. atlantis binds a caller to one
 * certificate by fingerprint and does not publish that fingerprint, so an
 * absent entry means "this console did not enrol it" and never "this caller has
 * no certificate".
 */
export interface CallerCertInfo {
  caller: string
  fingerprint: string
  issued_at: string
  expires_at: string
}

export interface CallerCertsResponse {
  certs: CallerCertInfo[]
  /** False when no signer is configured, which is when the enrol button should
   * say so rather than fail on being pressed. */
  enrolment_enabled: boolean
}

// ---------------------------------------------------------------------------
// Jobs types
// ---------------------------------------------------------------------------

// Mirrors atlantis.admin.v1.JobStatus. The field names here had drifted from
// the proto — payload/max_attempts/created_at against the wire's
// args/max_retries/enqueued_at — so the console read `undefined` for each and
// rendered em-dashes that looked like real empty values.
export interface JobStatus {
  job_id: string
  job_name: string
  queue: string
  /** Typed args from the DSL `args { ... }` block, as JSON bytes → base64. */
  args: string
  status: string
  attempts: number
  max_retries: number
  last_error: string
  last_error_at: string
  scheduled_for: string
  started_at: string
  completed_at: string
  enqueued_at: string
  submitted_by: string
  /** -1 when the handler has never checkpointed, distinguishing 0% from unknown. */
  progress_pct: number
  progress_msg: string
  progress_at: string
}

export interface GetJobStatusResponse {
  found: boolean
  job?: JobStatus
}

export interface ListDeadJobsResponse {
  jobs: JobStatus[]
}

export interface RetryDeadJobResponse {
  job_id: string
}

// ---------------------------------------------------------------------------
// Audit log types
// ---------------------------------------------------------------------------

export interface AuditEntry {
  id: number
  user_id: number
  user_email: string
  action: string
  detail?: unknown
  created_at: string
}

export interface AuditLogResponse {
  entries: AuditEntry[]
}

export interface HealthCheck {
  name: string
  status: 'healthy' | 'unhealthy' | 'unknown'
  message?: string
  last_check?: string
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error'

export interface LogRecord {
  seq: number
  time: string
  level: LogLevel
  msg: string
  attrs: Record<string, string>
}

export interface LogsResponse {
  records: LogRecord[]
  last_seq: number
}

export interface HealthResponse {
  atlantis: {
    status: 'healthy' | 'unhealthy' | 'degraded'
    checks: HealthCheck[]
    readyz_code?: number
    healthz_code?: number
    started_at?: string     // RFC3339 — atlantis process boot
    server_version?: string // linker-stamped main.version
    schema_version?: number // latest applied schema version
    metrics_series?: number // non-comment lines in /metrics
  }
}

// ── Worker dispatcher (PR 3) ────────────────────────────────────────

export interface WorkerSessionSummary {
  session_id: string
  caller: string
  queue: string
  pod_id?: string
  sdk_version?: string
  connected_at: string       // RFC3339
  last_heartbeat_at: string  // RFC3339
  max_in_flight: number
  inflight_count: number
  dispatched: number
  completed: number
  failed: number
  revoked: number
  drained?: boolean
}

export interface WorkerInflightRow {
  job_id: number
  job_name: string
  dispatched_at: string
  ack_received: boolean
}

export interface WorkerSessionEvent {
  at: string
  kind: string
  job_id?: number
  job_name?: string
  note?: string
}

export interface WorkerSessionDetail extends WorkerSessionSummary {
  job_names: string[]
  inflight: WorkerInflightRow[]
  events: WorkerSessionEvent[]
}

export interface ListWorkersResponse {
  sessions: WorkerSessionSummary[]
}

export interface GetWorkerResponse {
  session: WorkerSessionDetail
}

// ---------------------------------------------------------------------------
// Core fetch helper
// ---------------------------------------------------------------------------

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(init?.headers ?? {}),
    },
    ...init,
  })

  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    let code: string | undefined
    try {
      const body = await res.json() as { error?: string; message?: string; code?: string }
      msg = body.error ?? body.message ?? msg
      code = body.code
    } catch {
      // ignore JSON parse failure
    }
    throw new ApiError(res.status, msg, code)
  }

  // 204 No Content
  if (res.status === 204) return undefined as unknown as T

  return res.json() as Promise<T>
}

// ---------------------------------------------------------------------------
// Auth / setup
// ---------------------------------------------------------------------------

export const api = {
  auth: {
    // Trade an assertion issued by Atlantis Cloud for a session cookie.
    //
    // The assertion is spent here and never kept. It is single-use at the
    // server, so holding on to one buys nothing, and a token left in memory is
    // readable by anything else running on the page.
    exchange: (assertion: string): Promise<{ ok: boolean }> =>
      apiFetch('/api/auth/exchange', {
        method: 'POST',
        body: JSON.stringify({ assertion }),
      }),

    logout: (): Promise<void> =>
      apiFetch<void>('/api/auth/logout', { method: 'POST' }),

    me: (): Promise<MeResult> =>
      apiFetch<MeResult>('/api/auth/me'),

    // The organisation has answered the onboarding flow. Sent when the dialog
    // closes however it closes, so a decision either way is a decision.
    onboardingDone: (): Promise<{ ok: boolean }> =>
      apiFetch<{ ok: boolean }>('/api/onboarding/done', { method: 'POST' }),

    // The one call that answers without a session cookie. The URL names no
    // organisation: this console serves several, and a request carrying no
    // session identifies none of them.
    config: (): Promise<ConfigResult> =>
      apiFetch<ConfigResult>('/api/config'),

    // Step-up for destructive actions. Takes a *fresh* assertion, which means
    // returning to Cloud — proving yourself again is the whole point, and the
    // one already spent on sign-in is refused.
    sudo: (assertion: string): Promise<{ ok: boolean; expires_in_seconds: number }> =>
      apiFetch('/api/auth/sudo', {
        method: 'POST',
        body: JSON.stringify({ assertion }),
      }),

    signOutOthers: (): Promise<{ ok: boolean; sessions_removed: number }> =>
      apiFetch('/api/auth/sign-out-others', { method: 'POST' }),

    signOutAll: (): Promise<{ ok: boolean; sessions_removed: number }> =>
      apiFetch('/api/auth/sign-out-all', { method: 'POST' }),
  },

  instance: {
    get: (): Promise<{ endpoint: string }> =>
      apiFetch<{ endpoint: string }>('/api/instance'),
  },

  schema: {
    merged: (sinceVersion?: string): Promise<MergedSchemaResponse> => {
      const qs = sinceVersion ? `?since=${encodeURIComponent(sinceVersion)}` : ''
      return apiFetch<MergedSchemaResponse>(`/api/schema${qs}`)
    },

    canonical: (): Promise<CanonicalIRResponse> =>
      apiFetch<CanonicalIRResponse>('/api/schema/canonical'),

    rollback: (toVersion: number): Promise<{ new_version: number; up_sql: string }> =>
      apiFetch('/api/schema/rollback', {
        method: 'POST',
        body: JSON.stringify({ to_version: toVersion }),
      }),

    previewRollback: (toVersion: number): Promise<{
      target_version: number
      current_version: number
      up_sql: string
      plan_class: string
      change_count: number
    }> =>
      apiFetch('/api/schema/rollback/preview', {
        method: 'POST',
        body: JSON.stringify({ to_version: toVersion }),
      }),
  },

  history: {
    list: (opts?: { before?: number; caller?: string; limit?: number; entityId?: string }): Promise<SchemaHistoryResponse> => {
      const params = new URLSearchParams()
      if (opts?.before) params.set('before', String(opts.before))
      if (opts?.caller) params.set('caller', opts.caller)
      if (opts?.limit) params.set('limit', String(opts.limit))
      if (opts?.entityId) params.set('entity_id', opts.entityId)
      const qs = params.toString() ? `?${params.toString()}` : ''
      return apiFetch<SchemaHistoryResponse>(`/api/history${qs}`)
    },

    version: (version: number): Promise<SchemaVersionDetail> =>
      apiFetch<SchemaVersionDetail>(`/api/history/${version}`),

    diff: (from: number, to: number): Promise<DiffVersionsResponse> =>
      apiFetch<DiffVersionsResponse>(`/api/diff?from=${from}&to=${to}`),
  },

  lineage: {
    entity: (entityId: string): Promise<EntityLineageResponse> =>
      apiFetch<EntityLineageResponse>(`/api/lineage/${encodeURIComponent(entityId)}`),
  },

  parked: {
    list: (includeReaped = false): Promise<ParkedObjectsResponse> =>
      apiFetch<ParkedObjectsResponse>(`/api/parked${includeReaped ? '?all=1' : ''}`),
  },

  schemaImport: {
    /**
     * Reads a database and returns .atl describing it.
     *
     * Naming a database here is asking atlantis to manage it, so the server
     * hands the connection string to the organisation's own atlantis, which
     * seals it. The console stores host and port alone. The server still
     * refuses any address that is not a public host and reads inside a READ
     * ONLY transaction.
     *
     * allowInsecure is the acknowledgement that a server offers no TLS. Never
     * sent unless somebody ticked the box, because the same switch against a
     * production database sends a live password across the internet in clear.
     *
     * Slow by nature — it opens a connection to another database and walks its
     * catalogue — so callers show progress rather than assume it returns
     * promptly.
     */
    run: (dsn: string, allowInsecure: boolean): Promise<SchemaImportStarted> =>
      apiFetch<SchemaImportStarted>('/api/schema/import', {
        method: 'POST',
        body: JSON.stringify({ dsn, allow_insecure: allowInsecure }),
      }),

    /** Imports this organisation has run, newest first. Excludes expired ones. */
    list: (): Promise<{ imports: SchemaImportSummary[] }> =>
      apiFetch<{ imports: SchemaImportSummary[] }>('/api/schema/imports'),

    /** One import's header and counts. Small: no declarations, no findings. */
    get: (id: string): Promise<SchemaImportOverview> =>
      apiFetch<SchemaImportOverview>(`/api/schema/imports/${encodeURIComponent(id)}`),

    /** The declarations, optionally narrowed to one namespace.
     *
     * `header` is the comment a generated file opens with, composed by the
     * server so the text lives beside the code that decided what it says. */
    entities: (
      id: string,
      namespace?: string,
    ): Promise<{ entities: ImportedEntity[]; header: string }> =>
      apiFetch<{ entities: ImportedEntity[]; header: string }>(
        `/api/schema/imports/${encodeURIComponent(id)}/entities` +
          (namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''),
      ),

    /** The suggestions, the skipped tables and the warnings. */
    notes: (id: string): Promise<SchemaImportNotes> =>
      apiFetch<SchemaImportNotes>(`/api/schema/imports/${encodeURIComponent(id)}/notes`),

    /** What applying these declarations would do. Writes nothing.
     *
     * No caller: the server submits as its own identity, because
     * ApplyMigration binds req.caller to the authenticated certificate. */
    plan: (id: string): Promise<ImportPlan> =>
      apiFetch<ImportPlan>(`/api/schema/imports/${encodeURIComponent(id)}/plan`, {
        method: 'POST',
        body: '{}',
      }),

    /**
     * Registers the declarations as the schema atlantis holds.
     *
     * No DDL runs: the tables are already in the managed database, which is
     * what the import read. The server checks the declarations against it and
     * refuses if the two disagree.
     */
    apply: (id: string): Promise<ImportApplied> =>
      apiFetch<ImportApplied>(`/api/schema/imports/${encodeURIComponent(id)}/apply`, {
        method: 'POST',
        body: '{}',
      }),
  },

  owners: {
    all: (): Promise<EntityOwnersResponse> =>
      apiFetch<EntityOwnersResponse>('/api/owners'),
  },

  health: {
    get: (): Promise<HealthResponse> =>
      apiFetch<HealthResponse>('/api/health'),
  },

  logs: {
    since: (since: number): Promise<LogsResponse> =>
      apiFetch<LogsResponse>(`/api/logs?since=${since}`),
  },

  workers: {
    list: (): Promise<ListWorkersResponse> =>
      apiFetch<ListWorkersResponse>('/api/admin/workers'),

    get: (sessionID: string): Promise<GetWorkerResponse> =>
      apiFetch<GetWorkerResponse>(`/api/admin/workers/${encodeURIComponent(sessionID)}`),

    drain: (sessionID: string): Promise<Record<string, never>> =>
      apiFetch(`/api/admin/workers/${encodeURIComponent(sessionID)}/drain`, { method: 'POST' }),

    evict: (sessionID: string): Promise<Record<string, never>> =>
      apiFetch(`/api/admin/workers/${encodeURIComponent(sessionID)}/evict`, { method: 'POST' }),
  },

  plans: {
    list: (state?: string): Promise<SchemaPlansResponse> =>
      apiFetch<SchemaPlansResponse>(`/api/plans${state ? `?state=${encodeURIComponent(state)}` : ''}`),

    get: (id: string): Promise<SchemaPlanResponse> =>
      apiFetch<SchemaPlanResponse>(`/api/plans/${encodeURIComponent(id)}`),

    approve: (id: string, reason: string): Promise<SchemaPlanResponse> =>
      apiFetch<SchemaPlanResponse>(`/api/plans/${encodeURIComponent(id)}/approve`, {
        method: 'POST',
        body: JSON.stringify({ reason }),
      }),

    reject: (id: string, reason: string): Promise<SchemaPlanResponse> =>
      apiFetch<SchemaPlanResponse>(`/api/plans/${encodeURIComponent(id)}/reject`, {
        method: 'POST',
        body: JSON.stringify({ reason }),
      }),

    /** Admin + sudo. Approves past a freeze window, a protected-entity
     * floor, or the self-approval refusal; the reason is the record. */
    override: (id: string, reason: string): Promise<SchemaPlanResponse> =>
      apiFetch<SchemaPlanResponse>(`/api/plans/${encodeURIComponent(id)}/override`, {
        method: 'POST',
        body: JSON.stringify({ reason }),
      }),

    /** Developer or admin. Slow: the server clones the managed database and
     * runs this plan's SQL against the copy. */
    rehearse: (id: string): Promise<RehearsalResult> =>
      apiFetch<RehearsalResult>(`/api/plans/${encodeURIComponent(id)}/rehearse`, {
        method: 'POST',
        body: '{}',
      }),
  },

  rehearsals: {
    list: (caller?: string): Promise<{ rehearsals?: RehearsalSummary[] }> =>
      apiFetch<{ rehearsals?: RehearsalSummary[] }>(
        `/api/rehearsals${caller ? `?caller=${encodeURIComponent(caller)}` : ''}`),
  },

  protection: {
    list: (): Promise<{ entities?: ProtectedEntity[] }> =>
      apiFetch<{ entities?: ProtectedEntity[] }>('/api/protected-entities'),

    /** Admin + sudo. Upserts one pattern. */
    put: (e: { pattern: string; floor: string; reason?: string }): Promise<unknown> =>
      apiFetch<unknown>('/api/protected-entities', {
        method: 'PUT',
        body: JSON.stringify(e),
      }),

    /** Admin + sudo. */
    remove: (pattern: string): Promise<unknown> =>
      apiFetch<unknown>(`/api/protected-entities?pattern=${encodeURIComponent(pattern)}`, {
        method: 'DELETE',
      }),
  },

  freezes: {
    list: (): Promise<{ windows?: FreezeWindow[] }> =>
      apiFetch<{ windows?: FreezeWindow[] }>('/api/freeze-windows'),

    /** Admin + sudo. Times are RFC3339; empty classes freeze everything. */
    create: (w: {
      starts_at: string
      ends_at: string
      display_tz?: string
      classes?: string[]
      reason?: string
    }): Promise<unknown> =>
      apiFetch<unknown>('/api/freeze-windows', {
        method: 'POST',
        body: JSON.stringify(w),
      }),

    /** Admin + sudo. */
    remove: (id: number): Promise<unknown> =>
      apiFetch<unknown>(`/api/freeze-windows/${id}`, { method: 'DELETE' }),
  },

  policy: {
    get: (): Promise<ChangePolicyResponse> =>
      apiFetch<ChangePolicyResponse>('/api/policy'),

    /** Writes the rules it is given and leaves every other class alone.
     *
     * Sending only what changed is deliberate. A full replacement would mean
     * two operators with this page open each save the whole policy, and the
     * second save silently reverts the first one's edit to an unrelated class.
     */
    set: (entries: ChangePolicyEntry[]): Promise<ChangePolicyResponse> =>
      apiFetch<ChangePolicyResponse>('/api/policy', {
        method: 'PUT',
        body: JSON.stringify({ entries }),
      }),
  },

  callers: {
    list: (): Promise<GetCallersResponse> =>
      apiFetch<GetCallersResponse>('/api/callers'),

    register: (caller: string, canMutate: boolean): Promise<RegisterCallerResponse> =>
      apiFetch<RegisterCallerResponse>('/api/callers', {
        method: 'POST',
        body: JSON.stringify({ caller, can_mutate: canMutate }),
      }),

    revoke: (caller: string): Promise<RevokeCallerResponse> =>
      apiFetch<RevokeCallerResponse>(`/api/callers/${encodeURIComponent(caller)}`, {
        method: 'DELETE',
      }),

    /** Clear a revocation. Admin, and sudo — it re-admits a cut-off identity. */
    restore: (caller: string): Promise<RestoreCallerResponse> =>
      apiFetch<RestoreCallerResponse>(`/api/callers/${encodeURIComponent(caller)}/restore`, {
        method: 'POST',
      }),

    /** Mint a single-use enrolment token. Admin, and sudo — this produces a
     * credential that becomes a caller's identity. */
    enroll: (caller: string): Promise<EnrollTokenResponse> =>
      apiFetch<EnrollTokenResponse>(`/api/callers/${encodeURIComponent(caller)}/enroll`, {
        method: 'POST',
      }),

    /** The caller's apply-policy tier: how much may apply without a human. */
    applyPolicy: (caller: string): Promise<ApplyPolicyResponse> =>
      apiFetch<ApplyPolicyResponse>(
        `/api/callers/${encodeURIComponent(caller)}/apply-policy`),

    /** Admin + sudo. Empty policy resets the caller to the server default;
     * always send the current tier beside a rehearsal toggle, or the save
     * resets it. */
    setApplyPolicy: (caller: string, policy: string, rehearsalEnabled?: boolean): Promise<ApplyPolicyResponse> =>
      apiFetch<ApplyPolicyResponse>(
        `/api/callers/${encodeURIComponent(caller)}/apply-policy`, {
          method: 'PUT',
          body: JSON.stringify({
            apply_policy: policy,
            ...(rehearsalEnabled === undefined ? {} : { rehearsal_enabled: rehearsalEnabled }),
          }),
        }),

    /** Whether viewers may self-enrol as this caller through `tide login`. */
    enrollmentPolicy: (caller: string): Promise<{ developers_may_enroll: boolean }> =>
      apiFetch<{ developers_may_enroll: boolean }>(
        `/api/callers/${encodeURIComponent(caller)}/enrollment`),

    /** Admin + sudo: the flag decides who can obtain the caller's identity. */
    setEnrollmentPolicy: (caller: string, allowed: boolean): Promise<void> =>
      apiFetch<void>(`/api/callers/${encodeURIComponent(caller)}/enrollment`, {
        method: 'POST',
        body: JSON.stringify({ developers_may_enroll: allowed }),
      }),

    federationRules: (caller: string): Promise<{ rules: FederationRule[] }> =>
      apiFetch<{ rules: FederationRule[] }>(
        `/api/callers/${encodeURIComponent(caller)}/federation`),

    addFederationRule: (
      caller: string,
      rule: { issuer_url: string; audience: string; subject_pattern: string; renewal_budget?: number },
    ): Promise<{ id: number }> =>
      apiFetch<{ id: number }>(`/api/callers/${encodeURIComponent(caller)}/federation`, {
        method: 'POST',
        body: JSON.stringify(rule),
      }),

    revokeFederationRule: (caller: string, id: number): Promise<void> =>
      apiFetch<void>(`/api/callers/${encodeURIComponent(caller)}/federation/${id}`, {
        method: 'DELETE',
      }),

    certs: (): Promise<CallerCertsResponse> =>
      apiFetch<CallerCertsResponse>('/api/callers/certs'),

    revokeAll: (): Promise<{ ok: boolean; revoked: number; failures: string[] }> =>
      apiFetch('/api/callers/revoke-all', { method: 'POST' }),

    aliases: (caller: string): Promise<CallerAliasesResponse> =>
      apiFetch<CallerAliasesResponse>(`/api/admin/callers/${encodeURIComponent(caller)}/aliases`),

    setAliases: (caller: string, aliases: string[]): Promise<CallerAliasesResponse> =>
      apiFetch<CallerAliasesResponse>(`/api/admin/callers/${encodeURIComponent(caller)}/aliases`, {
        method: 'PUT',
        body: JSON.stringify({ aliases }),
      }),
  },

  // No users API. Membership and roles belong to the organisation, which lives
  // at Cloud; this console reads what an assertion tells it.

  jobs: {
    get: (id: string): Promise<GetJobStatusResponse> =>
      apiFetch<GetJobStatusResponse>(`/api/jobs/${encodeURIComponent(id)}`),

    listDead: (opts?: { limit?: number; job_name?: string }): Promise<ListDeadJobsResponse> => {
      const params = new URLSearchParams()
      if (opts?.limit) params.set('limit', String(opts.limit))
      if (opts?.job_name) params.set('job_name', opts.job_name)
      const qs = params.toString() ? `?${params.toString()}` : ''
      return apiFetch<ListDeadJobsResponse>(`/api/jobs/dead${qs}`)
    },

    retry: (id: string): Promise<RetryDeadJobResponse> =>
      apiFetch<RetryDeadJobResponse>(`/api/jobs/${encodeURIComponent(id)}/retry`, {
        method: 'POST',
      }),
  },

  audit: {
    list: (limit = 100): Promise<AuditLogResponse> =>
      apiFetch<AuditLogResponse>(`/api/audit?limit=${limit}`),
  },

  // ─────────────────────────────────────────────────────────────────
  // Sandbox — browser hits /api/sandbox/*; the BFF rewrites the path
  // and proxies to the in-process runtime at /v1/sandbox/*. Every
  // response carries t_server_us so the workbench can report server-
  // measured time without round-trip noise.
  // ─────────────────────────────────────────────────────────────────
  sandbox: {
    list: (): Promise<SandboxListResponse> =>
      apiFetch<SandboxListResponse>('/api/sandbox'),

    boot: (req: SandboxBootRequest): Promise<SandboxBootResponse> =>
      apiFetch<SandboxBootResponse>('/api/sandbox', {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    destroy: (pubID: string): Promise<void> =>
      apiFetch<void>(`/api/sandbox/${encodeURIComponent(pubID)}`, { method: 'DELETE' }),

    exec: (pubID: string, sql: string, args: unknown[] = []): Promise<SandboxExecResponse> =>
      apiFetch<SandboxExecResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/sql/exec`, {
        method: 'POST',
        body: JSON.stringify({ sql, args }),
      }),

    query: (pubID: string, sql: string, args: unknown[] = []): Promise<SandboxQueryResponse> =>
      apiFetch<SandboxQueryResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/sql/query`, {
        method: 'POST',
        body: JSON.stringify({ sql, args }),
      }),

    catalog: (pubID: string): Promise<SandboxCatalogResponse> =>
      apiFetch<SandboxCatalogResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/catalog`,
      ),

    describe: (pubID: string, qualified: string): Promise<SandboxDescribeResponse> =>
      apiFetch<SandboxDescribeResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/describe?q=${encodeURIComponent(qualified)}`,
      ),

    sample: (pubID: string, qualified: string, n = 10): Promise<SandboxRowsResponse> =>
      apiFetch<SandboxRowsResponse>(
        `/api/sandbox/${encodeURIComponent(pubID)}/inspect/sample?q=${encodeURIComponent(qualified)}&n=${n}`,
      ),

    find: (pubID: string, req: SandboxFindRequest): Promise<SandboxRowsResponse> =>
      apiFetch<SandboxRowsResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/inspect/find`, {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    mark: (pubID: string): Promise<SandboxMarkResponse> =>
      apiFetch<SandboxMarkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/mark`, {
        method: 'POST',
        body: '{}',
      }),

    restore: (pubID: string, markID: string): Promise<void> =>
      apiFetch<void>(`/api/sandbox/${encodeURIComponent(pubID)}/restore`, {
        method: 'POST',
        body: JSON.stringify({ mark_id: markID }),
      }),

    bulk: (pubID: string, req: SandboxBulkRequest): Promise<SandboxBulkResponse> =>
      apiFetch<SandboxBulkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/fixtures/bulk`, {
        method: 'POST',
        body: JSON.stringify(req),
      }),

    fork: (pubID: string, n: number): Promise<SandboxForkResponse> =>
      apiFetch<SandboxForkResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/fork`, {
        method: 'POST',
        body: JSON.stringify({ n }),
      }),

    diff: (pubID: string, beforeMarkID: string, afterMarkID: string): Promise<SandboxDiffResponse> =>
      apiFetch<SandboxDiffResponse>(`/api/sandbox/${encodeURIComponent(pubID)}/inspect/diff`, {
        method: 'POST',
        body: JSON.stringify({ before_mark_id: beforeMarkID, after_mark_id: afterMarkID }),
      }),
  },
}

// ─────────────────────────────────────────────────────────────────
// Sandbox wire types
// ─────────────────────────────────────────────────────────────────

export interface SandboxBootRequest {
  backend?: 'sim' | 'embedded'
  determinism?: 'off' | 'strict'
  seed?: number
}

export interface SandboxBootResponse {
  pub_id: string
  backend: string
  boot_ms: number
  schema_version?: string
  entity_count: number
  t_server_us?: number
}

export interface SandboxListEntry {
  pub_id: string
  backend: string
  schema_version?: string
  created_at: string
  last_active: string
  boot_ms: number
}

export interface SandboxListResponse {
  sandboxes: SandboxListEntry[]
}

export interface SandboxExecResponse {
  rows_affected: number
  t_server_us?: number
}

export interface SandboxQueryResponse {
  rows: Array<Record<string, unknown>>
  t_server_us?: number
}

export interface SandboxColumnInfo {
  name: string
  kind: string
  nullable: boolean
}

export interface SandboxCatalogResponse {
  entities: string[]
  t_server_us?: number
}

export interface SandboxDescribeResponse {
  schema: string
  name: string
  qualified: string
  columns: SandboxColumnInfo[]
  primary_key: string[]
  row_count: number
  soft_delete_field?: string
  touch_on_update_field?: string
  partition_field?: string
  time_field?: string
  identity_col?: string
  t_server_us?: number
}

export interface SandboxRowsResponse {
  rows: Array<Record<string, unknown>>
  t_server_us?: number
}

export interface SandboxFindPredicate {
  column: string
  op: '=' | '!=' | '<' | '<=' | '>' | '>=' | 'is null' | 'is not null'
  value?: unknown
}

export interface SandboxFindRequest {
  qualified: string
  predicates: SandboxFindPredicate[]
}

export interface SandboxMarkResponse {
  mark_id: string
  t_server_us?: number
}

export interface SandboxBulkRequest {
  qualified: string
  n: number
  seed?: number
  pk_start?: number
}

export interface SandboxBulkResponse {
  inserted: number
  t_server_us?: number
}

export interface SandboxForkResponse {
  ids: string[]
  backend: string
  t_server_us?: number
}

export interface SandboxTableDiff {
  added: number
  removed: number
  modified: number
}

export interface SandboxDiffResponse {
  tables: Record<string, SandboxTableDiff>
  t_server_us?: number
}

// ---------------------------------------------------------------------------
// TanStack Query factories
// ---------------------------------------------------------------------------

export const queries = {
  // One stored import. Keyed by id so the review screen is reachable by URL and
  // survives a reload; staleTime is Infinity because an import is a snapshot
  // and nothing rewrites it.
  schemaImport: (id: string) => ({
    queryKey: ['schema-import', id] as const,
    queryFn: () => api.schemaImport.get(id),
    staleTime: Infinity,
  }),

  schemaImportEntities: (id: string, namespace?: string) => ({
    queryKey: ['schema-import', id, 'entities', namespace ?? ''] as const,
    queryFn: () => api.schemaImport.entities(id, namespace),
    staleTime: Infinity,
  }),

  schemaImportNotes: (id: string) => ({
    queryKey: ['schema-import', id, 'notes'] as const,
    queryFn: () => api.schemaImport.notes(id),
    staleTime: Infinity,
  }),

  me: () => ({
    queryKey: ['auth', 'me'] as const,
    queryFn: () => api.auth.me(),
    retry: false,
  }),

  schemaMerged: (sinceVersion?: string) => ({
    queryKey: ['schema', 'merged', sinceVersion] as const,
    queryFn: () => api.schema.merged(sinceVersion),
    staleTime: 30_000,
  }),

  schemaCanonical: () => ({
    queryKey: ['schema', 'canonical'] as const,
    queryFn: () => api.schema.canonical(),
    staleTime: 30_000,
  }),

  historyList: (opts?: { before?: number; caller?: string; limit?: number; entityId?: string }) => ({
    queryKey: ['history', 'list', opts] as const,
    queryFn: () => api.history.list(opts),
  }),

  historyVersion: (version: number) => ({
    queryKey: ['history', 'version', version] as const,
    queryFn: () => api.history.version(version),
    enabled: version > 0,
  }),

  diff: (from: number, to: number) => ({
    queryKey: ['diff', from, to] as const,
    queryFn: () => api.history.diff(from, to),
    enabled: from > 0 && to > 0,
  }),

  entityLineage: (entityId: string) => ({
    queryKey: ['lineage', entityId] as const,
    queryFn: () => api.lineage.entity(entityId),
    enabled: !!entityId,
  }),

  parkedObjects: (includeReaped = false) => ({
    queryKey: ['parked', includeReaped] as const,
    queryFn: () => api.parked.list(includeReaped),
    // Keep the previous rows on screen while the toggle refetches; without it
    // the table is replaced by the loading block on every checkbox click.
    placeholderData: (prev: ParkedObjectsResponse | undefined) => prev,
  }),

  entityOwners: () => ({
    queryKey: ['owners', 'all'] as const,
    queryFn: () => api.owners.all(),
  }),

  health: () => ({
    queryKey: ['health'] as const,
    queryFn: () => api.health.get(),
    refetchInterval: 30_000,
  }),


  callers: () => ({
    queryKey: ['callers'] as const,
    queryFn: () => api.callers.list(),
    staleTime: 30_000,
  }),

  callerCerts: () => ({
    queryKey: ['caller-certs'] as const,
    queryFn: () => api.callers.certs(),
    staleTime: 30_000,
  }),

  instance: () => ({
    queryKey: ['instance'] as const,
    queryFn: () => api.instance.get(),
    staleTime: Infinity, // endpoint is process config, doesn't change at runtime
  }),

  deadJobs: (opts?: { limit?: number; job_name?: string }) => ({
    queryKey: ['jobs', 'dead', opts] as const,
    queryFn: () => api.jobs.listDead(opts),
    refetchInterval: 15_000,
  }),

  auditLog: (limit = 100) => ({
    queryKey: ['audit', limit] as const,
    queryFn: () => api.audit.list(limit),
    staleTime: 10_000,
  }),

  sandboxList: () => ({
    queryKey: ['sandbox', 'list'] as const,
    queryFn: () => api.sandbox.list(),
    // Poll cheaply so the list reflects ephemeral lifecycle. List
    // requests do NOT bump server-side TTL (see internal/console/sandbox.go).
    refetchInterval: 5_000,
  }),

  sandboxDescribe: (pubID: string, qualified: string) => ({
    queryKey: ['sandbox', pubID, 'describe', qualified] as const,
    queryFn: () => api.sandbox.describe(pubID, qualified),
    enabled: !!pubID && !!qualified,
  }),

  sandboxCatalog: (pubID: string) => ({
    queryKey: ['sandbox', pubID, 'catalog'] as const,
    queryFn: () => api.sandbox.catalog(pubID),
    enabled: !!pubID,
    // Catalog is built at boot and immutable for the sandbox's lifetime,
    // so cache aggressively — no point re-fetching.
    staleTime: Infinity,
  }),
}
