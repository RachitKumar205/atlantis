package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// AdoptBaselineRequest baselines an already-populated database. The
// server introspects the live schema, diffs it against the declared IR
// the operator submits, and — if the two match — records the IR as the
// authoritative checkpoint without running any DDL.
//
// Submissions is the multi-caller payload `tidectl adopt` sends: one
// entry per caller in atlantis.workspace.yaml, all baselined atomically
// in the same transaction. This is the canonical shape; the single
// Caller+Files pair is preserved as a thin shortcut for tooling that
// only knows about one caller at a time.
//
// AllowDrift bypasses the safety check: if true, the server baselines
// the declared IR even when introspection sees disagreement, and
// records the drift report into atlantis.adopt_history so future
// operators can audit what was lied about.
//
// AdoptedBy is the principal recorded into adopt_history. Filled by
// the CLI from $USER or an explicit --principal flag.
type AdoptBaselineRequest struct {
	Submissions []CallerSubmission `json:",omitempty"`

	// Single-caller shortcut. Server treats a non-empty Caller as a
	// one-element Submissions slice. Ignored when Submissions is set.
	Caller string          `json:",omitempty"`
	Files  []SubmittedFile `json:",omitempty"`

	AllowDrift bool
	AdoptedBy  string
}

// CallerSubmission is one row in a multi-caller adopt payload.
type CallerSubmission struct {
	Caller string
	Files  []SubmittedFile
}

