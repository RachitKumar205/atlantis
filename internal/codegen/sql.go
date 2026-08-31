package codegen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/predsql"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// SQLScripts is the migration output of one emit pass.
//
// Up and Down are the single-script forms plain `tide apply` consumes. The
// PreBackfill and PostBackfill fields and BackfillFields are populated only
// when the diff holds a field with a `backfill` modifier paired with a NOT-NULL
// change. `tide apply --backfill` runs them around the chunked UPDATE:
//
//	PreBackfillUp        additive parts, and ADD COLUMN nullable for backfilled fields
//	PreBackfillIndexes   CREATE INDEX CONCURRENTLY ... WHERE field IS NULL
//	(chunked UPDATE loop, driven by BackfillFields)
//	PostBackfillUp       ALTER COLUMN SET NOT NULL on backfilled fields
//	PostBackfillIndexes  DROP INDEX CONCURRENTLY, mirroring the partial index
//
// For non-backfill plans those four are empty and BackfillFields is nil.
// Callers test PostBackfillUp == "" for the no-phase-split signal.
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

// EmitSQL emits one migration covering every change in d, against the new IR.
// oldIR may be nil for an initial migration; newIR must not be nil. Every up
// script has a matching down, which CI exercises as up/down/up.
//
// Names are schema-qualified and no search_path is set, so the output does not
// depend on the caller's session. Backfill SQL is not generated, and cache and
// query_timeout changes emit a no-op comment so a script holding only those is
// not empty.
func EmitSQL(oldIR, newIR *dsl.IR, d *Diff) (SQLScripts, error) {
	if newIR == nil {
		return SQLScripts{}, fmt.Errorf("EmitSQL: newIR is required")
	}
	// Both sides. The down script re-creates entities dropped in this migration
	// from the *old* IR (emitChange/KindEntityRemoved), so an old entity with
	// two identically-named checks produced a rollback Postgres rejects —
	// EmitInitial refused the very same IR while EmitSQL emitted it happily.
	if err := assertNoTextCarriedArrays(newIR); err != nil {
		return SQLScripts{}, err
	}
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

	// Every schema a newly-added entity names must exist before any CREATE
	// TABLE in it. `atlantis` and `public` are skipped: the infra migrations
	// create the first at boot and the second always exists.
	//
	// Down does not drop schemas. One may hold non-atlantis objects, and a
	// rollback leaving an empty schema is cheaper than that risk.
	for _, name := range collectNewSchemas(d.Additive, newByID) {
		up.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	if len(collectNewSchemas(d.Additive, newByID)) > 0 {
		up.blank()
	}

	// Enum types before any column that names one, and their new labels with
	// them. A type added in the same migration as the table using it has to
	// exist by the time the CREATE TABLE runs.
	if err := emitEnumChanges(up, down, oldIR, newIR, d); err != nil {
		return SQLScripts{}, err
	}

	// Changes are split by class so the reader sees additive first, then
	// backfill-required, then breaking. The down script emits its class groups
	// in reverse, because a rollback has to undo the migration in the opposite
	// order it was applied.
	//
	// Emitting both scripts in the same order produces rollbacks that cannot
	// run. Dropping a column and the entity-level CHECK that references it puts
	// the CHECK removal in ADDITIVE and the column removal in BREAKING; the up
	// script drops the constraint first, and a same-order down script re-adds
	// the constraint before re-adding the column:
	//
	//	ALTER TABLE "zz_zz" ADD CONSTRAINT "zz_total_pos" CHECK (total > 0);
	//	ALTER TABLE "zz_zz" ADD COLUMN "total" INTEGER;
	//	ERROR:  column "total" does not exist
	//
	// This is general, not specific to CHECKs: any dependency spanning two
	// class groups has the same shape.
	// Policies needing a rebuild come down before any class group runs and go
	// back up after all of them.
	//
	// Not as a change competing for position: PostgreSQL refuses to alter a
	// column a policy depends on, and the column change's own class varies with
	// the change, so class-group ordering would place the two correctly only by
	// coincidence. A rebuild constrains the whole migration — nothing may touch
	// that column while the policy stands, and the policy must stand again at
	// the end.
	//
	// Bracketing also makes the down script correct with no second rule, since
	// the down groups are emitted in reverse.
	rebuilt := partitionRebuilds(d, newByID, oldByID)
	for _, r := range rebuilt {
		up.commentf("rebuild tenant isolation on %s: PostgreSQL refuses to alter a column a policy depends on", r.entityID)
		emitPartitionIndexDrop(up, r.oldE)
		// The lock goes up BEFORE the boundary comes down, so no statement
		// boundary in this script has the table open. See emitRebuildLock.
		emitRebuildLock(up, r.oldE)
		up.linef("DROP POLICY IF EXISTS %s ON %s;",
			quoteIdent(partitionPolicyName(r.oldE)), qualifiedTable(r.oldE))
		up.blank()
		emitPartitionIndexDrop(down, r.newE)
		emitRebuildLock(down, r.newE)
		down.linef("DROP POLICY IF EXISTS %s ON %s;",
			quoteIdent(partitionPolicyName(r.newE)), qualifiedTable(r.newE))
		down.blank()
	}

	downGroups := make([]*sqlBuilder, 0, 3)
	for _, g := range []struct {
		label   string
		changes []Change
	}{
		// EVERY group is filtered, not just BREAKING. Being bracketed is a
		// property of the entity, not of a class: a same-column type change
		// whose policy predicate does not move is ADDITIVE.
		//
		// A bracketed entity's partition change is removed because the bracket
		// owns the drop and the recreate. Left in a group it emits the policy
		// twice, and on the DOWN path recreates it before the column is
		// reverted — the SQLSTATE 0A000 the bracket exists to remove.
		{"ADDITIVE", withoutBracketedPartitionChanges(d.Additive, rebuilt)},
		{"BACKFILL REQUIRED", withoutBracketedPartitionChanges(d.BackfillRequired, rebuilt)},
		// Destructive before breaking: a parked object must be out of the way
		// before anything that might recreate a name it still holds.
		{"DESTRUCTIVE — PARKED, REAPED AFTER THE RETENTION WINDOW", withoutBracketedPartitionChanges(d.Destructive, rebuilt)},
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
		// After the boundary is back, never before. Unconditional, because a
		// migration that REMOVES `partition by` emits no boundary here at all
		// and would otherwise leave the lock standing forever.
		emitRebuildUnlock(up, r.oldE, r.newE)
		up.blank()
		emitPartitionPolicy(down, r.oldE)
		emitRebuildUnlock(down, r.newE, r.oldE)
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

	// The index scripts get no lock_timeout. SET LOCAL takes effect only inside
	// a transaction block and these run outside one, so it would be accepted,
	// emit a WARNING, and change nothing.
	//
	// A session-level SET would take effect and is still wrong: CREATE INDEX
	// CONCURRENTLY that times out mid-build leaves an INVALID index that no
	// later run cleans up and every planner ignores while it holds the name.
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

// reorderEntityAddsByFKDependency sorts EntityAdded entries topologically ahead
// of every other change, so an FK between two new entities in one migration
// resolves whatever their alphabetical order.
//
// Other changes target entities that already exist, so their relative order is
// preserved. A cycle between two new entities returns the original order, so
// the apply fails with Postgres's error rather than swallowing the change set.
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
// Each emitter mirrors its own up statement on the down side: CREATE TABLE
// against DROP TABLE, column ADD against column DROP.
//
// The down statements are not re-sorted. migrate runs them as written, and
// per-change reversal suffices because destructive and additive changes never
// share a migration without explicit ceremony.
func emitChange(up, down *sqlBuilder, ch Change, newByID, oldByID map[string]*dsl.Entity) {
	up.commentf("%s: %s", ch.Kind, ch.Detail)
	down.commentf("%s (reversed): %s", ch.Kind, ch.Detail)
	switch ch.Kind {
	case KindEntityAdded:
		e := newByID[ch.EntityID]
		emitEntityCreate(up, e)
		// A real drop, not a park. This undoes a table the same migration
		// created, so the only rows it can lose are ones written since it ran.
		// Parking is for an author deleting a declaration.
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
		// Dropped explicitly before recreation. Both names derive from the
		// table, not the column, so the old index occupies the new one's name
		// and emitPartitionPolicy's CREATE INDEX IF NOT EXISTS would find it
		// still pointing at the column the policy no longer uses. Every read
		// would be a sequential scan under a policy that looks correct.
		//
		// The policy needs no such care: emitPartitionPolicy writes DROP POLICY
		// IF EXISTS under the same name first.
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
		// kind. An unnamed entity check is named from its predicate, so
		// changing the predicate changes the name, which makes it a removal
		// plus an addition rather than a change — different keys, different
		// branch. A Change synthesized without the name makes the drop fall
		// back to hashing the old predicate, emitting DROP CONSTRAINT IF EXISTS
		// on a name that never existed, so the old constraint survives and the
		// ADD collides with it:
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
	// `IF NOT EXISTS` so the initial migration is idempotent: a partially
	// failed run, or an out-of-band repair, can re-apply this file. The
	// diff-driven `tidectl plan` migrations (additive / backfill / breaking)
	// carry no IF NOT EXISTS, so a missing object is an error rather than
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
	// Table-level CHECK constraints only; columnDecl inlines the field-level
	// ones for Postgres to auto-name. Names come from resolveCheckNames, which
	// the differ also uses, so a constraint created here and one a later ALTER
	// targets cannot be named differently.
	//
	// The filter is on the resolved list rather than e.Checks, because entity
	// checks are numbered in the presence of the field ones.
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
// The table moves into the tombstone schema, out of reach because every
// generated statement names atlantis.<table> explicitly, and stays intact until
// a scheduled reaper drops it. Undoing the change moves it back with its rows.
//
// Not CASCADE-equivalent: if another object still depends on the table, the
// move fails and the migration stops rather than removing the dependent.
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
// The table goes to the tombstone schema, out of reach because every generated
// statement names atlantis.<table> explicitly, and stays intact until a reaper
// drops it. Until then the change is undone by moving it back, rows and all.
//
// The only recovery from a real DROP TABLE is a point-in-time restore of the
// whole database.
func emitEntityPark(b *sqlBuilder, e *dsl.Entity) {
	// The PHYSICAL name, not the computed one. `table "consumer.accounts"`
	// overrides it, as 17 of the 19 schemas here do, and SET SCHEMA moves
	// whatever qualifiedTable names, so the table lands in the tombstone schema
	// as `accounts`.
	//
	// A rename addressing the computed `<ns>_<entity>` name finds nothing, and
	// with IF EXISTS is a silent no-op: the table is then stranded under a name
	// that never existed, invisible to the reaper and unrestorable by down.
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
// A column cannot move schema, so it is renamed. Every generated statement
// names its columns explicitly, so a renamed column is absent from reads,
// writes and the proto surface while the data stays. Undoing it is a rename.
//
// Its NOT NULL is dropped: a parked NOT NULL column would block inserts. The
// column is retained for its data, not its rules.
func emitFieldDrop(b *sqlBuilder, e *dsl.Entity, name string) {
	b.linef("ALTER TABLE %s DROP COLUMN %s;", qualifiedTable(e), quoteIdent(name))
}

// emitFieldPark renames a column out of the way instead of dropping it.
//
// A column cannot move schema, so it is renamed. Every generated statement
// names its columns, so a renamed column is absent from reads, writes and the
// proto surface while its data stays put.
//
// Its NOT NULL is dropped: a parked NOT NULL column would reject inserts that
// touch only the live columns.
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
// In the migration rather than alongside it in the server: a park applied but
// not recorded is invisible to the reaper and absent from `tide parked`, so the
// object outlives every retention window with nothing listing it.
func emitParkRegistration(b *sqlBuilder, kind, schemaName, objectName, parentTable, originalSchema, originalName string) {
	parent := "NULL"
	if parentTable != "" {
		parent = sqlStringLiteral(parentTable)
	}
	// originalSchema differs from schema_name for a table: schema_name is the
	// tombstone it now lives in, originalSchema where it goes back to.
	//
	// The conflict target is the partial unique index over live registrations.
	// Untargeted, DO NOTHING would swallow a primary-key conflict too, turning
	// a failed registration into a park applied and never recorded.
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

// checkConstraintName returns the identifier a CHECK change acts on. Resolved
// at diff time and carried on the Change, because only the differ sees both
// sides; an empty name yields invalid SQL that fails at apply.
//
// Deriving it here would scan the new entity, which for a removal cannot hold
// the constraint being removed, then fall back to hashing the predicate. DROP
// CONSTRAINT IF EXISTS on a name that never existed is a no-op, so the plan
// reports the constraint gone while the database still enforces it.
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
// Postgres silently truncates an over-long identifier rather than rejecting it,
// so a CREATE storing a 63-byte prefix and a DROP supplying the full 80-byte
// name match only by accident, and DROP CONSTRAINT IF EXISTS reports success.
//
// The hash suffix keeps distinct inputs distinct: plain truncation collapses
// two long names sharing a prefix, and CREATE TABLE then fails.
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

// unnamedCheckName names an entity-level `check "..."` the author did not name.
// EmitInitial writes it and resolvedChecks derives constraint identity from it;
// disagreement leaves a migrated and a freshly created database with
// differently-named constraints, so every later ALTER targets the wrong one.
//
// Derived from the predicate, not position: deleting the first of two unnamed
// checks renumbers the second, reporting a spurious predicate change and
// re-validating the table under ACCESS EXCLUSIVE. Truncated to four bytes.
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
// No NOT VALID. Classifying this backfill-required means the constraint is
// validated against existing rows, so the apply fails on data that violates it
// rather than leaving an unenforced constraint behind. A NOT VALID variant
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

// emitCompositeUnique adds or drops a multi-column UNIQUE constraint. The name
// comes from compositeUniqueName, so the DROP on `on=false` finds exactly what
// a prior ADD created.
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

// serialType names the sequence-backed spelling for an integer width.
//
// format_type reports a SERIAL column as its underlying integer, so a
// declaration and the column it created compare equal at drift.
func serialType(name string) string {
	switch name {
	case "smallint":
		return "SMALLSERIAL"
	case "int":
		return "SERIAL"
	}
	return "BIGSERIAL"
}

func tableName(e *dsl.Entity) string {
	return schema.TableName(e)
}

// columnDecl renders one column line for CREATE TABLE / ADD COLUMN.
//
// Identity strategy precedence:
//
//	serial   → render `SMALLSERIAL`/`SERIAL`/`BIGSERIAL` as the type itself
//	           (each carries the sequence + NOT NULL + DEFAULT nextval(...)
//	           implicitly).
//	identity → render `<type> GENERATED ALWAYS AS IDENTITY`.
//	neither  → render `<type>` with explicit NOT NULL / DEFAULT modifiers.
func columnDecl(f dsl.Field, checkName string) string {
	var parts []string
	switch {
	case f.Serial:
		// The serial spelling replaces both the type and the GENERATED clause.
		parts = []string{quoteIdent(f.Name), serialType(f.Type.Name)}
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
		// Postgres auto-names an inline single-column check
		// <table>_<column>_check and, when that exceeds the 63-byte identifier
		// limit, shortens the table and column parts proportionally
		// (makeObjectName), which is not reproducible by truncation:
		//
		//	postgres:  zzlongns_an_entity_with_a_reall_a_column_with_a_long_name_check
		//	truncated: zzlongns_an_entity_with_a_really_quite_long_name_a_column_with_a_long_name_check
		//
		// A later DROP CONSTRAINT IF EXISTS carrying the 80-byte name is
		// truncated by Postgres to a different 63-byte string, IF EXISTS
		// swallows the miss, and the migration reports success with the
		// constraint still enforcing. Emitting the name removes the guess.
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

// tableNameFromID converts a canonical "namespace.Entity" into the flat table
// name, `<namespace>_<snake>`.
func tableNameFromID(id string) string {
	parts := strings.SplitN(id, ".", 2)
	if len(parts) != 2 {
		return id
	}
	return parts[0] + "_" + snakeCase(parts[1])
}

// Constraint and index names are deterministic, so DROP CONSTRAINT and DROP
// INDEX need no record of Postgres's auto-generated ones.
//
// Each fits the 63-character identifier limit; past it, truncateIdent replaces
// the tail with a hash suffix.

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
// A `--` comment ends at the first newline, so any line terminator inside the
// text ends the comment and puts whatever follows at top level, as SQL.
//
// The migration header says DO NOT EDIT BY HAND, and every comment here is
// generated — but `Detail` carries author text verbatim: the CHECK expression,
// the backfill expression, the index expression and its predicate. The DSL
// lexer turns `\n` in a string literal into a real newline
// (internal/dsl/lexer.go), so a declaration reaches this function already
// carrying one. On PostgreSQL 17.8:
//
//	total int check "total > 0 /*\n; DROP POLICY shop_doc_partition ON shop.doc; --*/"
//
// The expression parses as an ordinary CHECK — the newline is whitespace and
// the block comment is stripped — so every validator in internal/dsl/sqlvalidate
// passes it. Emitted, the second line is top-level SQL, and applying the
// migration drops the row-level security policy off the table.
//
// The neutralising happens here, where every Detail channel converges, rather
// than in each validator, so a new `Kind` added later cannot forget it.
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
// ApplyMigration runs the script in one transaction and the DDL takes ACCESS
// EXCLUSIVE. A request that cannot be granted queues, and every later statement
// on that table queues behind it, so one long-running query turns a millisecond
// migration into an outage. It bounds the WAIT, not the HOLD: CREATE INDEX
// inside the transaction still blocks for its duration, 238 ms on 31 MB.
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
	if err := assertNoTextCarriedArrays(newIR); err != nil {
		return SQLScripts{}, err
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
	// Enum types before the tables whose columns name them.
	//
	// CREATE TYPE takes no IF NOT EXISTS, so a re-run of the same migration
	// would fail on an existing type; the down drops them, and apply runs a
	// migration once.
	for i := range newIR.Enums {
		emitEnumCreate(up, &newIR.Enums[i])
	}
	if len(newIR.Enums) > 0 {
		up.blank()
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

// emitEnumCreate writes the CREATE TYPE for one enum.
//
// Labels are single-quoted with embedded quotes doubled: Postgres accepts any
// text as a label, so a schema holding `it's` is ordinary rather than hostile.
func emitEnumCreate(b *sqlBuilder, e *dsl.Enum) {
	labels := make([]string, len(e.Values))
	for i, v := range e.Values {
		labels[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	b.linef("CREATE TYPE %s AS ENUM (%s);", schema.QualifiedEnum(e), strings.Join(labels, ", "))
}

// topoSortEntities orders entities so that FK target tables are created
// before the entities that reference them. Cycles (self-references aside)
// are reported as an error.
//
// Self-references are tolerated: the FK constraint is named inside CREATE
// TABLE, which Postgres accepts. A cycle between two different tables is an
// error, and would need the constraint emitted as a separate ALTER TABLE.
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

// assertNoTextCarriedArrays refuses an array whose element is carried by a text
// codec.
//
// pgx resolves an array's codec from its element's, so replacing the element
// codec makes `inet[]` decode the binary array body as text: the scan succeeds
// and returns the wire bytes reinterpreted as characters. Measured on
// PostgreSQL 17 — `point[]` reads back as "?\ufffd\t@\t" rather than
// "(1,2)".
//
// Without the codec the same read fails with "cannot scan _inet in binary
// format into *[]string", so refusing here is what keeps a silent wrong answer
// from replacing a loud one.
func assertNoTextCarriedArrays(ir *dsl.IR) error {
	if ir == nil {
		return nil
	}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		for _, f := range e.Fields {
			if !f.Type.Array || f.Type.Elem == nil {
				continue
			}
			if coltype.CarriedAsText(*f.Type.Elem) {
				return fmt.Errorf("%s.%s is an array of %s, which atlantis carries "+
					"as text — pgx decodes the array body with the element's codec, "+
					"so the values read back as the wire bytes. Store them as text[] "+
					"and cast in a custom query",
					e.ID(), f.Name, f.Type.Elem.Name)
			}
		}
	}
	return nil
}

// assertCheckNamesUnique refuses to emit a schema in which two author-supplied
// CHECK names collide on one entity. Postgres rejects such a table, so the
// migration would fail inside the apply transaction with a message naming the
// constraint but not the file it came from.
//
// Only an author-written duplicate reaches here; resolveCheckNames assigns
// generated names around whatever the author reserved. EmitSQL runs it on both
// IRs, because the down script re-creates dropped entities from the old one.
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
// `partition by`. Enforcement is in Postgres, not in each generated read, so a
// policy cannot be omitted by a handler written later and covers paths atlantis
// never sees.
//
// FORCE is required; without it the table owner bypasses RLS. What this emits
// is permanent: the differ compares declarations, so changing the policy text
// produces no migration.
func emitPartitionPolicy(b *sqlBuilder, e *dsl.Entity) {
	if e.PartitionField == "" {
		return
	}
	table := qualifiedTable(e)
	col := quoteIdent(e.PartitionField)

	// The discriminator is text, and which side carries the cast decides
	// whether the policy is usable. Measured on PG 17.8 over 200k rows, as a
	// role RLS applies to:
	//
	//	col = current_partition()          text col  0.29 ms,      16 buffers
	//	col::text = current_partition()    uuid col   286 ms,  400182 buffers
	//	col = current_partition()::uuid    uuid col  0.29 ms,      15 buffers
	//
	// Casting the COLUMN drops the index condition to a per-row filter, and
	// current_partition() is a function call rather than a constant, so it runs
	// once per row scanned; STABLE does not memoise it.
	//
	// Without any cast the policy cannot be created at all:
	// `operator does not exist: uuid = text`.
	predicate := partitionPolicyPredicate(e)

	b.linef("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", table)
	b.linef("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", table)
	// USING gates what a statement may read; WITH CHECK gates what it may
	// CREATE POLICY has no IF NOT EXISTS and the initial migration must stay
	// re-appliable after a partial failure, so the boundary is dropped first.
	//
	// The boundary only. The default grant below uses a catalogue check
	// instead, because dropping that one is a user's supported action.
	b.linef("DROP POLICY IF EXISTS %s ON %s;", quoteIdent(partitionPolicyName(e)), table)

	// The boundary is RESTRICTIVE, the grant PERMISSIVE. PostgreSQL admits a
	// row when any permissive policy allows it and every restrictive one does,
	// so the boundary ANDs with everything; a permissive boundary would OR with
	// a user's own `USING (true)` and expose every tenant.
	//
	// USING gates reads, WITH CHECK writes. Omitting WITH CHECK is safe —
	// PostgreSQL reuses USING, verified on 17 — but an explicit
	// `WITH CHECK (true)` leaks, so both are written out.
	b.linef("CREATE POLICY %s ON %s AS RESTRICTIVE USING (%s) WITH CHECK (%s);",
		quoteIdent(partitionPolicyName(e)), table, predicate, predicate)

	// The default grant, created only when the table would otherwise admit
	// nothing: restrictive policies only subtract, and RLS needs at least one
	// permissive policy to pass any row.
	//
	// Conditional, hence the DO block. A user may drop this and write narrower
	// permissive grants; re-creating `USING (true)` beside them would OR with
	// each and nullify all of them. The condition asks whether any permissive
	// policy exists, not whether this one is missing.
	tableLit := "'" + strings.ReplaceAll(table, "'", "''") + "'"
	b.line("DO $atlantis_default_access$")
	b.line("BEGIN")
	b.linef("    IF NOT EXISTS (SELECT 1 FROM pg_policy"+
		" WHERE polrelid = %s::regclass AND polpermissive) THEN", tableLit)
	b.linef("        CREATE POLICY %s ON %s AS PERMISSIVE USING (true) WITH CHECK (true);",
		quoteIdent(partitionDefaultPolicyName(e)), table)
	b.line("    END IF;")
	b.line("END")
	b.line("$atlantis_default_access$;")

	// Always emitted, never skipped because a declared index leads with the
	// column. Declared indexes are droppable and this one is not re-emitted —
	// emitPartitionPolicy runs only at entity creation — so removing the
	// declaration takes the index through an ADDITIVE migration. Measured on
	// 200k rows: 590 ms and 201,673 buffers against 0.046 ms and 7.
	//
	// "Leads with the column" is not "can answer col = value" either: a partial
	// index covers only matching rows, and gin and hnsw cannot serve equality.
	b.linef("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
		quoteIdent(partitionIndexName(e)), table, col)
}

// emitPartitionDisable removes tenant isolation from a table that has it.
//
// Four statements undoing the four emitPartitionPolicy writes, ordered so no
// intermediate state denies every statement. Neither the exact inverse nor the
// reverse order.
//
// IF EXISTS throughout: this is the DOWN path of the migration that added the
// policy, and must survive a partly-applied up half.
func emitPartitionDisable(b *sqlBuilder, e *dsl.Entity) {
	if e.PartitionField == "" {
		return
	}
	table := qualifiedTable(e)
	// The switches go BEFORE the policy. Dropping the policy first leaves
	// enabled=true, forced=true, policies=0 — deny-all. Harmless inside apply's
	// single transaction, but down_sql is also run by hand.
	//
	// Lifting FORCE then DISABLE makes the intermediate state "isolation off,
	// policy still present".
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

// partitionPolicyPredicate renders the boundary policy's condition, and is its
// single definition. emitPartitionPolicy writes it into CREATE POLICY and
// diffPartition compares old and new forms to decide whether a change alters
// what a caller can read; two renderings would be free to disagree silently.
//
// The cast is part of the predicate: it makes `varchar` -> `uuid` a real change
// to what the policy matches, while `varchar(16)` -> `varchar(32)` leaves the
// text byte-identical.
func partitionPolicyPredicate(e *dsl.Entity) string {
	if e == nil || e.PartitionField == "" {
		return ""
	}
	discriminator := "atlantis.current_partition()"
	if ct := partitionCastType(e); ct != "" {
		discriminator += "::" + ct
	}
	return quoteIdent(e.PartitionField) + " = " + discriminator
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
// Named for its purpose rather than its column, so it is not mistaken for one
// the author declared.
//
// The CREATE above uses IF NOT EXISTS, so a name collision drops this index
// silently. A field named `partition` would collide, and is unreachable only
// because the parser rejects that token.
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
// Dropping this policy and writing narrower grants is the supported way to add
// access control. Dropping partitionPolicyName removes tenant isolation.
func partitionDefaultPolicyName(e *dsl.Entity) string {
	return truncateIdent(tableName(e) + "_default_access")
}

// partitionRebuildLockName names the deny-all policy that stands in for the
// boundary while the boundary is down.
func partitionRebuildLockName(e *dsl.Entity) string {
	return truncateIdent(tableName(e) + "_rebuild_lock")
}

// emitRebuildLock closes the table before the tenant boundary comes down.
//
// The bracket must drop the boundary, since PostgreSQL refuses to alter a
// column a policy depends on, leaving RLS enabled with no boundary and the
// permissive grant still admitting rows.
//
// A RESTRICTIVE `USING (false)` ANDs with every permissive policy and names no
// column, so it does not block the ALTER.
func emitRebuildLock(b *sqlBuilder, e *dsl.Entity) {
	table := qualifiedTable(e)
	name := quoteIdent(partitionRebuildLockName(e))
	b.linef("DROP POLICY IF EXISTS %s ON %s;", name, table)
	b.linef("CREATE POLICY %s ON %s AS RESTRICTIVE USING (false) WITH CHECK (false);", name, table)
}

// emitRebuildUnlock reopens the table once the boundary is back.
//
// nameFrom is the entity the lock was named after, tableFrom where the table
// lives by now; they differ only if the migration moved it.
//
// Unconditional, not folded into emitPartitionPolicy: removing `partition by`
// creates no policy, and a lock dropped only alongside a boundary would remain
// as a permanent deny-all.
func emitRebuildUnlock(b *sqlBuilder, nameFrom, tableFrom *dsl.Entity) {
	b.linef("DROP POLICY IF EXISTS %s ON %s;",
		quoteIdent(partitionRebuildLockName(nameFrom)), qualifiedTable(tableFrom))
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
	// Bracketing only KindPartitionChanged with an unchanged column misses two
	// shapes that produce the same SQLSTATE 0A000:
	//
	//	remove `partition by` + widen the same column
	//	move `partition by` to org + widen the OLD column
	//
	// PostgreSQL refuses to alter a column any policy depends on, so the
	// invariant is not "the partition column's type changed" but "nothing may
	// touch the old partition column while its policy stands".
	//
	// Bracket whenever the old side had a policy and this migration alters the
	// column that policy names. What goes back up at the end is whatever the
	// new side declares — nothing, if the clause was removed.
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

// emitEnumChanges writes the DDL for enum types added and labels appended, and
// refuses the two shapes Postgres cannot perform.
//
// A removed label has no DDL: Postgres has no ALTER TYPE ... DROP VALUE, so the
// change is reported by the differ and refused here rather than emitted as
// something that would not run.
//
// ALTER TYPE ... ADD VALUE is accepted inside a transaction block from PG12,
// but the added label cannot be used until that transaction commits. Apply runs
// its DDL in one transaction, so a migration that both adds a label and writes
// it — as a column default — is refused with that reason instead of failing at
// the database as `unsafe use of new value`.
//
// Postgres exempts a type CREATEd in the same transaction, which is why only
// KindEnumValueAdded is checked: a column defaulting to a label of a type this
// same migration creates is legal, and refusing it would cost a migration that
// runs.
func emitEnumChanges(up, down *sqlBuilder, oldIR, newIR *dsl.IR, d *Diff) error {
	oldByID := indexEnums(oldIR)
	newByID := indexEnums(newIR)

	var wrote bool
	for _, c := range d.All() {
		switch c.Kind {
		case KindEnumValueRemoved:
			return fmt.Errorf("%s: %s. Add a new type with the labels you want, "+
				"move the columns to it, and drop the old one", c.EntityID, c.Detail)

		case KindEnumAdded:
			e, ok := newByID[c.EntityID]
			if !ok {
				continue
			}
			emitEnumCreate(up, e)
			down.linef("DROP TYPE IF EXISTS %s;", schema.QualifiedEnum(e))
			wrote = true

		case KindEnumRemoved:
			e, ok := oldByID[c.EntityID]
			if !ok {
				continue
			}
			up.linef("DROP TYPE IF EXISTS %s;", schema.QualifiedEnum(e))
			emitEnumCreate(down, e)
			wrote = true

		case KindEnumValueAdded:
			e, ok := newByID[c.EntityID]
			if !ok {
				continue
			}
			label, ok := addedLabel(c.Detail)
			if !ok {
				continue
			}
			if err := refuseLabelUsedWhenAdded(newIR, c.EntityID, label); err != nil {
				return err
			}
			up.linef("ALTER TYPE %s ADD VALUE IF NOT EXISTS '%s';",
				schema.QualifiedEnum(e), strings.ReplaceAll(label, "'", "''"))
			// No down: Postgres cannot remove a label, so a rollback leaves the
			// type wider than it was. Nothing reads a label no column holds.
			down.linef("-- %s keeps the label %q: Postgres has no ALTER TYPE DROP VALUE.",
				schema.QualifiedEnum(e), label)
			wrote = true
		}
	}
	if wrote {
		up.blank()
	}
	return nil
}

// addedLabel reads the label back out of a KindEnumValueAdded detail.
func addedLabel(detail string) (string, bool) {
	i := strings.IndexByte(detail, '"')
	if i < 0 {
		return "", false
	}
	j := strings.IndexByte(detail[i+1:], '"')
	if j < 0 {
		return "", false
	}
	return detail[i+1 : i+1+j], true
}

// refuseLabelUsedWhenAdded rejects a migration that adds a label and writes it
// in the same transaction.
func refuseLabelUsedWhenAdded(ir *dsl.IR, enumID, label string) error {
	for i := range ir.Entities {
		e := &ir.Entities[i]
		for _, f := range e.Fields {
			if !f.Type.Enum || f.Type.Name != enumID || f.Default == nil {
				continue
			}
			if f.Default.Kind == dsl.DefaultIRString && f.Default.Str == label {
				return fmt.Errorf("%s.%s defaults to %q, a label this migration adds "+
					"to %s. Postgres refuses a new enum label in the transaction that "+
					"added it; add the label first, then the default",
					e.ID(), f.Name, label, enumID)
			}
		}
	}
	return nil
}
