package codegen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/predsql"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// EmitSQL turns a Diff into a pair of migration scripts.
//
// Design choices:
//   - One file per call (caller decides the filename / sequence number).
//   - Up and down are returned as separate strings; the caller writes them
//     to disk side-by-side (NNNN_<name>.up.sql, NNNN_<name>.down.sql).
//   - Entities default to the `atlantis` schema; the `table "schema.table"`
//     override routes them elsewhere, and the emitter issues
//     CREATE SCHEMA IF NOT EXISTS for each non-default target before any
//     CREATE TABLE.
//   - Reversibility: every up has a matching down. CI runs up/down/up
//     against a fresh DB.
//
// Caveats accepted:
//   - We do NOT auto-generate backfill SQL — that's a separate file the
//     engineer supplies (--backfill).
//   - Cache and query_timeout changes do not produce SQL (they affect server
//     behavior only). They are reflected in the script as a no-op comment so
//     the file isn't empty when those are the only changes.
//   - Schema-qualified names throughout (atlantis.* by default; the
//     schema from `table "schema.table"` when overridden), with no
//     search_path manipulation, so the emitted SQL is unambiguous
//     regardless of the caller's session search_path.
//
// SQLScripts is the migration output of one emit pass. Up + Down are the
// legacy single-script forms — what plain `tide apply` consumes. The four
// PreBackfill / PostBackfill fields and BackfillFields are populated when
// the diff contains at least one field with a `backfill` modifier paired
// with a NOT-NULL change; `tide apply --backfill` runs them in order
// around the chunked UPDATE loop:
//
//	PreBackfillUp       — additive parts + ADD COLUMN nullable for backfilled fields
//	PreBackfillIndexes  — CREATE INDEX CONCURRENTLY ... WHERE field IS NULL
//	(chunked UPDATE loop runs here, driven by BackfillFields)
//	PostBackfillUp      — ALTER COLUMN SET NOT NULL on backfilled fields
//	PostBackfillIndexes — DROP INDEX CONCURRENTLY (mirror of the partial idx)
//
// For non-backfill plans the four scripts are empty strings and
// BackfillFields is nil — callers treat "PostBackfillUp == ”" as the
// no-phase-split signal.
type SQLScripts struct {
	Up   string
	Down string

	PreBackfillUp       string
	PreBackfillIndexes  string
	PostBackfillUp      string
	PostBackfillIndexes string

	BackfillFields []BackfillField
}

// BackfillField is one entry in the chunked-UPDATE driver list. The admin
// RPC turns these into atlantis.backfill_field_state rows that the
// background worker drains. TableName is schema-qualified and pre-quoted
// so the splicer can embed it verbatim.
type BackfillField struct {
	EntityID   string `json:"entity_id"`
	Field      string `json:"field"`
	Expression string `json:"expression"`
	PKColumn   string `json:"pk_column"`
	TableName  string `json:"table_name"`
}