// AdoptDriftItem is one row in the drift report. Mirrors codegen.Change
// across the wire so the CLI can render without importing internal/codegen.
//
// Severity splits the report into three Terraform-style buckets:
//   - "addition": declared by .atl but missing in live DB. Outstanding
//     work for `tide apply`, not a bug. Doesn't block baselining.
//   - "removal":  present in live DB but undeclared. Either an old prod
//     artifact the .atl never picked up, or a real omission. Doesn't
//     block baselining; surfacing it is the value.
//   - "mismatch": both sides have it but they disagree (type changed,
//     NOT NULL flipped, default rewritten). This is real drift and
//     blocks baseline unless --allow-drift.
type AdoptDriftItem struct {
	EntityID string `json:"entity_id"`
	Field    string `json:"field,omitempty"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // "addition" | "removal" | "mismatch"
	Detail   string `json:"detail,omitempty"`
}

// AdoptBaselineResponse is returned by AdoptBaseline. CheckpointWritten
// signals whether the IR checkpoint was actually inserted (false when
// drift was detected and --allow-drift was not set, or when a prior
// identical adopt completed successfully).
//
// Drift always contains the report regardless of whether the checkpoint
// was written — so the CLI can echo it back even on a clean adopt
// (Drift will be empty in that case).
type AdoptBaselineResponse struct {
	CheckpointWritten bool
	AlreadyAdopted    bool // true when a previous adopt with the same file hash succeeded
	Drift             []AdoptDriftItem
	Warnings          []string
}

// AdoptBaseline implements the "verify, then baseline" flow. See
// docs/guides/adopt-an-existing-database.md for the user-facing model.
//
// Multi-caller atomicity: every caller in req.Submissions is parsed
// and lowered into one union IR; introspection diffs that whole union
// in a single transaction. Either every caller baselines or none do.
// FK refs that cross caller namespaces resolve naturally because the
// IR lowering sees the full set.
func (s *Service) AdoptBaseline(ctx context.Context, req *adminpb.AdoptBaselineRequest) (*adminpb.AdoptBaselineResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	subs := callerSubmissionsFromPB(req.GetSubmissions())
	if len(subs) == 0 {
		if req.GetCaller() == "" || len(req.GetFiles()) == 0 {
			return nil, errors.New("admin: at least one CallerSubmission is required")
		}
		subs = []CallerSubmission{{Caller: req.GetCaller(), Files: submittedFilesFromPB(req.GetFiles())}}
	}
	for i, s := range subs {
		if s.Caller == "" {
			return nil, fmt.Errorf("admin: submission[%d]: caller is required", i)
		}
		if len(s.Files) == 0 {
			return nil, fmt.Errorf("admin: submission[%d] %s: at least one .atl file is required", i, s.Caller)
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same advisory lock id as ApplyMigration so adopt + apply can't
	// race.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	// Idempotency: either every caller's last adopt hash matches the new one,
	// or the adopt proceeds. A mixed result proceeds too, being one caller
	// re-done and the rest re-confirmed.
	hashesNow := make(map[string]string, len(subs))
	allMatch := true
	for _, sub := range subs {
		h := hashFiles(sub.Files)
		hashesNow[sub.Caller] = h
		prior, ok, err := lastAdoptHash(ctx, tx, sub.Caller)
		if err != nil {
			return nil, fmt.Errorf("read adopt history for %s: %w", sub.Caller, err)
		}
		if !ok || prior != h {
			allMatch = false
		}
	}
	if allMatch {
		return &adminpb.AdoptBaselineResponse{AlreadyAdopted: true, CheckpointWritten: true}, nil
	}

	// The comparison describes the managed database, not the one atlantis
	// keeps its tables in. Adopt exists to record what a database already has,
	// so reading the wrong one baselines nothing and reports every declared
	// entity as absent — which is what it did before this was split.
	live, releaseLive, err := s.liveTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("adopt: %w", err)
	}
	defer releaseLive()

	cmp, err := s.compareToLive(ctx, tx, live, subs)
	if err != nil {
		return nil, err
	}
	introspectedIR := cmp.IntrospectedIR
	existingIDs, warnings, d, drift := cmp.ExistingIDs, cmp.Warnings, cmp.Diff, cmp.Drift

	// Only mismatches block baselining. Additions / removals are
	// outstanding-work indicators; surfacing them is the value, blocking
	// on them is the bug.
	if mismatchCount(drift) > 0 && !req.GetAllowDrift() {
		return &adminpb.AdoptBaselineResponse{
			CheckpointWritten: false,
			Drift:             adoptDriftToPB(drift),
			Warnings:          warnings,
		}, nil
	}

	// Write each caller's files + one adopt_history row per caller +
	// one rewrite of the (single-row) IR checkpoint.
	for _, sub := range subs {
		if err := s.upsertCallerFiles(ctx, tx, sub.Caller, sub.Files); err != nil {
			return nil, err
		}
		if err := insertAdoptHistory(ctx, tx, sub.Caller, hashesNow[sub.Caller], drift, req.GetAllowDrift(), req.GetAdoptedBy()); err != nil {
			return nil, fmt.Errorf("insert adopt history for %s: %w", sub.Caller, err)
		}
	}
	// The checkpoint records only entities that physically exist in the
	// live DB. Entities declared in .atl but not yet applied are
	// excluded so a subsequent `tide apply` sees them as additive
	// changes and emits the CREATE TABLE statements. Without this
	// filtering, adopt would baseline phantom entities (RPCs reachable
	// but failing at runtime against missing tables).
	//
	// Filtered from the introspected IR, not the declared one. Filtering
	// handles an entity that does not exist; it cannot handle an attribute
	// that does not exist on an entity that does. Baselining the declaration
	// records every such attribute as already present, so the drift adopt just
	// reported becomes work that can never happen: the next `tide plan`
	// compares the declaration against a checkpoint saying the same thing and
	// finds nothing to do.
	//
	// `partition by` is the sharpest case. A table declared with tenant
	// isolation but carrying no policy, baselined as isolated, gets no policy
	// from any later apply, and the dispatcher reads the checkpoint and treats
	// the table as partitioned. Every caller then reads every tenant's rows
	// while the one operator-visible signal — omit the tenant, get refused —
	// keeps reporting healthy.
	//
	// The same shape applied to a declared column that did not exist. Using
	// the introspected IR fixes both at once, and any future attribute
	// introspection learns to verify, because that IR is defined as
	// "declared metadata, with everything the catalog can confirm overwritten
	// by what the catalog actually says".
	//
	// KNOWN LIMIT: introspection does not yet read indexes, uniques or checks
	// back from the catalogue — FromPostgres carries those over from the
	// declaration untouched — so a declared-but-absent index is still
	// baselined as present. That is an introspection gap rather than an adopt
	// one, and closing it here would mean re-deriving the catalogue a second
	// time in the wrong package.
	baseline := filterToExistingEntities(introspectedIR, existingIDs)
	_, err = s.persistCheckpoint(ctx, tx, baseline, versionMeta{
		Caller:    "adopt",
		PlanClass: "adopt",
		Diff:      d,
		EventType: "adopt",
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &adminpb.AdoptBaselineResponse{
		CheckpointWritten: true,
		Drift:             adoptDriftToPB(drift),
		Warnings:          warnings,
	}, nil
}

// loadOtherCallersExcluding loads files for every caller NOT in the
// supplied set. Mirror of loadOtherCallersTx but generalized for the
// multi-caller adopt case.
// Takes a Querier rather than a pgx.Tx because it only reads. That is what
// lets InspectSchema drive this path inside a READ ONLY transaction.
func (s *Service) loadOtherCallersExcluding(ctx context.Context, tx introspect.Querier, exclude map[string]bool) ([]*dsl.File, error) {
	rows, err := tx.Query(ctx, `
SELECT caller, file_path, content
FROM atlantis.caller_registrations
ORDER BY caller, file_path`)
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
		if exclude[caller] {
			continue
		}
		f, err := dsl.Parse(caller+":"+path, []byte(content))
		if err != nil {
			return nil, fmt.Errorf("caller %s: stored file %s no longer parses: %w", caller, path, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// hashFiles returns a stable sha256 over (path, content) tuples for
// dedup-and-idempotency checks. The order is forced so two semantically
// equivalent submissions hash the same.
func hashFiles(files []SubmittedFile) string {
	type fh struct{ Path, ContentHash string }
	tmp := make([]fh, len(files))
	for i, f := range files {
		h := sha256.Sum256(f.Content)
		tmp[i] = fh{Path: f.Path, ContentHash: hex.EncodeToString(h[:])}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].Path < tmp[j].Path })
	h := sha256.New()
	for _, t := range tmp {
		h.Write([]byte(t.Path))
		h.Write([]byte{0})
		h.Write([]byte(t.ContentHash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lastAdoptHash returns the declared-IR hash from the most recent
// adopt for the caller. (false, nil) when no prior adopt exists.
func lastAdoptHash(ctx context.Context, tx pgx.Tx, caller string) (string, bool, error) {
	var h string
	err := tx.QueryRow(ctx, `
SELECT declared_hash
FROM atlantis.adopt_history
WHERE caller = $1
ORDER BY adopted_at DESC
LIMIT 1`, caller).Scan(&h)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return h, true, nil
}

func insertAdoptHistory(ctx context.Context, tx pgx.Tx, caller, hash string, drift []AdoptDriftItem, allowDrift bool, adoptedBy string) error {
	if adoptedBy == "" {
		adoptedBy = "unknown"
	}
	body, err := json.Marshal(drift)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO atlantis.adopt_history (caller, declared_hash, drift_count, drift_report, allow_drift, adopted_by)
VALUES ($1, $2, $3, $4, $5, $6)`,
		caller, hash, len(drift), body, allowDrift, adoptedBy)
	return err
}