// EmitSQL emits one migration covering every change in d, against the new IR
// (newIR is needed to look up entity / field shape for additive changes).
//
// oldIR may be nil (initial migration). newIR must not be nil.
func EmitSQL(oldIR, newIR *dsl.IR, d *Diff) (SQLScripts, error) {
	if newIR == nil {
		return SQLScripts{}, fmt.Errorf("EmitSQL: newIR is required")
	}
	// Both sides. The down script re-creates entities dropped in this migration
	// from the *old* IR (emitChange/KindEntityRemoved), so an old entity with
	// two identically-named checks produced a rollback Postgres rejects —
	// EmitInitial refused the very same IR while EmitSQL emitted it happily.
	if err := assertCheckNamesUnique(newIR); err != nil {
		return SQLScripts{}, err
	}
	if err := assertCheckNamesUnique(oldIR); err != nil {
		return SQLScripts{}, fmt.Errorf("prior schema: %w", err)
	}
	newByID := indexByID(newIR)
	oldByID := indexByID(oldIR)

	up := &sqlBuilder{}
	down := &sqlBuilder{}

	// Static banner. Per-change banners come from emitClass below.
	up.line("-- atlantis migration (generated)")
	up.line("-- DO NOT EDIT BY HAND. Re-run `tide plan` after editing .atl files.")
	up.blank()
	emitLockTimeout(up)
	down.line("-- atlantis migration (generated, down)")
	down.line("-- DO NOT EDIT BY HAND.")
	down.blank()
	emitLockTimeout(down)

	// Ensure every Postgres schema referenced by a newly-added entity exists
	// before any CREATE TABLE in that schema runs. CREATE SCHEMA IF NOT
	// EXISTS is idempotent, so re-emitting it on a follow-up migration that
	// happens to add another entity to the same schema is harmless. The
	// `atlantis` and `public` schemas are skipped — `atlantis` is created
	// by the infra migrations at server boot, and `public` always exists.
	// Down does NOT drop schemas: a schema may hold non-atlantis objects,
	// and a rollback that leaves an empty schema is a tiny, harmless
	// artifact compared to risking data loss in unrelated tables.
	for _, name := range collectNewSchemas(d.Additive, newByID) {
		up.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	if len(collectNewSchemas(d.Additive, newByID)) > 0 {
		up.blank()
	}

	// Process all changes in a deterministic order. We split by class so the
	// reader sees additive first, then any backfill-required changes (with
	// big comment banners), then breaking changes (likewise).
	// The down script emits its class groups in reverse, because a rollback
	// has to undo the migration in the opposite order it was applied.
	//
	// Emitting both scripts in the same order produced rollbacks that could not
	// run. Dropping a column and the entity-level CHECK that references it puts
	// the CHECK removal in ADDITIVE and the column removal in BREAKING; the up
	// script correctly drops the constraint first, but the down script then
	// re-added the constraint before re-adding the column:
	//
	//	ALTER TABLE "zz_zz" ADD CONSTRAINT "zz_total_pos" CHECK (total > 0);
	//	ALTER TABLE "zz_zz" ADD COLUMN "total" INTEGER;
	//	ERROR:  column "total" does not exist
	//
	// This is general, not specific to CHECKs: any dependency spanning two
	// class groups has the same shape.
	// Policies that must be rebuilt come down BEFORE any class group runs, and
	// go back up after all of them.
	//
	// Not as a change competing for position. PostgreSQL refuses to alter a
	// column a policy depends on, and the column change's own class varies with
	// what kind of change it is — so ordering by class group is not a rule, it
	// is a coincidence that holds until it does not. A rebuild is a constraint
	// on the WHOLE migration: nothing may touch that column while the policy
	// stands, and the policy must stand again when the migration ends.
	//
	// Bracketing it is also what makes the down script correct without a second
	// rule, because the down groups are emitted in reverse.
	rebuilt := partitionRebuilds(d, newByID, oldByID)
	for _, r := range rebuilt {
		up.commentf("rebuild tenant isolation on %s: PostgreSQL refuses to alter a column a policy depends on", r.entityID)
		emitPartitionIndexDrop(up, r.oldE)
		up.linef("DROP POLICY IF EXISTS %s ON %s;",
			quoteIdent(partitionPolicyName(r.oldE)), qualifiedTable(r.oldE))
		up.blank()
		emitPartitionIndexDrop(down, r.newE)
		down.linef("DROP POLICY IF EXISTS %s ON %s;",
			quoteIdent(partitionPolicyName(r.newE)), qualifiedTable(r.newE))
		down.blank()
	}

	downGroups := make([]*sqlBuilder, 0, 3)
	for _, g := range []struct {
		label   string
		changes []Change
	}{
		{"ADDITIVE", d.Additive},
		{"BACKFILL REQUIRED", d.BackfillRequired},
		// Destructive before breaking: a parked object must be out of the way
		// before anything that might recreate a name it still holds.
		{"DESTRUCTIVE — PARKED, REAPED AFTER THE RETENTION WINDOW", d.Destructive},
		// Bracketed entities have their partition change removed from the
		// group. The bracket owns the drop and the recreate for them, and
		// leaving the change here as well emitted the policy twice — on the
		// DOWN path the group's copy recreated it BEFORE the column was
		// reverted, reproducing the exact SQLSTATE 0A000 the bracket removes.
		// The same duplication was already fixed for the same-column arm; this
		// makes it true for all three.
		{"BREAKING — REVIEW CAREFULLY", withoutBracketedPartitionChanges(d.Breaking, rebuilt)},
	} {
		gd := &sqlBuilder{}
		emitClass(up, gd, g.label, g.changes, newByID, oldByID)
		downGroups = append(downGroups, gd)
	}
	for i := len(downGroups) - 1; i >= 0; i-- {
		down.raw(downGroups[i].String())
	}

	for _, r := range rebuilt {
		emitPartitionPolicy(up, r.newE)
		up.blank()
		emitPartitionPolicy(down, r.oldE)
		down.blank()
	}

	// If nothing was emitted, the migration is genuinely empty (e.g., only
	// cache changes). Leave a comment so the file isn't blank.
	if d.IsEmpty() {
		up.line("-- (no schema changes)")
		down.line("-- (no schema changes)")
	}

	scripts := SQLScripts{Up: up.String(), Down: down.String()}
	// Phase-split scripts are emitted in a separate pass because they
	// have different routing rules (NOT NULL deferred to post, partial
	// index on the NULL set bracketing the chunked backfill loop).
	// Populated only when the diff contains a backfilled field paired
	// with a NOT NULL tightening or new-NOT-NULL — otherwise the four
	// PreBackfill* / PostBackfill* fields stay empty and callers fall
	// through to plain `tide apply`.
	if needsPhaseSplit(d, newByID) {
		pre, preIdx, post, postIdx, fields := buildPhaseSplit(d, newByID, oldByID)
		scripts.PreBackfillUp = pre.String()
		scripts.PreBackfillIndexes = preIdx.String()
		scripts.PostBackfillUp = post.String()
		scripts.PostBackfillIndexes = postIdx.String()
		scripts.BackfillFields = fields
	}
	return scripts, nil
}

// collectRequiredExtensions walks newIR and returns the sorted set of
// Postgres extension names the schema depends on. Triggers:
//
//	vector(N) column      → vector
//	hypertable entity     → timescaledb
//	citext column         → citext
//
// Used by EmitInitial to emit only the CREATE EXTENSION calls the schema
// actually needs. The server's apply-time auto-enable path
// (internal/server/admin/extensions.go) uses the same trigger list to
// drive pg_available_extensions / pg_extension checks at apply time.
func collectRequiredExtensions(newIR *dsl.IR) []string {
	seen := map[string]struct{}{}
	add := func(name string) { seen[name] = struct{}{} }
	for i := range newIR.Entities {
		e := &newIR.Entities[i]
		if e.Kind == dsl.EntityKindHypertable {
			add("timescaledb")
		}
		for j := range e.Fields {
			collectFieldExtensions(&e.Fields[j].Type, add)
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// collectFieldExtensions descends through Array/Elem so a hypothetical
// `[]vector(N)` still triggers pgvector.
func collectFieldExtensions(t *dsl.FieldType, add func(string)) {
	if t == nil {
		return
	}
	switch t.Name {
	case "vector":
		add("vector")
	case "citext":
		add("citext")
	}
	if t.Array && t.Elem != nil {
		collectFieldExtensions(t.Elem, add)
	}
}

// collectInitialSchemas returns every distinct Postgres schema (other than
// "atlantis" and "public") referenced by entities in newIR — sorted for
// deterministic output. Used by EmitInitial to provision schemas for
// `table "schema.table"` overrides ahead of CREATE TABLE.
func collectInitialSchemas(newIR *dsl.IR) []string {
	seen := map[string]struct{}{}
	for i := range newIR.Entities {
		s := entitySchema(&newIR.Entities[i])
		if s == "atlantis" || s == "public" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// collectNewSchemas walks the EntityAdded changes in additive and returns
// every distinct Postgres schema (other than "atlantis" and "public") that
// the new entities will land in — sorted for deterministic output. Used to
// emit `CREATE SCHEMA IF NOT EXISTS` headers ahead of the first CREATE
// TABLE in each non-default schema.
func collectNewSchemas(additive []Change, newByID map[string]*dsl.Entity) []string {
	seen := map[string]struct{}{}
	for _, ch := range additive {
		if ch.Kind != KindEntityAdded {
			continue
		}
		e := newByID[ch.EntityID]
		if e == nil {
			continue
		}
		s := entitySchema(e)
		if s == "atlantis" || s == "public" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// needsPhaseSplit returns true iff the diff requires the apply to split
// around a chunked backfill — i.e., a field has Backfill != "" and is
// involved in a NOT NULL tightening or new-NOT-NULL change.
func needsPhaseSplit(d *Diff, newByID map[string]*dsl.Entity) bool {
	for _, ch := range d.BackfillRequired {
		if isBackfilledFieldInDiff(ch, newByID) {
			return true
		}
	}
	for _, ch := range d.Additive {
		if isBackfilledFieldInDiff(ch, newByID) {
			return true
		}
	}
	return false
}

func isBackfilledFieldInDiff(ch Change, newByID map[string]*dsl.Entity) bool {
	if ch.Kind != KindFieldAdded && ch.Kind != KindFieldNotNullTightened {
		return false
	}
	e := newByID[ch.EntityID]
	if e == nil {
		return false
	}
	f := e.FindField(ch.Field)
	if f == nil || f.Backfill == "" {
		return false
	}
	// FieldAdded counts only when the new field is NOT NULL — otherwise
	// there's no constraint to defer.
	if ch.Kind == KindFieldAdded && !f.NotNull {
		return false
	}
	return true
}

// buildPhaseSplit walks d once more and emits the four phase-split
// scripts. Non-backfill changes go to pre verbatim (via emitChange). For
// each backfilled field with a NOT NULL change: the ADD COLUMN body goes
// to pre as nullable, the SET NOT NULL goes to post, and a partial index
// on the NULL set is created in preIdx + dropped in postIdx.
//
// Down emission is not phase-split — a rollback of a backfill plan is a
// manual operator concern; the legacy Down covers it.
func buildPhaseSplit(d *Diff, newByID, oldByID map[string]*dsl.Entity) (pre, preIdx, post, postIdx *sqlBuilder, fields []BackfillField) {
	pre = &sqlBuilder{}
	preIdx = &sqlBuilder{}
	post = &sqlBuilder{}
	postIdx = &sqlBuilder{}

	pre.line("-- atlantis migration (pre-backfill phase)")
	pre.line("-- Runs in the apply tx before `tide apply --backfill` kicks off the chunked UPDATE.")
	pre.blank()
	emitLockTimeout(pre)

	// Mirror EmitSQL's CREATE SCHEMA prelude in the phase-split path so a
	// backfill plan whose additive part introduces a new namespace can also
	// boot from a clean DB.
	for _, name := range collectNewSchemas(d.Additive, newByID) {
		pre.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	if len(collectNewSchemas(d.Additive, newByID)) > 0 {
		pre.blank()
	}
	post.line("-- atlantis migration (post-backfill phase)")
	post.line("-- Runs after the chunked backfill completes — applies SET NOT NULL on backfilled fields.")
	post.blank()
	emitLockTimeout(post)

	// The index scripts deliberately get no lock_timeout, for two reasons that
	// both have to hold.
	//
	// SET LOCAL only takes effect inside a transaction block. These run outside
	// one, so the statement would be accepted, emit a WARNING, and change
	// nothing — a guard that reads as present and cannot fire, which is worse
	// than an absent one because it stops anybody looking again.
	//
	// A session-level SET would take effect, and is still wrong here. CREATE
	// INDEX CONCURRENTLY that hits its timeout mid-build leaves an INVALID
	// index behind, which no later run cleans up and which every planner then
	// ignores while it occupies the name. Waiting is the better failure.
	preIdx.line("-- Partial-index lifecycle for the chunked backfill. CREATE INDEX CONCURRENTLY")
	preIdx.line("-- runs OUTSIDE a transaction; each line is its own statement.")
	preIdx.line("-- No lock_timeout here: SET LOCAL is inert outside a tx, and a timeout")
	preIdx.line("-- mid-build would leave an INVALID index holding the name.")
	preIdx.blank()
	postIdx.line("-- Drop the partial indexes created pre-backfill. CONCURRENTLY for parity.")
	postIdx.blank()

	deferred := map[[2]string]*dsl.Entity{}
	throwaway := &sqlBuilder{}

	for _, ch := range d.Additive {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}
	for _, ch := range d.BackfillRequired {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}
	for _, ch := range d.Breaking {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}

	for key, e := range deferred {
		entityID, fieldName := key[0], key[1]
		f := e.FindField(fieldName)
		pk := primaryKeyColumn(e)
		if pk == "" || f == nil {
			preIdx.commentf("SKIPPED: %s has no single-column PK; chunked backfill requires a single-column PK for the cursor", qualifiedTable(e))
			continue
		}
		idxName := backfillIndexName(e, fieldName)
		preIdx.linef("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s) WHERE %s IS NULL;",
			quoteIdent(idxName), qualifiedTable(e), quoteIdent(pk), quoteIdent(fieldName))
		postIdx.linef(`DROP INDEX CONCURRENTLY IF EXISTS %s.%s;`, quoteIdent(entitySchema(e)), quoteIdent(idxName))
		fields = append(fields, BackfillField{
			EntityID:   entityID,
			Field:      fieldName,
			Expression: f.Backfill,
			PKColumn:   pk,
			TableName:  qualifiedTable(e),
		})
	}
	return pre, preIdx, post, postIdx, fields
}

// emitPhaseSplitChange emits one change into the pre/post builders,
// routing NOT NULL on backfilled fields to post. throwaway absorbs the
// "down" output that emitChange always writes — phase split doesn't need
// it.
func emitPhaseSplitChange(ch Change, newByID, oldByID map[string]*dsl.Entity, pre, post, throwaway *sqlBuilder, deferred map[[2]string]*dsl.Entity) {
	e := newByID[ch.EntityID]
	if e == nil {
		emitChange(pre, throwaway, ch, newByID, oldByID)
		return
	}
	switch ch.Kind {
	case KindFieldAdded:
		f := e.FindField(ch.Field)
		if f != nil && f.Backfill != "" && f.NotNull {
			pre.commentf("%s: %s (backfill-deferred; ADD nullable here, SET NOT NULL in post)", ch.Kind, ch.Detail)
			emitFieldAddNullable(pre, e, f)
			pre.blank()
			post.commentf("%s: %s (post-backfill SET NOT NULL)", ch.Kind, ch.Detail)
			emitNotNull(post, e, ch.Field, true)
			post.blank()
			deferred[[2]string{ch.EntityID, ch.Field}] = e
			return
		}
		emitChange(pre, throwaway, ch, newByID, oldByID)
	case KindFieldNotNullTightened:
		f := e.FindField(ch.Field)
		if f != nil && f.Backfill != "" {
			post.commentf("%s: %s (post-backfill SET NOT NULL)", ch.Kind, ch.Detail)
			emitNotNull(post, e, ch.Field, true)
			post.blank()
			deferred[[2]string{ch.EntityID, ch.Field}] = e
			return
		}
		emitChange(pre, throwaway, ch, newByID, oldByID)
	default:
		emitChange(pre, throwaway, ch, newByID, oldByID)
	}
}

// emitFieldAddNullable adds the column with NOT NULL suppressed, even if
// f.NotNull is true. Used by the phase-split builder so the column is
// nullable while the chunked backfill populates it; Phase 3 then runs
// SET NOT NULL.
func emitFieldAddNullable(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	nullable := *f
	nullable.NotNull = false
	b.linef("ALTER TABLE %s ADD COLUMN %s;", qualifiedTable(e), columnDecl(nullable, fieldCheckName(e, nullable.Name)))
	if f.Ref != nil {
		emitFKAdd(b, e, f)
	}
}

// backfillIndexName names the partial index Phase 1 creates and Phase 3
// drops. Shape mirrors the existing pkName / fkName / uqName helpers.
func backfillIndexName(e *dsl.Entity, fieldName string) string {
	return tableName(e) + "_" + fieldName + "_backfill_idx"
}

// primaryKeyColumn returns the single PK column name, or "" for a
// composite-PK entity. The chunked-backfill driver only supports
// single-column cursors today.
func primaryKeyColumn(e *dsl.Entity) string {
	if pf := e.PrimaryField(); pf != nil {
		return pf.Name
	}
	return ""
}

func emitClass(up, down *sqlBuilder, label string, changes []Change, newByID, oldByID map[string]*dsl.Entity) {
	if len(changes) == 0 {
		return
	}
	up.commentf("==== %s ====", label)
	down.commentf("==== %s ====", label)
	for _, ch := range reorderEntityAddsByFKDependency(changes, newByID) {
		emitChange(up, down, ch, newByID, oldByID)
	}
	up.blank()
	down.blank()
}

// reorderEntityAddsByFKDependency returns changes with EntityAdded entries
// topologically sorted ahead of all other changes, so an FK from a new
// entity to another new entity in the same migration resolves correctly
// regardless of the entities' alphabetical order. Non-EntityAdded changes
// (field add, NOT NULL tighten, …) target entities that already exist in
// the prior schema, so their ordering relative to each other is preserved.
// On a topo failure (true cycle between two different new entities), the
// original order is returned so the apply fails with a clearer Postgres
// error rather than swallowing the change set.
func reorderEntityAddsByFKDependency(changes []Change, newByID map[string]*dsl.Entity) []Change {
	var adds []Change
	var rest []Change
	for _, ch := range changes {
		if ch.Kind == KindEntityAdded {
			adds = append(adds, ch)
		} else {
			rest = append(rest, ch)
		}
	}
	if len(adds) <= 1 {
		return changes
	}

	entities := make([]dsl.Entity, 0, len(adds))
	for _, ch := range adds {
		if e := newByID[ch.EntityID]; e != nil {
			entities = append(entities, *e)
		}
	}
	sorted, err := topoSortEntities(entities)
	if err != nil {
		return changes
	}

	addByID := map[string]Change{}
	for _, ch := range adds {
		addByID[ch.EntityID] = ch
	}
	out := make([]Change, 0, len(changes))
	for _, e := range sorted {
		out = append(out, addByID[e.ID()])
	}
	out = append(out, rest...)
	return out
}

// emitChange dispatches one change to its specific emitter.
//
// Down statements are written in REVERSE structural order on the down side by
// each emitter — i.e. a CREATE TABLE on up is mirrored by DROP TABLE on down,
// and a column ADD is mirrored by a column DROP. The orchestration outer
// loop preserves the additive→backfill→breaking sequence on the up side; the
// down side mirrors that. (We don't re-sort the down statements —
// migrate runs them top-to-bottom as written, and the per-change reversal is
// sufficient because we never combine destructive + additive changes in the
// same migration without explicit ceremony.)
func emitChange(up, down *sqlBuilder, ch Change, newByID, oldByID map[string]*dsl.Entity) {
	up.commentf("%s: %s", ch.Kind, ch.Detail)
	down.commentf("%s (reversed): %s", ch.Kind, ch.Detail)
	switch ch.Kind {
	case KindEntityAdded:
		e := newByID[ch.EntityID]
		emitEntityCreate(up, e)
		// A real drop, not a park. This undoes a table THIS migration created,
		// so the only rows it can lose are ones written since it ran, and
		// parking would leave a tombstone behind for something the schema never
		// deliberately removed. Parking is for an author deleting a declaration.
		emitEntityDrop(down, e)
	case KindEntityRemoved:
		e := oldByID[ch.EntityID]
		emitEntityPark(up, e)
		emitEntityUnpark(down, e)
	case KindFieldAdded:
		e := newByID[ch.EntityID]
		f := e.FindField(ch.Field)
		emitFieldAdd(up, e, f)
		emitFieldDrop(down, e, f.Name)
	case KindFieldRemoved:
		oldE := oldByID[ch.EntityID]
		f := oldE.FindField(ch.Field)
		emitFieldPark(up, oldE, f.Name)
		// The down of a park is a rename back, not a recreate: recreating the
		// column would produce an empty one and discard exactly the data the
		// park exists to preserve.
		emitFieldUnpark(down, oldE, f.Name, f)
	case KindFieldNotNullTightened:
		e := newByID[ch.EntityID]
		emitNotNull(up, e, ch.Field, true)
		emitNotNull(down, e, ch.Field, false)
	case KindFieldNotNullLoosened:
		e := newByID[ch.EntityID]
		emitNotNull(up, e, ch.Field, false)
		emitNotNull(down, e, ch.Field, true)
	case KindFieldTypeChanged:
		e := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		fromT := oldE.FindField(ch.Field).Type
		toT := e.FindField(ch.Field).Type
		emitTypeChange(up, e, ch.Field, toT)
		emitTypeChange(down, oldE, ch.Field, fromT)
	case KindFieldDefaultChanged:
		e := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		emitDefault(up, e, ch.Field, e.FindField(ch.Field).Default)
		emitDefault(down, oldE, ch.Field, oldE.FindField(ch.Field).Default)
	case KindFieldUniqueAdded:
		e := newByID[ch.EntityID]
		emitUnique(up, e, ch.Field, true)
		emitUnique(down, e, ch.Field, false)
	case KindFieldUniqueRemoved:
		e := newByID[ch.EntityID]
		emitUnique(up, e, ch.Field, false)
		emitUnique(down, e, ch.Field, true)
	case KindChunkTimeIntervalChanged:
		e := newByID[ch.EntityID]
		if e == nil {
			break
		}
		emitChunkInterval := func(b *sqlBuilder, ms int) {
			if ms <= 0 {
				// Unset means "TimescaleDB's default". There is no call that
				// restores it, so the down script leaves the interval where the
				// up script put it rather than guessing at 7 days and pinning a
				// value the author never wrote.
				b.line("-- chunk_time_interval unset; TimescaleDB's default applies to new chunks")
				return
			}
			b.linef("SELECT set_chunk_time_interval('%s', INTERVAL '%d milliseconds');",
				qualifiedTable(e), ms)
		}
		to, _ := ch.To.(int)
		from, _ := ch.From.(int)
		emitChunkInterval(up, to)
		emitChunkInterval(down, from)
	case KindPartitionAdded:
		e := newByID[ch.EntityID]
		if e == nil {
			break
		}
		emitPartitionPolicy(up, e)
		emitPartitionDisable(down, e)
	case KindPartitionRemoved:
		// The clause is gone from the new side, so the column, the policy name
		// and the index name all have to come from the OLD entity — asking
		// newByID would find PartitionField empty and emit nothing, which is
		// the shape this whole task exists to remove.
		e := oldByID[ch.EntityID]
		if e == nil {
			break
		}
		emitPartitionDisable(up, e)
		emitPartitionPolicy(down, e)
	case KindPartitionChanged:
		newE, oldE := newByID[ch.EntityID], oldByID[ch.EntityID]
		if newE == nil || oldE == nil {
			break
		}
		// A same-column rebuild is owned by the prologue/epilogue bracket, not
		// by this arm. Emitting it here as well created the policy twice, and
		// the copy inside the class group could land BEFORE the column change
		// it exists to make room for — re-creating the wedge the bracket
		// removes. Only a genuine column MOVE is handled here.
		if oldE.PartitionField == newE.PartitionField {
			break
		}
		// The index is dropped explicitly before it is recreated. Both names
		// derive from the TABLE, not the column, so the old index already
		// occupies the new one's name — and emitPartitionPolicy writes
		// CREATE INDEX IF NOT EXISTS, which would find it and leave the index
		// pointing at the column the policy no longer uses. Every read would
		// then be a sequential scan under a policy that looks correct.
		//
		// The policy needs no such care: emitPartitionPolicy writes DROP POLICY IF
		// EXISTS under the same name before it creates one. (Third statement,
		// not first — an earlier comment here said "opens with".)
		emitPartitionIndexDrop(up, oldE)
		emitPartitionPolicy(up, newE)
		emitPartitionIndexDrop(down, newE)
		emitPartitionPolicy(down, oldE)
	case KindCheckAdded:
		e := newByID[ch.EntityID]
		if e == nil {
			break
		}
		emitCheckAdd(up, e, ch)
		emitCheckDrop(down, e, ch)
	case KindCheckRemoved:
		// The constraint exists on the old side, so its name and predicate come
		// from there; newByID is what the entity looks like after the change.
		e := newByID[ch.EntityID]
		if e == nil {
			break
		}
		emitCheckDrop(up, e, ch)
		// Re-adding on the down path restores the predicate that was dropped,
		// under the name it was dropped from. Omitting ch.Constraint here made
		// the synthetic Change fall back to the predicate scan, which found a
		// *different* constraint carrying the same predicate and re-added under
		// its name — so rolling back a removal of "b" emitted
		// ADD CONSTRAINT "a", colliding with the "a" that was never touched:
		//
		//	ERROR:  constraint "a" for relation "ckt_order" already exists
		emitCheckAdd(down, e, Change{
			EntityID:   ch.EntityID,
			Field:      ch.Field,
			Constraint: ch.Constraint,
			To:         ch.From,
		})
	case KindCheckChanged:
		e := newByID[ch.EntityID]
		if e == nil {
			break
		}
		// Drop then add, in that order, within one statement group. Splitting
		// them across groups would leave a window where the table carries
		// neither predicate.
		//
		// Every statement targets ch.Constraint, because a changed check keeps
		// its name: only an author-named check or a field check can reach this
		// kind. An *unnamed* entity check is named from its predicate, so
		// changing the predicate changes the name, which makes it a removal
		// plus an addition rather than a change — different keys, different
		// branch. Synthesizing a Change without the name here was the bug: the
		// drop fell back to hashing the old predicate and emitted
		// DROP CONSTRAINT IF EXISTS on a name that had never existed, so the
		// old constraint survived and the ADD then collided with it:
		//
		//	ERROR:  constraint "total_positive" for relation "shop_order" already exists
		emitCheckDrop(up, e, ch)
		emitCheckAdd(up, e, ch)
		emitCheckDrop(down, e, ch)
		emitCheckAdd(down, e, Change{
			EntityID:   ch.EntityID,
			Field:      ch.Field,
			Constraint: ch.Constraint,
			To:         ch.From,
		})
	case KindCompositeUniqueAdded:
		u, _ := ch.To.(dsl.UniqueSpec)
		e := newByID[ch.EntityID]
		emitCompositeUnique(up, e, u.Fields, true)
		emitCompositeUnique(down, e, u.Fields, false)
	case KindCompositeUniqueRemoved:
		u, _ := ch.From.(dsl.UniqueSpec)
		e := newByID[ch.EntityID]
		if e == nil {
			e = oldByID[ch.EntityID]
		}
		emitCompositeUnique(up, e, u.Fields, false)
		emitCompositeUnique(down, e, u.Fields, true)
	case KindFieldReferenceAdded:
		e := newByID[ch.EntityID]
		f := e.FindField(ch.Field)
		emitFKAdd(up, e, f)
		emitFKDrop(down, e, f)
	case KindFieldReferenceRemoved:
		oldE := oldByID[ch.EntityID]
		f := oldE.FindField(ch.Field)
		emitFKDrop(up, oldE, f)
		emitFKAdd(down, oldE, f)
	case KindFieldReferenceModified:
		newE := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		newF := newE.FindField(ch.Field)
		oldF := oldE.FindField(ch.Field)
		// Drop old FK, add new one.
		emitFKDrop(up, oldE, oldF)
		emitFKAdd(up, newE, newF)
		emitFKDrop(down, newE, newF)
		emitFKAdd(down, oldE, oldF)
	case KindIndexAdded:
		idx, _ := ch.To.(dsl.Index)
		e := newByID[ch.EntityID]
		emitIndexCreate(up, e, idx)
		emitIndexDrop(down, e, idx)
	case KindIndexRemoved:
		idx, _ := ch.From.(dsl.Index)
		e := newByID[ch.EntityID]
		if e == nil {
			e = oldByID[ch.EntityID]
		}
		emitIndexDrop(up, e, idx)
		emitIndexCreate(down, e, idx)
	case KindCacheChanged, KindQueryTimeoutChanged:
		up.line("-- (no SQL: cache / query_timeout are server-side)")
		down.line("-- (no SQL: cache / query_timeout are server-side)")
	case KindCustomQueryAdded, KindCustomQueryChanged, KindCustomQueryRemoved,
		KindProcedureAdded, KindProcedureChanged, KindProcedureRemoved:
		// Custom queries/procedures are served at runtime from the
		// checkpoint IR, not migrated — no DDL. The comment keeps the
		// migration file self-documenting and stops a custom-only apply
		// from rendering as "(no schema changes)".
		up.commentf("(no SQL: %s)", ch.Detail)
		down.commentf("(no SQL: %s)", ch.Detail)
	case KindFieldBackfillAdded, KindFieldBackfillRemoved, KindFieldBackfillChanged:
		// The backfill modifier is metadata for `tide apply --backfill`;
		// the schema doesn't change so no SQL is emitted in the legacy
		// up/down. Phase-split scripts are populated separately in
		// buildPhaseSplit.
		up.line("-- (no SQL: backfill modifier is metadata for tide apply --backfill)")
		down.line("-- (no SQL: backfill modifier is metadata for tide apply --backfill)")
	case KindFieldSerialAdded, KindFieldSerialRemoved:
		// Serial flips need operator coordination (sequence seed or caller
		// behavior verification); no auto-SQL today.
		up.line("-- (no SQL: serial flip requires explicit operator coordination)")
		down.line("-- (no SQL: serial flip requires explicit operator coordination)")
	case KindEntityTableChanged:
		// Atlantis won't auto-rename a physical table; if the `table "..."`
		// value moved, the operator runs ALTER TABLE RENAME themselves
		// before re-applying. We surface the intent as a comment so the
		// migration file is self-explaining.
		up.commentf("TABLE NAME CHANGED — manual ALTER TABLE RENAME required: %s", ch.Detail)
		down.commentf("TABLE NAME CHANGED — manual ALTER TABLE RENAME required: %s", ch.Detail)
	}
	up.blank()
	down.blank()
}

// emitTouchTrigger writes the per-entity BEFORE UPDATE trigger + its
// trigger function. We emit a dedicated function per entity rather than
// one shared function across the schema because the trigger function hardcodes
// the column name (`NEW.updated_at = now()`), and reaching for hstore /
// dynamic SQL to parameterize that adds an extension dependency. Per-entity
// functions are a few extra bytes of DDL each but cost nothing at runtime
// (Postgres caches the plan).
func emitTouchTrigger(b *sqlBuilder, e *dsl.Entity) {
	if e.TouchOnUpdateField == "" {
		return
	}
	fnName := triggerName(e, "touch_fn")
	trName := triggerName(e, "touch")
	col := e.TouchOnUpdateField

	// Function — OR REPLACE makes this idempotent on re-runs.
	b.linef(`CREATE OR REPLACE FUNCTION "atlantis".%s() RETURNS TRIGGER AS $$`, quoteIdent(fnName))
	b.line(`BEGIN`)
	b.linef(`  NEW.%s = now();`, quoteIdent(col))
	b.line(`  RETURN NEW;`)
	b.line(`END;`)
	b.line(`$$ LANGUAGE plpgsql;`)

	// Trigger — Postgres < 14 doesn't accept CREATE TRIGGER IF NOT EXISTS,
	// so DROP IF EXISTS + CREATE is the cross-version-safe idempotent form.
	b.linef(`DROP TRIGGER IF EXISTS %s ON %s;`, quoteIdent(trName), qualifiedTable(e))
	b.linef(`CREATE TRIGGER %s BEFORE UPDATE ON %s`, quoteIdent(trName), qualifiedTable(e))
	b.linef(`  FOR EACH ROW EXECUTE FUNCTION "atlantis".%s();`, quoteIdent(fnName))
}

func triggerName(e *dsl.Entity, suffix string) string {
	return tableName(e) + "_" + suffix
}

func emitEntityCreate(b *sqlBuilder, e *dsl.Entity) {
	// `IF NOT EXISTS` so the initial migration is idempotent — a
	// partially-failed run (or an out-of-band repair) can re-apply this
	// file without conflict. The diff-driven `tidectl plan` migrations
	// (additive / backfill / breaking) deliberately do NOT carry IF NOT
	// EXISTS so a missing object surfaces as a loud error rather than
	// silent drift.
	b.linef("CREATE TABLE IF NOT EXISTS %s (", qualifiedTable(e))
	cols := []string{}
	var tableConstraints []string
	for _, f := range e.Fields {
		cols = append(cols, "  "+columnDecl(f, fieldCheckName(e, f.Name)))
		if f.Ref != nil {
			// Emit FKs as table-level constraints so we can name them
			// deterministically (needed for DROP CONSTRAINT on FK removal).
			tableConstraints = append(tableConstraints, "  "+fkConstraintInline(e, &f))
		}
		if f.Primary && len(e.CompositePK) == 0 {
			tableConstraints = append(tableConstraints,
				fmt.Sprintf("  CONSTRAINT %s PRIMARY KEY (%s)", quoteIdent(pkName(e)), quoteIdent(f.Name)))
		}
	}
	// Composite primary key, if declared. Mutually exclusive with single-
	// field `primary` (the validator enforces this).
	if len(e.CompositePK) > 0 {
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s PRIMARY KEY (%s)",
				quoteIdent(pkName(e)), joinQuoted(e.CompositePK)))
	}
	// Composite UNIQUE constraints — table-level only (single-column UNIQUE
	// stays on the column).
	for _, u := range e.Uniques {
		name := compositeUniqueName(e, u.Fields)
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s UNIQUE (%s)", quoteIdent(name), joinQuoted(u.Fields)))
	}
	// Table-level CHECK constraints (multi-column / polymorphic XOR
	// predicates). The Expr is whatever the engineer wrote inside the
	// `check "..."` declaration; Postgres validates it at migration time.
	// Names come from resolveCheckNames, the same function the differ uses, so
	// a constraint created here and a constraint an ALTER later targets cannot
	// be named differently. Field-level checks are inlined by columnDecl and
	// auto-named by Postgres, so only the entity-level ones are emitted here —
	// but they are named in the presence of the field ones, which is why the
	// filter is on the resolved list rather than on e.Checks.
	for _, r := range resolveCheckNames(e) {
		if r.field != "" {
			continue
		}
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s CHECK (%s)", quoteIdent(r.name), r.expr))
	}
	allLines := append(cols, tableConstraints...)
	b.line(strings.Join(allLines, ",\n"))
	b.line(");")

	// Indexes.
	for _, idx := range e.Indexes {
		emitIndexCreate(b, e, idx)
	}

	// Hypertable bootstrap. create_hypertable takes the time column as a
	// quoted string literal (not an identifier), so we double the single
	// quotes for safety in the same way defaultExpr does.
	if e.Kind == dsl.EntityKindHypertable {
		// chunk_time_interval sizes each chunk. Omitted when unset so
		// TimescaleDB applies its own default (7 days) rather than us
		// hard-coding a number that would then be ours to keep current.
		//
		// Passed as an INTERVAL literal rather than a bare integer: the
		// parameter is ANYELEMENT, and an integer is interpreted in the time
		// column's own units — microseconds for timestamptz — so `604800000`
		// meaning "7 days" would silently become 10 minutes.
		interval := ""
		if e.ChunkTimeIntervalMS > 0 {
			interval = fmt.Sprintf(", chunk_time_interval => INTERVAL '%d milliseconds'",
				e.ChunkTimeIntervalMS)
		}
		b.linef("SELECT create_hypertable('%s', '%s', if_not_exists => TRUE%s);",
			qualifiedTable(e), strings.ReplaceAll(e.TimeField, "'", "''"), interval)
	}

	// Row-level security for `partition by`.
	emitPartitionPolicy(b, e)

	// BEFORE UPDATE auto-touch trigger. Emitted after the
	// table so the table exists at the moment the trigger function
	// references it via CREATE TRIGGER ... ON.
	emitTouchTrigger(b, e)
}

// emitEntityDrop parks a table instead of dropping it.
//
// The table is moved into the tombstone schema, where the application cannot
// reach it — every generated statement names atlantis.<table> explicitly — and
// left intact. A scheduled reaper drops it once the retention window has
// passed. Until then the change is undone by moving it back, with the rows
// still in it.
//
// This replaces DROP TABLE ... CASCADE, which was two problems in one line. The
// drop itself was irreversible: the documentation already conceded that a
// migration losing data is not reversible and that point-in-time restore of the
// entire database is the recovery path, which is not a recovery path for one
// mistaken line in a schema file. And CASCADE silently removed dependent
// objects — views, foreign keys from tables that were not part of this change —
// so the blast radius exceeded what the plan showed.
//
// Parking is deliberately not CASCADE-equivalent: if another object still
// depends on this table, the move fails and the migration stops. That is the
// point. A dependency nobody accounted for should surface as an error at apply
// time, not as a silent deletion.
func emitEntityDrop(b *sqlBuilder, e *dsl.Entity) {
	// No CASCADE. It removed dependent objects — views, foreign keys from
	// tables outside this change — that the plan never showed, so the blast
	// radius exceeded what was reviewed. Without it, an unaccounted dependency
	// fails the migration instead, which is the outcome worth having.
	b.linef("DROP TABLE IF EXISTS %s;", qualifiedTable(e))
	if e.TouchOnUpdateField != "" {
		// The trigger goes with the table; the function is separate and must be
		// dropped explicitly so the down migration is a true inverse of up.
		fnName := triggerName(e, "touch_fn")
		b.linef(`DROP FUNCTION IF EXISTS "atlantis".%s();`, quoteIdent(fnName))
	}
}

// emitEntityPark moves a table out of reach instead of dropping it.
//
// The table goes to the tombstone schema, where nothing can reach it — every
// generated statement names atlantis.<table> explicitly — and stays intact. A
// reaper drops it once the retention window passes; until then the change is
// undone by moving it back, rows and all.
//
// This is what replaces DROP TABLE for a removal the author asked for. The
// documentation already conceded that a migration losing data is irreversible
// and that point-in-time restore of the whole database is the recovery path,
// which is no recovery path at all for one mistaken line in a schema file.
func emitEntityPark(b *sqlBuilder, e *dsl.Entity) {
	// The PHYSICAL name, not the computed one. An entity may override its table
	// with `table "consumer.accounts"`, and 17 of the 19 schemas in this repo
	// do. SET SCHEMA moves whatever qualifiedTable names, so the table arrives
	// in the tombstone schema called `accounts` — while a rename addressing the
	// computed `<ns>_<entity>` name finds nothing. With IF EXISTS that rename
	// was a silent no-op, so the real table ended up stranded in the tombstone
	// schema registered under a name that had never existed: invisible to the
	// reaper, unrestorable by the down migration, and recorded in the register
	// as reaped on a date when it was not.
	srcSchema, srcTable := physicalParts(e)

	// Qualified by source schema because the tombstone schema is shared: two
	// entities differing only by schema (public.accounts, consumer.accounts)
	// would otherwise both want to be `accounts__parked` in it, and the second
	// park of the pair would fail the migration.
	parked := parkedName(srcSchema + "_" + srcTable)

	b.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(TombstoneSchema))
	// No IF EXISTS. Parking a table that is not there means the diff and the
	// database disagree about what exists, which is worth stopping for.
	b.linef("ALTER TABLE %s SET SCHEMA %s;", qualifiedTable(e), quoteIdent(TombstoneSchema))
	b.linef("ALTER TABLE %s.%s RENAME TO %s;",
		quoteIdent(TombstoneSchema), quoteIdent(srcTable), quoteIdent(parked))
	emitParkRegistration(b, "table", TombstoneSchema, parked, "", srcSchema, srcTable)
}

// emitEntityUnpark is the inverse: bring the table back with its rows.
//
// Not emitEntityCreate. Recreating the table would produce an empty one and
// discard precisely what parking preserved.
func emitEntityUnpark(b *sqlBuilder, e *dsl.Entity) {
	srcSchema, srcTable := physicalParts(e)
	parked := parkedName(srcSchema + "_" + srcTable)

	// Both statements below are renames, and a rename of an absent table under
	// IF EXISTS is a silent no-op — so this path used to report success having
	// restored nothing, which is the worst available answer to "put it back".
	// The guard exists to say WHY it is absent: by far the likeliest reason is
	// that the retention window passed and the reaper did its job, and the
	// operator needs to hear that rather than a bare "relation does not exist".
	b.linef("DO $$ BEGIN IF to_regclass(format('%%I.%%I', %s, %s)) IS NULL THEN "+
		"RAISE EXCEPTION 'atlantis: cannot restore %%: it is no longer in the "+
		"tombstone schema. Its retention window has most likely passed and the "+
		"reaper has dropped it, in which case its rows are gone and only a "+
		"restore from backup will bring them back.', %s; END IF; END $$;",
		sqlStringLiteral(TombstoneSchema), sqlStringLiteral(parked),
		sqlStringLiteral(srcSchema+"."+srcTable))

	b.linef("ALTER TABLE %s.%s RENAME TO %s;",
		quoteIdent(TombstoneSchema), quoteIdent(parked), quoteIdent(srcTable))
	// Back to the schema it came from, which is not necessarily atlantis: this
	// line named atlantis literally, so an adopted table restored into the
	// wrong schema and the entity's own reads could not find it.
	b.linef("ALTER TABLE %s.%s SET SCHEMA %s;",
		quoteIdent(TombstoneSchema), quoteIdent(srcTable), quoteIdent(srcSchema))
	emitParkDeregistration(b, TombstoneSchema, "", parked)
}

// TombstoneSchema holds parked objects awaiting the retention window.
//
// A separate schema rather than a naming convention inside atlantis: a parked
// table must be unreachable by anything that enumerates the live schema —
// introspection, drift detection, a caller's own query — and a schema boundary
// does that without every one of those paths having to remember a prefix rule.
const TombstoneSchema = "atlantis_tombstone"

// parkedName is the name a parked object takes.
//
// The suffix carries the plan that parked it rather than a timestamp: two drops
// of the same table in one retention window would otherwise collide on the
// second, and a collision here means the migration fails rather than the older
// tombstone being overwritten — but a name that says which plan did it is what
// makes the reaper's retention decision and any manual restore legible.
func parkedName(base string) string {
	return truncateIdent(base + "__parked")
}

func emitFieldAdd(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s ADD COLUMN %s;", qualifiedTable(e), columnDecl(*f, fieldCheckName(e, f.Name)))
	if f.Ref != nil {
		emitFKAdd(b, e, f)
	}
}

// emitFieldDrop parks a column instead of dropping it.
//
// A column cannot be moved to another schema, so it is renamed. That is enough
// to make it invisible: every generated statement names its columns explicitly,
// so a renamed column is absent from reads, writes and the entity's proto
// surface, while the data stays exactly where it was. Undoing it is a rename
// back.
//
// The cost is that the column still occupies its space and still enforces any
// NOT NULL it carried, so a parked NOT NULL column would block inserts. Its
// constraint is dropped for that reason — the column is retained for its data,
// not its rules.
func emitFieldDrop(b *sqlBuilder, e *dsl.Entity, name string) {
	b.linef("ALTER TABLE %s DROP COLUMN %s;", qualifiedTable(e), quoteIdent(name))
}

// emitFieldPark renames a column out of the way instead of dropping it.
//
// A column cannot move to another schema, so it is renamed — which is enough:
// every generated statement names its columns, so a renamed column is absent
// from reads, writes and the proto surface while its data stays put.
//
// Its NOT NULL is dropped because the column is being retained for its data,
// not its rules, and a parked NOT NULL column would reject inserts that touch
// only the live ones.
func emitFieldPark(b *sqlBuilder, e *dsl.Entity, name string) {
	parked := parkedName(name)
	b.linef("ALTER TABLE %s RENAME COLUMN %s TO %s;",
		qualifiedTable(e), quoteIdent(name), quoteIdent(parked))
	b.linef("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;",
		qualifiedTable(e), quoteIdent(parked))
	sch, tbl := physicalParts(e)
	emitParkRegistration(b, "column", sch, parked, tbl, sch, name)
}

// emitFieldUnpark renames the column back and restores the NOT NULL the park
// removed. Restoring it can fail if rows were written while the column was
// parked, which is correct: those rows have no value for it, and silently
// leaving the column nullable would return a different schema than the one
// being rolled back to.
func emitFieldUnpark(b *sqlBuilder, e *dsl.Entity, name string, f *dsl.Field) {
	parked := parkedName(name)
	b.linef("ALTER TABLE %s RENAME COLUMN %s TO %s;",
		qualifiedTable(e), quoteIdent(parked), quoteIdent(name))
	if f != nil && f.NotNull && !f.Primary {
		b.linef("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;",
			qualifiedTable(e), quoteIdent(name))
	}
	sch, tbl := physicalParts(e)
	emitParkDeregistration(b, sch, tbl, parked)
}

// DefaultParkRetention is how long a parked object is kept before the reaper
// may drop it.
//
// Long enough that a mistake is noticed by someone returning from leave, rather
// than only by whoever was watching that afternoon. The window is written into
// each row as an absolute instant, so shortening this later cannot retroactively
// expire something already parked under a longer promise.
const DefaultParkRetention = 30 * 24 * time.Hour

// emitParkRegistration records a parked object in the same statement group as
// the rename that parked it.
//
// In the migration rather than in the server on the side, because a park
// applied but not recorded is invisible to the reaper and to anyone looking for
// it: the object would survive every retention window and nobody would have a
// list saying it exists.
func emitParkRegistration(b *sqlBuilder, kind, schemaName, objectName, parentTable, originalSchema, originalName string) {
	parent := "NULL"
	if parentTable != "" {
		parent = sqlStringLiteral(parentTable)
	}
	// originalSchema is recorded separately from schema_name because for a
	// table they differ: schema_name is the tombstone the object now lives in,
	// originalSchema is where it has to go back to. Without it the register
	// cannot describe a restore, which is most of what a register is for.
	//
	// The conflict target is the partial unique index over live registrations.
	// Untargeted, DO NOTHING would also swallow a primary-key conflict and any
	// future constraint, turning a failed registration into a park that was
	// applied and never recorded.
	b.linef("INSERT INTO atlantis.parked_objects "+
		"(kind, schema_name, object_name, parent_table, original_schema, original_name, reap_after) "+
		"VALUES (%s, %s, %s, %s, %s, %s, now() + INTERVAL '%d days') "+
		"ON CONFLICT (schema_name, coalesce(parent_table, ''), object_name) "+
		"WHERE reaped_at IS NULL DO NOTHING;",
		sqlStringLiteral(kind), sqlStringLiteral(schemaName), sqlStringLiteral(objectName),
		parent, sqlStringLiteral(originalSchema), sqlStringLiteral(originalName),
		int(DefaultParkRetention/(24*time.Hour)))
}

// emitParkDeregistration removes the registration when a park is undone.
//
// A DELETE, not a reaped_at stamp: the object was restored, not reaped, and
// recording it as reaped would tell a later reader that data was destroyed when
// it was returned.
func emitParkDeregistration(b *sqlBuilder, schemaName, parentTable, objectName string) {
	parent := "parent_table IS NULL"
	if parentTable != "" {
		parent = "parent_table = " + sqlStringLiteral(parentTable)
	}
	b.linef("DELETE FROM atlantis.parked_objects "+
		"WHERE schema_name = %s AND %s AND object_name = %s AND reaped_at IS NULL;",
		sqlStringLiteral(schemaName), parent, sqlStringLiteral(objectName))
}

// sqlStringLiteral renders a Go string as a SQL literal. These values come from
// the schema (table and column names), not from request data, but they are
// still interpolated into DDL, so doubling quotes is not optional.
func sqlStringLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// physicalParts splits an entity's physical location into schema and table,
// which the registry stores separately so the reaper can address either.
func physicalParts(e *dsl.Entity) (string, string) {
	if sch, tbl, ok := strings.Cut(strings.ReplaceAll(qualifiedTable(e), `"`, ""), "."); ok {
		return sch, tbl
	}
	return "atlantis", tableName(e)
}

func emitNotNull(b *sqlBuilder, e *dsl.Entity, field string, on bool) {
	op := "SET"
	if !on {
		op = "DROP"
	}
	b.linef("ALTER TABLE %s ALTER COLUMN %s %s NOT NULL;", qualifiedTable(e), quoteIdent(field), op)
}

func emitTypeChange(b *sqlBuilder, e *dsl.Entity, field string, t dsl.FieldType) {
	b.linef("ALTER TABLE %s ALTER COLUMN %s TYPE %s;", qualifiedTable(e), quoteIdent(field), sqlType(t))
}

func emitDefault(b *sqlBuilder, e *dsl.Entity, field string, d *dsl.Default) {
	if d == nil {
		b.linef("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;", qualifiedTable(e), quoteIdent(field))
		return
	}
	b.linef("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;", qualifiedTable(e), quoteIdent(field), defaultExpr(*d))
}

// emitCompositeUnique adds or drops a multi-column UNIQUE constraint. The
// name is deterministic (compositeUniqueName) so the DROP on `on=false`
// finds exactly what a prior ADD created.
// checkConstraintName resolves the name a CHECK constraint is emitted under.
//
// This is the emitter's business, not the differ's. The differ matches old
// against new by identity (see resolvedChecks); names are computed here, from
// the entity being emitted for, so a positional name is always positional
// within the schema version it belongs to.
//
// A field-level check written as `total int check "total > 0"` is inlined into
// the column definition by EmitInitial and so has no name of its own there. It
// needs one to be added or dropped later, and the name must be derivable from
// the column alone.
// checkConstraintName returns the identifier a CHECK change acts on.
//
// Resolved at diff time and carried on the Change, because only the differ can
// see both sides. Deriving it here scanned the entity the emitter was handed —
// always the *new* one, which for a removal cannot contain the constraint being
// removed — then fell back to hashing the predicate, emitting
// DROP CONSTRAINT IF EXISTS on a name that had never existed. IF EXISTS turned
// that into a no-op, so the plan reported the constraint gone while the
// database went on enforcing it.
//
// There is no fallback. An earlier version kept one "for Changes built before
// Constraint existed", justified by a persisted-plan replay path — which does
// not exist: every emitter call site recomputes the diff from IR snapshots
// (admin.go, backfill.go, history.go, sandbox/embedded.go, cmd/tidectl/plan.go),
// and the stored schema_versions.diff JSON is only ever counted and rendered,
// never re-emitted. A fallback that cannot run is a second naming scheme
// nobody maintains, and it disagreed with resolveCheckNames.
//
// An empty name yields DROP CONSTRAINT IF EXISTS "" — invalid SQL that fails at
// apply. That is the right outcome for a Change this build cannot interpret,
// and the opposite of the silent success the old fallback produced.
func checkConstraintName(ch Change) string { return ch.Constraint }

// fieldCheckName returns the constraint name resolveCheckNames assigned to a
// field's `check` modifier, so the DDL that creates it and the DDL that later
// drops it cannot disagree. Empty when the field carries no check.
func fieldCheckName(e *dsl.Entity, field string) string {
	for _, r := range resolveCheckNames(e) {
		if r.field == field {
			return r.name
		}
	}
	return ""
}

// truncateIdent keeps a generated identifier inside Postgres's 63-byte limit.
//
// Postgres does not reject an over-long identifier; it silently truncates it.
// That is the dangerous behaviour: a CREATE storing a 63-byte prefix and a
// later DROP supplying the full 80-byte name refer to the same object only by
// accident, and DROP CONSTRAINT IF EXISTS turns the mismatch into a success.
//
// Truncating here, deterministically, means the name we emit is the name stored
// — so the two statements always agree. The hash suffix keeps distinct inputs
// distinct, which plain truncation would not: two long column names sharing a
// prefix would otherwise collapse to one identifier and CREATE TABLE would fail
// with "constraint already exists".
func truncateIdent(name string) string {
	const maxIdent = 63
	if len(name) <= maxIdent {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:4])

	// Cut on a rune boundary. The limit is bytes, but slicing bytes can land
	// mid-rune, and the resulting identifier is not valid UTF-8 — Postgres
	// rejects the whole statement with "invalid byte sequence for encoding
	// UTF8", naming a byte offset rather than the column, which leaves a
	// destructive migration stuck behind an error nobody can trace back to a
	// field name. The DSL admits any unicode letter, so this is reachable.
	budget := maxIdent - len(suffix)
	cut := 0
	for i := range name {
		if i > budget {
			break
		}
		cut = i
	}
	if cut+len(suffix) > maxIdent {
		cut = 0
	}
	return name[:cut] + suffix
}

// unnamedCheckName names an entity-level `check "..."` that the author did not
// name. It is the single source of that name: EmitInitial writes it when
// creating the table, and resolvedChecks derives constraint identity from it.
// The two must agree, or a migrated database and a freshly created one carry
// differently-named constraints and every later ALTER targets the wrong one.
//
// Derived from the predicate rather than from position. Position was the
// obvious choice and it was wrong: deleting the first of two unnamed checks
// renumbered the second, so a single deletion reported as a removal *plus* a
// predicate change on a constraint nobody touched — escalating the plan to
// backfill-required and re-validating the whole table under ACCESS EXCLUSIVE
// for a constraint that had not moved. Predicate-derived names make reordering
// a no-op, which is what it is.
//
// occurrence disambiguates the degenerate case of the same predicate written
// twice; Postgres requires distinct constraint names on a table, and two
// identical CHECKs are two constraints as far as the catalog is concerned.
//
// Truncated to four bytes. The collision domain is the checks of one table, so
// 2^32 is ample, and the name has to stay inside Postgres's 63-byte identifier
// limit alongside the table name.
func unnamedCheckName(e *dsl.Entity, expr string, occurrence int) string {
	sum := sha256.Sum256([]byte(expr))
	name := fmt.Sprintf("%s_check_%s", tableName(e), hex.EncodeToString(sum[:4]))
	if occurrence > 1 {
		name = fmt.Sprintf("%s_%d", name, occurrence)
	}
	return truncateIdent(name)
}

// emitCheckAdd renders ADD CONSTRAINT ... CHECK.
//
// No NOT VALID: the whole point of classifying this backfill-required is that
// the constraint IS validated against existing rows, so the apply fails loudly
// on data that violates it rather than leaving an unenforced constraint behind.
// A NOT VALID variant would be a different, deliberately-chosen behaviour, and
// would need its own plan class.
func emitCheckAdd(b *sqlBuilder, e *dsl.Entity, ch Change) {
	expr, _ := ch.To.(string)
	if expr == "" {
		return
	}
	b.linef("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);",
		qualifiedTable(e), quoteIdent(checkConstraintName(ch)), expr)
}

// emitCheckDrop renders DROP CONSTRAINT IF EXISTS.
//
// IF EXISTS because a down-migration may run against a database where the ADD
// never landed — the apply that would have created it failed on the very data
// validation this constraint exists to perform.
func emitCheckDrop(b *sqlBuilder, e *dsl.Entity, ch Change) {
	b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;",
		qualifiedTable(e), quoteIdent(checkConstraintName(ch)))
}

func emitCompositeUnique(b *sqlBuilder, e *dsl.Entity, fields []string, on bool) {
	name := compositeUniqueName(e, fields)
	if on {
		b.linef("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);", qualifiedTable(e), quoteIdent(name), joinQuoted(fields))
	} else {
		b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", qualifiedTable(e), quoteIdent(name))
	}
}

func emitUnique(b *sqlBuilder, e *dsl.Entity, field string, on bool) {
	name := uqName(e, field)
	if on {
		b.linef("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);", qualifiedTable(e), quoteIdent(name), quoteIdent(field))
	} else {
		b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", qualifiedTable(e), quoteIdent(name))
	}
}

func emitFKAdd(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s ADD CONSTRAINT %s %s;",
		qualifiedTable(e), quoteIdent(fkName(e, f.Name)), fkConstraintBody(f))
}

func emitFKDrop(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;",
		qualifiedTable(e), quoteIdent(fkName(e, f.Name)))
}

func emitIndexCreate(b *sqlBuilder, e *dsl.Entity, idx dsl.Index) {
	// `CREATE INDEX IF NOT EXISTS` on both the initial-migration and the
	// diff-driven add-index paths — the index name is deterministic, so a
	// re-run is a no-op rather than an error.
	name := quoteIdent(indexName(e, idx))
	switch idx.Kind {
	case dsl.IndexBtree:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			name, qualifiedTable(e), indexFieldList(idx.Fields))
	case dsl.IndexPartial:
		// Only the partial form can be unique (Postgres UNIQUE constraints
		// can't be partial) — see the DSL `unique index partial` form.
		unique := ""
		if idx.Unique {
			unique = "UNIQUE "
		}
		where := ""
		if idx.Where != nil {
			where = " WHERE " + predsql.Render(idx.Where)
		}
		b.linef("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)%s;",
			unique, name, qualifiedTable(e), indexFieldList(idx.Fields), where)
	case dsl.IndexHNSW:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (%s %s);",
			name, qualifiedTable(e), quoteIdent(idx.Field), idx.VecOps)
	case dsl.IndexGIN:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s USING gin (%s);",
			name, qualifiedTable(e), quoteIdent(idx.Field))
	}
}