// translateDrift converts a codegen.Diff to wire-side AdoptDriftItems
// and categorizes each by severity. The categorization mirrors how
// Terraform talks about a plan: + creates, - destroys, ~ modifies.
// Only the ~ (mismatch) bucket is a real disagreement; the others are
// outstanding work for the operator to apply.
func translateDrift(d *codegen.Diff) []AdoptDriftItem {
	if d == nil || d.IsEmpty() {
		return nil
	}
	var out []AdoptDriftItem
	add := func(changes []codegen.Change) {
		for _, ch := range changes {
			out = append(out, AdoptDriftItem{
				EntityID: ch.EntityID,
				Field:    ch.Field,
				Kind:     string(ch.Kind),
				Severity: classifyDriftSeverity(string(ch.Kind)),
				Detail:   ch.Detail,
			})
		}
	}
	// Every bucket, via All(), so a new class cannot go unreported. Drift that
	// is not listed is drift the operator does not know about.
	add(d.All())
	return out
}

// liveComparison is what it costs to answer "how does this declaration differ
// from the database": parse, lower, introspect, diff, classify.
//
// Shared by AdoptBaseline and InspectSchema, which ask the same question and
// differ only in what they do with the answer — one writes a checkpoint, the
// other returns. Two copies of this sequence would be two places for the diff
// to be computed against a different pair of IRs, and getting that pairing
// wrong is exactly what 3f819ac was: adopt baselined the declaration while
// reporting drift derived from the introspection.
type liveComparison struct {
	DeclaredIR     *dsl.IR
	IntrospectedIR *dsl.IR
	ExistingIDs    map[string]bool
	Diff           *codegen.Diff
	Drift          []AdoptDriftItem
	Warnings       []string
}