func emitIndexDrop(b *sqlBuilder, e *dsl.Entity, idx dsl.Index) {
	b.linef("DROP INDEX IF EXISTS %s;", qualifiedIndexName(e, idx))
}

// qualifiedTable returns the schema-qualified, double-quoted table name.
// Delegates to the shared schema package.
func qualifiedTable(e *dsl.Entity) string {
	return schema.QualifiedTable(e)
}

// entitySchema returns the schema where this entity's physical objects live.
// Delegates to the shared schema package.
func entitySchema(e *dsl.Entity) string {
	return schema.EntitySchema(e)
}

// quoteIdent wraps a SQL identifier in double quotes.
// Delegates to the shared schema package.
func quoteIdent(s string) string {
	return schema.QuoteIdent(s)
}

// tableName maps an entity to its physical table name.
// Delegates to the shared schema package.
func tableName(e *dsl.Entity) string {
	return schema.TableName(e)
}

// columnDecl renders one column line for CREATE TABLE / ADD COLUMN.
//
// Identity strategy precedence:
//
//	serial   → render `BIGSERIAL` as the type itself (carries the
//	           sequence + NOT NULL + DEFAULT nextval(...) implicitly).
//	identity → render `<type> GENERATED ALWAYS AS IDENTITY`.
//	neither  → render `<type>` with explicit NOT NULL / DEFAULT modifiers.
func columnDecl(f dsl.Field, checkName string) string {
	var parts []string
	switch {
	case f.Serial:
		// BIGSERIAL replaces both the type and the GENERATED clause.
		parts = []string{quoteIdent(f.Name), "BIGSERIAL"}
	case f.Identity:
		parts = []string{quoteIdent(f.Name), sqlType(f.Type), "GENERATED ALWAYS AS IDENTITY"}
	default:
		parts = []string{quoteIdent(f.Name), sqlType(f.Type)}
		if f.NotNull && !f.Primary {
			parts = append(parts, "NOT NULL")
		}
		if f.Default != nil {
			parts = append(parts, "DEFAULT "+defaultExpr(*f.Default))
		}
	}
	if f.Unique && !f.Primary {
		parts = append(parts, "UNIQUE")
	}
	if f.Check != "" {
		// Named explicitly rather than left to Postgres.
		//
		// Postgres auto-names an inline single-column check <table>_<column>_check
		// and, when that exceeds the 63-byte identifier limit, shortens the table
		// and column parts *proportionally* (makeObjectName). Reproducing that
		// meant guessing an implementation detail, and the guess was wrong:
		//
		//	postgres:  zzlongns_an_entity_with_a_reall_a_column_with_a_long_name_check
		//	atlantis:  zzlongns_an_entity_with_a_really_quite_long_name_a_column_with_a_long_name_check
		//
		// A later DROP CONSTRAINT IF EXISTS used our 80-byte name, Postgres
		// truncated it to a different 63-byte string, IF EXISTS swallowed the
		// miss, and the migration reported success with the constraint still
		// enforcing. Emitting the name removes the guess entirely.
		//
		// For names inside the limit this produces exactly the name Postgres
		// would have chosen, so nothing already deployed is renamed.
		if checkName != "" {
			parts = append(parts, "CONSTRAINT "+quoteIdent(checkName)+" CHECK ("+f.Check+")")
		} else {
			parts = append(parts, "CHECK ("+f.Check+")")
		}
	}
	return strings.Join(parts, " ")
}

// sqlType maps a DSL field type to its Postgres type.
// Delegates to the shared schema package.
func sqlType(t dsl.FieldType) string {
	return schema.SQLType(t)
}

// defaultExpr renders a Default in SQL form.
// Delegates to the shared schema package.
func defaultExpr(d dsl.Default) string {
	return schema.DefaultExpr(d)
}

// fkConstraintInline renders an FK constraint suitable for use inside a
// CREATE TABLE column list (table-level form).
func fkConstraintInline(e *dsl.Entity, f *dsl.Field) string {
	return fmt.Sprintf("CONSTRAINT %s %s",
		quoteIdent(fkName(e, f.Name)), fkConstraintBody(f))
}

// fkConstraintBody renders the FOREIGN KEY ... REFERENCES ... body. Shared
// between inline (CREATE TABLE) and standalone (ALTER TABLE) forms. Every
// identifier — local column, target table, target column — is quoted so
// reserved-word names roundtrip through Postgres unchanged.
//
// Honors a `table "..."` override on the target entity via
// Ref.TargetTableName, populated at IR-lower time. Without the override,
// falls back to `"atlantis"."<flat>"`.
func fkConstraintBody(f *dsl.Field) string {
	target := fkTargetRef(f.Ref)
	out := fmt.Sprintf(`FOREIGN KEY (%s) REFERENCES %s (%s)`,
		quoteIdent(f.Name), target, quoteIdent(f.Ref.TargetField))
	if f.Ref.OnDelete != dsl.RefActionUnset {
		out += " ON DELETE " + f.Ref.OnDelete.String()
	}
	if f.Ref.OnUpdate != dsl.RefActionUnset {
		out += " ON UPDATE " + f.Ref.OnUpdate.String()
	}
	return out
}