// compareToLive parses the submissions, lowers them alongside every other
// registered caller so cross-caller FKs resolve, introspects the live database
// and returns the classified difference.
//
// Takes Queriers rather than a Tx so a caller can hand it a READ ONLY
// transaction. InspectSchema does exactly that, which is what makes "inspect
// writes nothing" a property Postgres enforces rather than one a reviewer has
// to check.
//
// Two of them, because this reads from both databases: ctl holds the other
// callers' declarations under the atlantis schema, and live is the database
// being described. They are the same connection unless a managed database is
// configured — passing live for both sent a SELECT on
// atlantis.caller_registrations to the customer's database, which does not
// have it.
func (s *Service) compareToLive(ctx context.Context, ctl, live introspect.Querier, subs []CallerSubmission) (*liveComparison, error) {
	submitterNames := make(map[string]bool, len(subs))
	var parsed []*dsl.File
	for _, sub := range subs {
		submitterNames[sub.Caller] = true
		ps, errs := parseSubmitted(sub.Caller, sub.Files)
		if len(errs) > 0 {
			return nil, fmt.Errorf("admin: parse failed for %s: %v", sub.Caller, errs)
		}
		parsed = append(parsed, ps...)
	}
	others, err := s.loadOtherCallersExcluding(ctx, ctl, submitterNames)
	if err != nil {
		return nil, err
	}
	declaredIR, err := dsl.Lower(append(parsed, others...))
	if err != nil {
		return nil, fmt.Errorf("admin: lower failed: %w", err)
	}

	introspectedIR, existingIDs, warnings, err := introspect.FromPostgres(ctx, live, declaredIR)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	codegen.AssignProtoNumbers(introspectedIR, declaredIR)
	d := codegen.ComputeDiff(introspectedIR, declaredIR)
	return &liveComparison{
		DeclaredIR:     declaredIR,
		IntrospectedIR: introspectedIR,
		ExistingIDs:    existingIDs,
		Diff:           d,
		Drift:          translateDrift(d),
		Warnings:       warnings,
	}, nil
}

// mismatchCount is how many findings block a baseline: both sides exist and
// disagree. Additions and removals are outstanding work.
func mismatchCount(drift []AdoptDriftItem) int {
	n := 0
	for _, di := range drift {
		if di.Severity == "mismatch" {
			n++
		}
	}
	return n
}

// filterToExistingEntities returns a shallow-cloned IR with only the
// entities whose physical table exists in the live DB. Declarations for
// tables that don't yet exist are dropped so they remain pending for
// `tide apply` to materialize.
//
// Give this the INTROSPECTED IR. It filters whole entities and nothing
// finer, so whatever it is handed is what gets baselined attribute for
// attribute — hand it the declaration and every declared-but-absent
// attribute of an existing table is recorded as present. See the call site
// in AdoptBaseline for what that cost.
//
// Procedures and custom queries are preserved verbatim because they
// have no physical SQL footprint until invoked; FromPostgres carries them
// across from the declaration for the same reason.
func filterToExistingEntities(in *dsl.IR, existing map[string]bool) *dsl.IR {
	out := &dsl.IR{
		Queries:    append([]dsl.CustomQuery(nil), in.Queries...),
		Procedures: append([]dsl.CustomProcedure(nil), in.Procedures...),
	}
	for _, e := range in.Entities {
		if existing[e.ID()] {
			out.Entities = append(out.Entities, e)
		}
	}
	return out
}