// fkTargetRef renders the schema-qualified, quoted table reference for
// the right-hand side of a REFERENCES clause. Mirrors the splitting
// logic in entitySchema / entityPhysicalTable but operates on the Ref
// directly so callers don't need the target Entity pointer.
func fkTargetRef(r *dsl.Ref) string {
	if r.TargetTableName != "" {
		schema, table := "public", r.TargetTableName
		if i := strings.IndexByte(r.TargetTableName, '.'); i >= 0 {
			schema, table = r.TargetTableName[:i], r.TargetTableName[i+1:]
		}
		return quoteIdent(schema) + "." + quoteIdent(table)
	}
	return `"atlantis".` + quoteIdent(tableNameFromID(r.TargetID))
}

// tableNameFromID converts a canonical "namespace.Entity" into our flat table name.
func tableNameFromID(id string) string {
	parts := strings.SplitN(id, ".", 2)
	if len(parts) != 2 {
		return id
	}
	return parts[0] + "_" + snakeCase(parts[1])
}

// Constraint / index names are deterministic so we can DROP CONSTRAINT and
// DROP INDEX without remembering Postgres's auto-generated names. Each name
// fits within the 63-char Postgres identifier limit (truncation falls back
// to a hash suffix — we keep the unhashed form short by relying on
// snake_case entity names).

func pkName(e *dsl.Entity) string               { return tableName(e) + "_pkey" }
func fkName(e *dsl.Entity, field string) string { return tableName(e) + "_" + field + "_fkey" }
func uqName(e *dsl.Entity, field string) string { return tableName(e) + "_" + field + "_key" }

// compositeUniqueName names a multi-column UNIQUE constraint deterministically.
// The shape is <table>_<field1>_<field2>_..._key so DROP CONSTRAINT can find it.
func compositeUniqueName(e *dsl.Entity, fields []string) string {
	return tableName(e) + "_" + strings.Join(fields, "_") + "_key"
}

func indexName(e *dsl.Entity, idx dsl.Index) string {
	prefix := tableName(e)
	switch idx.Kind {
	case dsl.IndexBtree:
		return prefix + "_" + joinFieldNames(idx.Fields) + "_idx"
	case dsl.IndexPartial:
		if idx.Unique {
			return prefix + "_" + joinFieldNames(idx.Fields) + "_unique_partial_idx"
		}
		return prefix + "_" + joinFieldNames(idx.Fields) + "_partial_idx"
	case dsl.IndexHNSW:
		return prefix + "_" + idx.Field + "_hnsw_idx"
	case dsl.IndexGIN:
		return prefix + "_" + idx.Field + "_gin_idx"
	}
	return prefix + "_idx"
}

// qualifiedIndexName: indexes live in the same schema as the table they
// attach to, so honoring a `table "..."` override here is mandatory —
// otherwise DROP INDEX targets the wrong schema and silently no-ops.
func qualifiedIndexName(e *dsl.Entity, idx dsl.Index) string {
	return quoteIdent(entitySchema(e)) + "." + quoteIdent(indexName(e, idx))
}

func joinFieldNames(fs []dsl.IndexField) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		if f.IsExpr {
			names[i] = exprSlug(f.Expr)
		} else {
			names[i] = f.Name
		}
	}
	return strings.Join(names, "_")
}

// exprSlug turns an arbitrary SQL expression into an identifier-safe slug
// for use in generated index names. We don't try to keep it readable —
// uniqueness and stability are the only requirements. Lowercase alnum,
// everything else compressed to _.
func exprSlug(expr string) string {
	var b strings.Builder
	prev := byte('_')
	for i := range len(expr) {
		c := expr[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			prev = c
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32) // ASCII tolower
			prev = c + 32
		default:
			if prev != '_' {
				b.WriteByte('_')
				prev = '_'
			}
		}
	}
	out := b.String()
	if out == "" {
		return "expr"
	}
	return out
}

func indexFieldList(fs []dsl.IndexField) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		body := quoteIdent(f.Name)
		if f.IsExpr {
			// Wrap the expression in parens — Postgres requires parens
			// around expression-index targets, e.g. CREATE INDEX ... ((lower(email))).
			// Expression bodies are user-supplied SQL fragments; we don't
			// quote them as identifiers (they're not identifiers).
			body = "(" + f.Expr + ")"
		}
		if f.Desc {
			parts[i] = body + " DESC"
		} else {
			parts[i] = body
		}
	}
	return strings.Join(parts, ", ")
}

// joinQuoted renders a list of identifiers as `"a", "b", "c"` — used wherever
// the SQL grammar wants a parenthesized identifier list (composite PK,
// composite UNIQUE).
func joinQuoted(ids []string) string {
	parts := make([]string, len(ids))
	for i, s := range ids {
		parts[i] = quoteIdent(s)
	}
	return strings.Join(parts, ", ")
}

// snakeCase converts UpperCamelCase to snake_case.
// Delegates to the shared schema package.
func snakeCase(s string) string {
	return schema.SnakeCase(s)
}

// migrationLockTimeoutMS bounds how long a migration waits for a lock.
//
// Long enough that an ordinary short query in flight does not fail the
// migration, short enough that a stuck one does not take the table down with
// it. An operator who hits it retries; an operator who does not have it has no
// signal at all until the table stops answering.
const migrationLockTimeoutMS = 3000

type sqlBuilder struct{ b strings.Builder }

func (s *sqlBuilder) line(line string) {
	s.b.WriteString(line)
	s.b.WriteByte('\n')
}

func (s *sqlBuilder) linef(format string, args ...any) {
	s.line(fmt.Sprintf(format, args...))
}

// commentf writes one `--` comment line, and guarantees it stays one line.
//
// # Why this is a security boundary and not formatting
//
// A `--` comment ends at the first newline, so ANY line terminator inside the
// text ends the comment and puts whatever follows at top level, as SQL.
//
// The migration header says DO NOT EDIT BY HAND, and every comment here is
// generated — but `Detail` carries author text verbatim: the CHECK expression,
// the backfill expression, the index expression and its predicate. The DSL
// lexer turns `\n` in a string literal into a real newline
// (internal/dsl/lexer.go), so a declaration reaches this function already
// carrying one. A review executed the whole chain on PostgreSQL 17.8:
//
//	total int check "total > 0 /*\n; DROP POLICY shop_doc_partition ON shop.doc; --*/"
//
// The expression parses as an ordinary CHECK — the newline is whitespace and
// the block comment is stripped — so every validator in internal/dsl/sqlvalidate
// passes it. Emitted, the second line was top-level SQL, and applying the
// migration dropped the row-level security policy off the table.
//
// That is the exact capability the CHECK gate was written to remove, arriving
// through the comment rather than through the constraint. So the neutralising
// happens HERE, where every Detail channel converges, rather than in each
// validator: a new `Kind` added later cannot forget it.
//
// Every control character goes, not just \n and \r: the set PostgreSQL's
// scanner ends a comment on is not worth re-deriving, and nothing legitimate
// in a schema declaration needs one.
func (s *sqlBuilder) commentf(format string, args ...any) {
	s.line("-- " + sanitizeComment(fmt.Sprintf(format, args...)))
}