// classifyDriftSeverity assigns a Terraform-style severity. "Added" /
// "removed" kinds are pure additions or removals; everything else is a
// modification (both sides exist, they disagree). The string match is
// against codegen.ChangeKind values verbatim, so a kind added in codegen and
// not listed here falls to the default. "mismatch" understates nothing, but it
// is silent: an unlisted _added kind reads as a modification. Nothing links
// this switch to codegen's ChangeKind constants at compile time, so
// driftseverity_test.go reads them out of codegen's source and holds this
// function to the rule above.
//
// The kinds stay enumerated rather than classified by suffix, so each is a
// decision. A kind whose name ends in _added but which is really a tightening
// of an existing column would be classified wrongly by a suffix rule.
func classifyDriftSeverity(kind string) string {
	switch kind {
	case "entity_added",
		"field_added",
		"field_reference_added",
		"field_unique_added",
		"field_serial_added",
		"field_backfill_added",
		"check_added",
		"index_added",
		"composite_unique_added",
		"custom_query_added",
		"procedure_added",
		// Tenant isolation appearing. The judgement, since this test demands
		// one rather than deriving it from the suffix: reconciling means
		// CREATING a policy the live database does not have, so it is an
		// addition in the same sense as index_added.
		//
		// Worth saying plainly that "addition" understates it. The live
		// database currently serves every tenant's rows to every caller on
		// this table while the schema claims otherwise, and adopting will
		// change what every existing reader can see. There is no arm here that
		// carries that weight — the three are addition, removal and
		// modification — so this is filed by DDL direction and the severity
		// lives in the plan class, which is ClassDestructive.
		"partition_added",
		// An enum type or one of its labels present in the declaration and not
		// in the database. Reconciling means CREATE TYPE, or ALTER TYPE ...
		// ADD VALUE, so both are additions in the same sense as index_added.
		"enum_added",
		"enum_value_added":
		return "addition"
	case "entity_removed",
		"field_removed",
		"field_reference_removed",
		"field_unique_removed",
		"field_serial_removed",
		"field_backfill_removed",
		"check_removed",
		"index_removed",
		"composite_unique_removed",
		"custom_query_removed",
		"procedure_removed",
		// Tenant isolation disappearing. Reconciling means DROPPING a policy
		// the live database is enforcing, after which every caller reads every
		// tenant's rows. A removal, and the most consequential one in this
		// list.
		"partition_removed",
		// An enum type present in the database and not in the declaration.
		// Reconciling means DROP TYPE, which drops every column of it.
		"enum_removed",
		// A label the database has and the declaration does not.
		//
		// Filed as a removal by DDL direction, like partition_removed, though
		// Postgres cannot perform it: there is no ALTER TYPE ... DROP VALUE, so
		// reconciling needs a new type, a rewrite of every column using it, and
		// a drop of the old one. EmitSQL refuses the migration, and the plan
		// class carries that weight.
		"enum_value_removed":
		return "removal"
	}
	return "mismatch"
}

func callerSubmissionsFromPB(in []*adminpb.CallerSubmission) []CallerSubmission {
	if len(in) == 0 {
		return nil
	}
	out := make([]CallerSubmission, 0, len(in))
	for _, sub := range in {
		out = append(out, CallerSubmission{
			Caller: sub.GetCaller(),
			Files:  submittedFilesFromPB(sub.GetFiles()),
		})
	}
	return out
}

func adoptDriftToPB(in []AdoptDriftItem) []*adminpb.AdoptDriftItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.AdoptDriftItem, 0, len(in))
	for _, d := range in {
		out = append(out, &adminpb.AdoptDriftItem{
			EntityId: d.EntityID,
			Field:    d.Field,
			Kind:     d.Kind,
			Severity: d.Severity,
			Detail:   d.Detail,
		})
	}
	return out
}