// sanitizeComment replaces every control character with a space.
func sanitizeComment(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}

// emitLockTimeout bounds how long the migration will WAIT for a lock.
//
// # The failure this prevents
//
// ApplyMigration runs the whole script in one transaction. DDL here takes
// ACCESS EXCLUSIVE — ALTER TABLE ... ENABLE ROW LEVEL SECURITY does, and so
// does ADD COLUMN — and an ACCESS EXCLUSIVE request that cannot be granted
// QUEUES. Every subsequent statement on that table, including plain SELECTs,
// then queues behind the waiter. So a migration that would have taken
// milliseconds turns into a full outage on the table for as long as one
// unrelated long-running query holds its lock.
//
// Without a timeout there is no upper bound on that. With one, the migration
// fails, rolls back, and can be retried when the table is quieter — which is
// the outcome an operator would choose if asked.
//
// # Which scripts get it
//
// Every script that a caller executes inside a transaction, which is all four
// of Up, Down, PreBackfillUp and PostBackfillUp. It went onto Up and Down
// first, and the backfill path was left without it for a while — the same
// exposure by a different route, since PreBackfillUp carries ADD COLUMN
// (backfill.go runs it on the apply tx) and PostBackfillUp carries ALTER
// COLUMN SET NOT NULL (internal/backfill/runner.go runs it on its own tx).
// Both take ACCESS EXCLUSIVE. A migration is not bounded because the common
// path is bounded.
//
// TestEveryTransactionalScriptBoundsItsLockWait asserts this over the fields of
// SQLScripts by reflection, so a fifth script cannot ship without an answer.
// The index scripts are excluded there for the reason stated at their emit
// site: they run outside a transaction, where SET LOCAL is inert.
//
// # Why this is not a fix for the index build
//
// It bounds the WAIT, not the HOLD. Once granted, the lock is held for the rest
// of the transaction, and CREATE INDEX (not CONCURRENTLY) inside it can run for
// minutes on a large table with reads and writes blocked throughout. The
// concurrent-index channel exists — buildPhaseSplit emits CREATE INDEX
// CONCURRENTLY into a script that runs outside a transaction — but only for
// backfill plans. Routing ordinary DDL through it needs an apply path that can
// leave the transaction, which does not exist yet. Tracked as its own task; a
// review measured 238 ms of blocked reads and writes at 31 MB, which scales.
func emitLockTimeout(b *sqlBuilder) {
	b.line("-- Bound the WAIT for locks. Without this an ACCESS EXCLUSIVE request")
	b.line("-- queues behind any long-running query, and every later statement on")
	b.line("-- the table queues behind the waiter — an outage rather than a wait.")
	b.linef("SET LOCAL lock_timeout = '%dms';", migrationLockTimeoutMS)
	b.blank()
}

func (s *sqlBuilder) blank() {
	s.b.WriteByte('\n')
}

func (s *sqlBuilder) raw(text string) { s.b.WriteString(text) }

func (s *sqlBuilder) String() string { return s.b.String() }

// EmitInitial generates the initial migration that creates every entity in
// newIR from scratch, in topological order so FK targets exist before they're
// referenced. Used by `tidectl plan` for the very first migration on an empty
// database.
func EmitInitial(newIR *dsl.IR) (SQLScripts, error) {
	if newIR == nil {
		return SQLScripts{}, fmt.Errorf("EmitInitial: newIR is required")
	}
	if err := assertCheckNamesUnique(newIR); err != nil {
		return SQLScripts{}, err
	}
	order, err := topoSortEntities(newIR.Entities)
	if err != nil {
		return SQLScripts{}, err
	}
	up := &sqlBuilder{}
	down := &sqlBuilder{}
	up.line("-- atlantis initial migration")
	up.line("CREATE SCHEMA IF NOT EXISTS atlantis;")
	// Emit CREATE EXTENSION only for what the schema actually needs.
	// A vanilla-text schema shouldn't force operators to install
	// pgvector + timescaledb at the OS level.
	for _, ext := range collectRequiredExtensions(newIR) {
		up.linef("CREATE EXTENSION IF NOT EXISTS %s;", ext)
	}
	// Also create any Postgres schemas that entities with explicit
	// `table "schema.table"` overrides require — `public` always exists
	// and `atlantis` was created above. The down does NOT drop these;
	// a non-atlantis schema may hold unrelated objects.
	for _, name := range collectInitialSchemas(newIR) {
		up.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	up.blank()
	down.line("-- atlantis initial migration (down)")
	for _, e := range order {
		emitEntityCreate(up, e)
		up.blank()
	}
	// Drop in reverse FK order on the down side.
	for i := len(order) - 1; i >= 0; i-- {
		emitEntityDrop(down, order[i])
	}
	down.line("DROP SCHEMA IF EXISTS atlantis CASCADE;")
	return SQLScripts{Up: up.String(), Down: down.String()}, nil
}

// topoSortEntities orders entities so that FK target tables are created
// before the entities that reference them. Cycles (self-references aside)
// are reported as an error.
//
// Self-references are tolerated by emitting the entity but issuing the FK
// constraint AFTER the table — we already use named FK constraints in the
// CREATE TABLE statement, which Postgres accepts even for self-references.
// True cycles between two different tables are an error (we'd need
// to emit the constraint with a separate ALTER TABLE — future work).
func topoSortEntities(entities []dsl.Entity) ([]*dsl.Entity, error) {
	byID := map[string]*dsl.Entity{}
	for i := range entities {
		byID[entities[i].ID()] = &entities[i]
	}
	// Stable input order keyed by ID — entities are already sorted by Lower.
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	color := map[string]int{} // 0=white, 1=gray (on stack), 2=black (done)
	var out []*dsl.Entity
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case 1:
			return fmt.Errorf("FK cycle detected involving %s", id)
		case 2:
			return nil
		}
		color[id] = 1
		e := byID[id]
		for _, f := range e.Fields {
			if f.Ref == nil || f.Ref.TargetID == id {
				continue // skip self-references
			}
			if _, ok := byID[f.Ref.TargetID]; !ok {
				continue // validated elsewhere; skip
			}
			if err := visit(f.Ref.TargetID); err != nil {
				return err
			}
		}
		color[id] = 2
		out = append(out, e)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// assertCheckNamesUnique refuses to emit a schema in which two author-supplied
// CHECK names collide on one entity. Postgres rejects such a table, so emitting
// it would produce a migration that cannot apply; the generated SQL would fail
// deep inside an apply transaction with Postgres's own message, naming the
// constraint but not the file it came from.
//
// Generated names never reach here — resolveCheckNames assigns them around
// whatever the author reserved — so this can only fire on something a person
// wrote twice.
//
// It runs on both IRs in EmitSQL, not just the new one, because the down script
// re-creates dropped entities from the old IR.
func assertCheckNamesUnique(ir *dsl.IR) error {
	if ir == nil {
		return nil
	}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		if dup := checkNameCollisions(e); len(dup) > 0 {
			return fmt.Errorf("%s: CHECK constraint name(s) declared more than once: %s "+
				"— Postgres requires constraint names to be unique per table",
				e.ID(), strings.Join(dup, ", "))
		}
	}
	return nil
}

// emitPartitionPolicy renders the row-level security that enforces
// `partition by`.
//
// Enforcement lives in Postgres rather than in each generated read, and that is
// the entire point. The previous design injected a predicate into every
// generated query; the server was later rewritten to dispatch from the IR at
// runtime, the injection was not carried across, and every read silently
// returned every tenant's rows. A policy cannot be forgotten by a handler
// written afterwards, and it covers paths atlantis never sees — custom query
// bodies, backfill expressions, anything executed on that connection.
//
// FORCE is not optional. Without it the table owner bypasses RLS entirely, and
// atlantis owns the tables it creates, so the policy would apply to everyone
// except the one role that actually connects.
//
// The predicate goes through atlantis.current_partition() rather than inlining
// whatever that function reads. That indirection is what let migration 0024
// move the discriminator from a table to a run-time parameter without touching
// a single policy, and the reason it is still required is worth stating
// precisely, because the obvious version of it stopped being true.
//
// The differ does now read `partition by` — diffPartition emits DDL when the
// clause is added, removed or moved. What it compares is DECLARATIONS. A change
// to the POLICY TEXT this function emits is invisible to it: the .atl file is
// byte-identical before and after, so there is no diff, so no migration. Ship a
// version of atlantis that writes a different predicate and every policy
// already in the field keeps the old one, permanently, with nothing able to
// tell you.
//
// So the constraint holds in the shape that matters: whatever this function
// emits has to stay correct forever, and indirection through a function is how
// the mechanism stays changeable when the text cannot be.
func emitPartitionPolicy(b *sqlBuilder, e *dsl.Entity) {
	if e.PartitionField == "" {
		return
	}
	table := qualifiedTable(e)
	col := quoteIdent(e.PartitionField)

	// The discriminator is text, so anything else needs a cast — and WHICH
	// side is cast decides whether this is usable. Measured on PG 17.8 over
	// 200k rows, as a role RLS applies to:
	//
	//   col = current_partition()          text col  0.29 ms,      16 buffers
	//   col::text = current_partition()    uuid col   286 ms,  400182 buffers
	//   col = current_partition()::uuid    uuid col  0.29 ms,      15 buffers
	//
	// Casting the COLUMN defeats the index: the comparison stops being an
	// index key and becomes a per-row filter, and because current_partition()
	// is a function call rather than a constant, it is then invoked once per
	// row scanned. STABLE does not memoise it. Casting the FUNCTION keeps the
	// whole thing an index condition and one call per query.
	//
	// Without any cast, a non-text column does not merely perform badly — the
	// policy cannot be created at all: `operator does not exist: uuid = text`,
	// surfacing at apply time as an opaque Postgres error against DDL nobody
	// hand-wrote.
	discriminator := "atlantis.current_partition()"
	if ct := partitionCastType(e); ct != "" {
		discriminator += "::" + ct
	}

	b.linef("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", table)
	b.linef("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", table)
	// USING gates what a statement may read; WITH CHECK gates what it may
	// write. Both are required: USING alone would let a caller INSERT a row
	// attributed to another tenant, which it could then not see — a write leak
	// rather than a read leak, and just as much a breach.
	// CREATE POLICY has no IF NOT EXISTS, and the initial migration promises to
	// be re-appliable after a partial failure. Dropping first keeps that true.
	b.linef("DROP POLICY IF EXISTS %s ON %s;", quoteIdent(partitionPolicyName(e)), table)
	b.linef("DROP POLICY IF EXISTS %s ON %s;", quoteIdent(partitionDefaultPolicyName(e)), table)

	// TWO policies, and which is which is the whole design.
	//
	// PostgreSQL admits a row when ANY permissive policy allows it AND EVERY
	// restrictive policy allows it. So the tenant boundary goes in the
	// restrictive slot, where it ANDs with everything and nothing can widen
	// past it, and the grant goes in the permissive slot, where it is one of
	// possibly many.
	//
	// It used to be the other way round: one permissive policy carrying the
	// boundary. That works only while it is the sole policy on the table,
	// because a second permissive policy ORs with it — so a user adding
	// `USING (true)` for their own access control silently exposed every
	// tenant. The defence was to refuse the migration outright whenever
	// another permissive policy existed, which held the line and made
	// user-defined access control impossible: the first RBAC rule anyone wrote
	// stopped `tide apply` working on that table.
	//
	// Restrictive inverts that. A user may add whatever permissive policies
	// their authorization model needs; each is a grant, and none of them can
	// reach outside the tenant. Verified against a deliberately hostile
	// `AS PERMISSIVE USING (true) WITH CHECK (true)`: reads returned only the
	// bound tenant's rows, an INSERT attributed to another tenant was refused
	// naming this policy, and a cross-tenant UPDATE touched nothing.
	//
	// USING gates what a statement may read; WITH CHECK what it may write.
	// Both are required on the boundary: USING alone lets a caller INSERT a row
	// attributed to another tenant, which it then cannot see — a write leak
	// rather than a read leak, and just as much a breach.
	b.linef("CREATE POLICY %s ON %s AS RESTRICTIVE USING (%s = %s) WITH CHECK (%s = %s);",
		quoteIdent(partitionPolicyName(e)), table, col, discriminator, col, discriminator)

	// The default grant. Restrictive policies only ever subtract, so a table
	// carrying the boundary alone admits nothing at all — RLS needs at least
	// one permissive policy to let any row through.
	//
	// This one is deliberately total: it reproduces the behaviour of the single
	// permissive policy it replaces, where the tenant check was the only
	// constraint. It is also the policy a user REPLACES when they define their
	// own access control — dropping it and writing narrower grants is the
	// supported path, and doing so cannot weaken tenant isolation because that
	// lives in the restrictive policy above.
	b.linef("CREATE POLICY %s ON %s AS PERMISSIVE USING (true) WITH CHECK (true);",
		quoteIdent(partitionDefaultPolicyName(e)), table)

	// An index on the discriminator, always, owned by the policy.
	//
	// An earlier version of this skipped the index when the schema already
	// declared something leading with the column — a btree, a unique, a
	// composite primary key. That was wrong in the direction that matters, for
	// two reasons found by executing it rather than reading it.
	//
	// First, the constructs it trusted are droppable and this index is not
	// re-emitted. emitPartitionPolicy runs only when the entity is created, so
	// removing the declaration the skip relied on takes the index with it —
	// through a migration classified ADDITIVE, which is the class nobody looks
	// twice at. (The skip inspected e.Indexes and single-column primaries, never
	// e.Uniques, so `unique by` was never actually one of the constructs it
	// trusted — an earlier version of this comment said it was. `index by` and
	// `index partial by` were.) Measured on 200k rows, the end state is 590 ms and 201,673
	// buffers against 0.046 ms and 7. A safety-critical index must not have its
	// lifetime tied to an unrelated declaration somebody may reasonably delete.
	//
	// Second, "leads with the column" is not the same as "can answer
	// col = value". A partial index covers only rows matching its predicate; a
	// gin or hnsw index cannot serve equality at all. Both were accepted, and
	// both measured as sequential scans — 590 ms and 670 ms respectively.
	//
	// The cost of always emitting is a duplicate btree when the author also
	// declared one: bounded, visible in the migration, and removable by the
	// author. The cost of the skip was a silent thousandfold read regression
	// arriving through the safest migration class. IF NOT EXISTS keeps this
	// re-appliable.
	b.linef("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
		quoteIdent(partitionIndexName(e)), table, col)
}

// emitPartitionDisable removes tenant isolation from a table that has it.
//
// Not the exact inverse of emitPartitionPolicy, and not its reverse order —
// an earlier version of this comment claimed both and neither was true. What it
// is: the four statements that undo the four the policy emitter writes, ordered
// so no intermediate state denies every statement.
//
// IF EXISTS throughout, because this is also the DOWN path of the migration
// that added the policy, and a down migration has to survive being run against
// a database where the up half only partly applied.
func emitPartitionDisable(b *sqlBuilder, e *dsl.Entity) {
	if e.PartitionField == "" {
		return
	}
	table := qualifiedTable(e)
	// The switches go BEFORE the policy, not after.
	//
	// An earlier version dropped the policy first and claimed in this comment
	// that the table was "never left in a state where FORCE is on with no
	// policy". A review replayed the statements one at a time and refuted it:
	// after the DROP the table read enabled=true, forced=true, policies=0 — the
	// deny-all state the comment said it avoided. Harmless during apply, which
	// runs the whole script in one transaction, but down_sql is also handed to
	// operators to run by hand.
	//
	// Lifting FORCE and then DISABLE first means the intermediate state is
	// "isolation off, policy still present", which reads as the table did
	// before the policy existed.
	b.linef("ALTER TABLE %s NO FORCE ROW LEVEL SECURITY;", table)
	b.linef("ALTER TABLE %s DISABLE ROW LEVEL SECURITY;", table)
	b.linef("DROP POLICY IF EXISTS %s ON %s;", quoteIdent(partitionPolicyName(e)), table)
	// Both policies, because both were created. Leaving the default grant
	// behind would be harmless while RLS is disabled and would become a
	// permissive `USING (true)` the moment anything re-enabled it.
	b.linef("DROP POLICY IF EXISTS %s ON %s;", quoteIdent(partitionDefaultPolicyName(e)), table)
	emitPartitionIndexDrop(b, e)
}

// emitPartitionIndexDrop drops the index the policy owns.
//
// Separate from emitPartitionDisable because the changed-column case needs the
// drop without the disable: the table keeps its isolation and only the column
// underneath it moves.
func emitPartitionIndexDrop(b *sqlBuilder, e *dsl.Entity) {
	if e.PartitionField == "" {
		return
	}
	b.linef("DROP INDEX IF EXISTS %s.%s;",
		quoteIdent(entitySchema(e)), quoteIdent(partitionIndexName(e)))
}

// partitionCastType returns the SQL type the discriminator must be cast to for
// this entity, or "" when the column is already text-shaped.
func partitionCastType(e *dsl.Entity) string {
	f := e.FindField(e.PartitionField)
	if f == nil {
		return ""
	}
	// An array is never text-shaped, whatever its element is. Checking the
	// rendered string first matched "varchar(20)[]" on its varchar prefix, took
	// the no-cast branch, and emitted a policy PostgreSQL refuses to create:
	// `operator does not exist: character varying[] = text` — the exact failure
	// the cast exists to prevent.
	if f.Type.Array {
		return strings.ToLower(sqlType(f.Type))
	}
	t := strings.ToLower(sqlType(f.Type))
	// text and varchar(n) need no cast. citext does have a citext = text
	// operator, so it would compare without one — but casting the function
	// side to citext is what makes the comparison case-insensitive, which is
	// the whole reason somebody picks citext for a tenant column.
	if t == "text" || strings.HasPrefix(t, "varchar(") || t == "varchar" ||
		strings.HasPrefix(t, "character varying") {
		return ""
	}
	return t
}

// partitionIndexName is the index backing the policy predicate.
//
// Named for its purpose rather than its column, so it is recognisable as the
// policy's own and not mistaken for one the author declared.
//
// Note the residual hazard, which is structural rather than specific to this
// name: the CREATE above uses IF NOT EXISTS, so any collision drops this index
// silently rather than failing. A field literally named `partition` would
// collide, and is only unreachable because `partition` is a reserved token the
// parser rejects.
func partitionIndexName(e *dsl.Entity) string {
	return truncateIdent(tableName(e) + "_partition_idx")
}

// partitionPolicyName is the policy identifier, derived so the differ can find
// it again. Length-capped for the same reason constraint names are.
func partitionPolicyName(e *dsl.Entity) string {
	return truncateIdent(tableName(e) + "_tenant_isolation")
}

// partitionDefaultPolicyName names the permissive grant that sits beside the
// restrictive boundary.
//
// Named for what it is rather than after the mechanism: an operator reading
// pg_policy should be able to tell at a glance which policy they may replace
// (this one) and which is load-bearing (partitionPolicyName). Dropping this one
// and writing narrower grants is the supported way to add access control;
// dropping the other removes tenant isolation.
func partitionDefaultPolicyName(e *dsl.Entity) string {
	return truncateIdent(tableName(e) + "_default_access")
}

// partitionRebuild names an entity whose isolation policy must come down for
// the duration of the migration and go back up at the end.
type partitionRebuild struct {
	entityID   string
	oldE, newE *dsl.Entity
}

// partitionRebuilds returns the entities whose policy must be bracketed.
//
// Only the same-column case. A column MOVE already drops and recreates within
// its own change, and an add or remove has no policy on both sides to bracket.
// What needs bracketing is the case where the policy survives the migration but
// the column underneath it changes shape — PostgreSQL refuses to alter a column
// a policy depends on, so the policy has to be out of the way while it happens.
func partitionRebuilds(d *Diff, newByID, oldByID map[string]*dsl.Entity) []partitionRebuild {
	// Keyed on the OLD partition column being touched, not on the kind of
	// partition change.
	//
	// The first version bracketed only KindPartitionChanged with an unchanged
	// column, and a review executed two shapes that fall outside it and both
	// produce the identical SQLSTATE 0A000 the bracket exists to prevent:
	//
	//	remove `partition by` + widen the same column
	//	move `partition by` to org + widen the OLD column
	//
	// PostgreSQL refuses to alter a column any policy depends on. The invariant
	// is therefore not "the partition column's type changed" — it is NOTHING
	// MAY TOUCH THE OLD PARTITION COLUMN WHILE ITS POLICY STANDS. Selecting on
	// the change kind was picking the symptom that had been reproduced.
	//
	// So: bracket whenever the old side had a policy and this migration alters
	// the column that policy names. What goes back up at the end is whatever
	// the NEW side declares — nothing, if the clause was removed.
	seen := map[string]bool{}
	var out []partitionRebuild
	consider := func(entityID string) {
		if seen[entityID] {
			return
		}
		oldE, newE := oldByID[entityID], newByID[entityID]
		if oldE == nil || newE == nil || oldE.PartitionField == "" {
			return
		}
		if !altersColumn(d, entityID, oldE.PartitionField) {
			return
		}
		seen[entityID] = true
		out = append(out, partitionRebuild{entityID: entityID, oldE: oldE, newE: newE})
	}
	for _, ch := range d.All() {
		consider(ch.EntityID)
	}
	return out
}

// altersColumn reports whether the diff changes the named column of an entity
// in a way PostgreSQL will refuse while a policy depends on it.
//
// A type change is the case that bites; dropping the column takes the policy
// with it via CASCADE and needs no bracket, but is included because a dropped
// partition column with a surviving policy is a state worth never producing.
func altersColumn(d *Diff, entityID, column string) bool {
	for _, ch := range d.All() {
		if ch.EntityID != entityID || ch.Field != column {
			continue
		}
		switch ch.Kind {
		case KindFieldTypeChanged, KindFieldRemoved, KindPartitionChanged:
			return true
		}
	}
	return false
}

// emitForeignPolicyGuard was here, and its deletion is the point of the
// restrictive inversion rather than a side effect of it.
//
// It aborted the migration when the table carried any permissive policy other
// than atlantis's own, because permissive policies OR and a permissive tenant
// boundary could therefore be widened past. That was the correct defence for
// that design, and it made user-defined access control impossible: the first
// RBAC grant anyone wrote stopped `tide apply` working on that table, and the
// error's advice — make your grants RESTRICTIVE — inverts the logic, since
// restrictive policies AND and so cannot express "admins OR auditors".
//
// With the boundary restrictive (see emitPartitionPolicy), a foreign permissive
// policy is exactly what it should be: a grant, bounded by the tenant, and no
// longer anything to refuse.

// sqlLiteral went with it. Its only caller was that guard, which spliced schema
// and policy names into a RAISE. Nothing emitted here needs a runtime string
// literal any more — identifiers go through quoteIdent.

// withoutBracketedPartitionChanges drops the partition changes whose policy the
// prologue/epilogue bracket already owns.
//
// The change stays in the plan and in the operator-facing summary — this only
// removes it from SQL emission, because emitting it there as well produces the
// policy twice and, on the down path, in the wrong order.
func withoutBracketedPartitionChanges(changes []Change, rebuilt []partitionRebuild) []Change {
	if len(rebuilt) == 0 {
		return changes
	}
	bracketed := make(map[string]bool, len(rebuilt))
	for _, r := range rebuilt {
		bracketed[r.entityID] = true
	}
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		switch ch.Kind {
		case KindPartitionAdded, KindPartitionRemoved, KindPartitionChanged:
			if bracketed[ch.EntityID] {
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}
