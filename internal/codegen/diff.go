// Package codegen takes a dsl.IR and emits the artifacts callers consume:
// SQL migrations, .proto files, gRPC server stubs, typed clients, and cache
// key derivation. It also diffs two IRs to classify schema changes.
package codegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/predsql"
)

// ChangeClass selects how a change must be applied.
type ChangeClass int

const (
	// ClassAdditive is auto-applied by `tide apply`. Examples: add entity,
	// add nullable field, add index, loosen NOT NULL, widen a type, add
	// default, add cache hint.
	ClassAdditive ChangeClass = iota

	// ClassBackfillRequired needs explicit backfill SQL before applying.
	// Examples: NOT NULL on an existing column, type narrowing, FK on a
	// populated table.
	ClassBackfillRequired

	// ClassCrossCallerBreaking opens a PR in atlantis. Examples: drop a
	// field other callers may read, remove an entity, rename a field/entity.
	// The diff engine does not know which callers are pinned — the Admin
	// service composes that knowledge with this classification to decide
	// whether to auto-apply or escalate.
	ClassCrossCallerBreaking
)

// String returns the human-readable label for the class.
func (c ChangeClass) String() string {
	switch c {
	case ClassAdditive:
		return "additive"
	case ClassBackfillRequired:
		return "backfill-required"
	case ClassCrossCallerBreaking:
		return "cross-caller-breaking"
	}
	return "unknown"
}

// ChangeKind identifies the structural shape of one diff entry. Together with
// ChangeClass these drive the SQL emitter.
type ChangeKind string

const (
	KindEntityAdded   ChangeKind = "entity_added"
	KindEntityRemoved ChangeKind = "entity_removed"

	// KindEntityTableChanged: the entity's `table "<schema.table>"` value
	// moved (or appeared, or disappeared). Atlantis won't auto-rename the
	// physical table; an operator runs their own ALTER TABLE RENAME (or
	// follows whichever data-migration path they prefer) before re-applying.
	KindEntityTableChanged ChangeKind = "entity_table_changed"

	KindFieldAdded   ChangeKind = "field_added"
	KindFieldRemoved ChangeKind = "field_removed"

	// KindFieldNotNullTightened: NOT NULL added to existing nullable column.
	KindFieldNotNullTightened ChangeKind = "field_not_null_tightened"
	// KindFieldNotNullLoosened: NOT NULL removed.
	KindFieldNotNullLoosened ChangeKind = "field_not_null_loosened"

	KindFieldTypeChanged       ChangeKind = "field_type_changed"
	KindFieldDefaultChanged    ChangeKind = "field_default_changed"
	KindFieldUniqueAdded       ChangeKind = "field_unique_added"
	KindFieldUniqueRemoved     ChangeKind = "field_unique_removed"
	KindFieldReferenceAdded    ChangeKind = "field_reference_added"
	KindFieldReferenceRemoved  ChangeKind = "field_reference_removed"
	KindFieldReferenceModified ChangeKind = "field_reference_modified"

	KindIndexAdded   ChangeKind = "index_added"
	KindIndexRemoved ChangeKind = "index_removed"

	// KindCompositeUnique{Added,Removed}: a multi-column `unique by a, b`
	// constraint appeared or disappeared. Adding is backfill-required (may
	// fail on existing duplicate tuples); removing is additive. Field-level
	// uniqueness has its own KindFieldUnique* kinds; these cover only the
	// entity-level composite spec, which diffEntity otherwise never diffed.
	KindCompositeUniqueAdded   ChangeKind = "composite_unique_added"
	KindCompositeUniqueRemoved ChangeKind = "composite_unique_removed"

	// CHECK constraints, entity-level (`check "..."`) and per-field (`check`).
	// Added and Changed are backfill-required: ADD CONSTRAINT without NOT VALID
	// takes ACCESS EXCLUSIVE and validates every existing row, so both can fail
	// on data. Removed is additive — rows the constraint rejected become legal.
	// KindChunkTimeIntervalChanged carries a hypertable's chunk sizing.
	//
	// create_hypertable's chunk_time_interval only applies at creation, and the
	// call is emitted with if_not_exists, so re-running it against an existing
	// hypertable changes nothing. Without this the clause would work on a fresh
	// table and silently no-op on every edit — a clause that appears to work,
	// which is worse than one that plainly does not.
	KindChunkTimeIntervalChanged ChangeKind = "chunk_time_interval_changed"

	KindCheckAdded   ChangeKind = "check_added"
	KindCheckRemoved ChangeKind = "check_removed"
	KindCheckChanged ChangeKind = "check_changed"

	// KindFieldSerialAdded: BIGSERIAL added to an existing column.
	// Sequence must be seeded to MAX(col)+1 before apply or new inserts
	// collide with existing rows.
	KindFieldSerialAdded ChangeKind = "field_serial_added"
	// KindFieldSerialRemoved: BIGSERIAL removed. Callers that relied on
	// auto-increment must now supply the column explicitly.
	KindFieldSerialRemoved ChangeKind = "field_serial_removed"

	// KindFieldBackfill* track changes to the `backfill "<expr>"` field
	// modifier. The modifier itself causes no schema change — it's the
	// signal `tide apply --backfill` uses to know how to populate the
	// column when an associated NOT NULL or new-NOT-NULL change needs
	// existing-row data. Classified Additive on its own; the apply-time
	// rejection comes from the paired NotNull change.
	KindFieldBackfillAdded   ChangeKind = "field_backfill_added"
	KindFieldBackfillRemoved ChangeKind = "field_backfill_removed"
	KindFieldBackfillChanged ChangeKind = "field_backfill_changed"

	KindCacheChanged ChangeKind = "cache_changed"

	KindQueryTimeoutChanged ChangeKind = "query_timeout_changed"

	// Custom query / procedure changes. These carry NO DDL — custom decls
	// are served at runtime from the checkpoint IR, not migrated — but they
	// ARE real changes (validated + persisted), so the diff must surface
	// them or a procedure-only apply misreports as "0 changes". EntityID
	// holds the decl's "namespace.Name" id.
	KindCustomQueryAdded   ChangeKind = "custom_query_added"
	KindCustomQueryRemoved ChangeKind = "custom_query_removed"
	KindCustomQueryChanged ChangeKind = "custom_query_changed"
	KindProcedureAdded     ChangeKind = "procedure_added"
	KindProcedureRemoved   ChangeKind = "procedure_removed"
	KindProcedureChanged   ChangeKind = "procedure_changed"
)

// Change is one structural difference between two IRs.
type Change struct {
	Kind     ChangeKind  `json:"kind"`
	Class    ChangeClass `json:"class"`
	EntityID string      `json:"entity_id"`        // namespace.Name
	Field    string      `json:"field,omitempty"`  // for field/index changes
	Detail   string      `json:"detail,omitempty"` // human-readable summary
	From     any         `json:"from,omitempty"`
	To       any         `json:"to,omitempty"`

	// Constraint is the database identifier this change acts on, for kinds
	// where the emitter must name one (today: the CHECK kinds).
	//
	// Resolved here rather than in the emitter because the emitter is handed
	// the *new* entity, and a removal's constraint exists only on the old one.
	// Deriving the name there meant scanning an entity that did not contain the
	// constraint, falling through to a hash of the predicate, and emitting
	// DROP CONSTRAINT IF EXISTS <name-that-never-existed> — which, because of
	// the IF EXISTS, dropped nothing and reported success. The plan said the
	// constraint was gone; it was still enforcing.
	Constraint string `json:"constraint,omitempty"`
}

// Diff is the full set of changes between two IRs, partitioned by class.
type Diff struct {
	Additive         []Change `json:"additive,omitempty"`
	BackfillRequired []Change `json:"backfill_required,omitempty"`
	Breaking         []Change `json:"breaking,omitempty"`
}

// IsEmpty reports whether the diff has no changes.
func (d *Diff) IsEmpty() bool {
	return len(d.Additive)+len(d.BackfillRequired)+len(d.Breaking) == 0
}

// HighestClass returns the most-restrictive class present, which drives
// whether `tide apply` can auto-apply, requires backfill, or escalates to a PR.
func (d *Diff) HighestClass() ChangeClass {
	if len(d.Breaking) > 0 {
		return ClassCrossCallerBreaking
	}
	if len(d.BackfillRequired) > 0 {
		return ClassBackfillRequired
	}
	return ClassAdditive
}

// diffCtx carries optional caller-ownership context into the diff engine.
// When populated, removals of entities/fields owned exclusively by the
// submitting caller (with no cross-caller references) are downgraded from
// ClassCrossCallerBreaking to ClassAdditive.
type diffCtx struct {
	submittingCaller string
	entityOwnership  map[string]string // entityID → caller who declared it
	crossCallerRefs  map[string]bool   // "entityID" or "entityID.fieldName" → referenced by another caller
}

// DiffOption configures optional behavior of ComputeDiff.
type DiffOption func(*diffCtx)

// WithCallerContext supplies per-caller ownership and cross-reference data
// so the diff engine can downgrade removals that only affect the submitting
// caller from ClassCrossCallerBreaking to ClassAdditive.
func WithCallerContext(caller string, ownership map[string]string, refs map[string]bool) DiffOption {
	return func(c *diffCtx) {
		c.submittingCaller = caller
		c.entityOwnership = ownership
		c.crossCallerRefs = refs
	}
}

// classifyRemoval returns ClassAdditive when the submitting caller owns the
// entity and no other caller references the given key (entityID or
// entityID.field). Falls back to ClassCrossCallerBreaking when context is
// absent or conditions aren't met.
func (ctx *diffCtx) classifyRemoval(entityID, refKey string) ChangeClass {
	if ctx.submittingCaller == "" {
		return ClassCrossCallerBreaking
	}
	owner, ok := ctx.entityOwnership[entityID]
	if !ok || owner != ctx.submittingCaller {
		return ClassCrossCallerBreaking
	}
	if ctx.crossCallerRefs[refKey] {
		return ClassCrossCallerBreaking
	}
	return ClassAdditive
}

// ComputeDiff diffs old → new. Either IR may be nil; nil means "no schema yet".
//
// Determinism: the returned change order is stable (entities sorted by ID,
// fields by name within each entity, then index/cache changes), which is
// important because the SQL emitter and tests depend on it.
//
// The variadic opts parameter accepts DiffOption values. When no options are
// supplied, the engine uses the conservative default: every removal is
// classified ClassCrossCallerBreaking.
func ComputeDiff(oldIR, newIR *dsl.IR, opts ...DiffOption) *Diff {
	ctx := &diffCtx{}
	for _, o := range opts {
		o(ctx)
	}

	d := &Diff{}
	oldByID := indexByID(oldIR)
	newByID := indexByID(newIR)

	// Stable union of entity IDs.
	ids := make([]string, 0, len(oldByID)+len(newByID))
	seen := map[string]bool{}
	for id := range oldByID {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	for id := range newByID {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		oldE, hasOld := oldByID[id]
		newE, hasNew := newByID[id]
		switch {
		case hasOld && !hasNew:
			// Entity removed. When caller context is available and the
			// submitting caller owns this entity with no cross-caller
			// references, this is safe to auto-apply. Otherwise flag as
			// cross-caller-breaking (the conservative default).
			class := ctx.classifyRemoval(id, id)
			d.append(Change{
				Kind:     KindEntityRemoved,
				Class:    class,
				EntityID: id,
				Detail:   "entity removed",
				From:     oldE,
			})
		case !hasOld && hasNew:
			d.Additive = append(d.Additive, Change{
				Kind:     KindEntityAdded,
				Class:    ClassAdditive,
				EntityID: id,
				Detail:   "entity added",
				To:       newE,
			})
		default:
			diffEntity(oldE, newE, d, ctx)
		}
	}

	// Custom queries and procedures aren't entities and emit no DDL, but a
	// change to one is a real, persisted change — diff it so plan/apply
	// don't report "0 changes" for a procedure-only edit.
	diffCustomDecls(oldIR, newIR, d)

	return d
}

// diffCustomDecls compares the custom-query and procedure sets by id, and by
// canonical content for those present on both sides. All changes are
// additive (no DDL, no stored-data impact); the Detail spells out the
// runtime effect — a changed decl hot-reloads on apply, while a brand-new
// one isn't dispatchable until the server restarts (its gRPC method is
// registered at startup only).
func diffCustomDecls(oldIR, newIR *dsl.IR, d *Diff) {
	oldQ, newQ := indexQueries(oldIR), indexQueries(newIR)
	for id, q := range newQ {
		old, ok := oldQ[id]
		switch {
		case !ok:
			d.Additive = append(d.Additive, Change{
				Kind: KindCustomQueryAdded, Class: ClassAdditive, EntityID: id,
				Detail: "custom query added (its RPC registers on the next server restart)",
			})
		case !customQueryContentEqual(old, q):
			d.Additive = append(d.Additive, Change{
				Kind: KindCustomQueryChanged, Class: ClassAdditive, EntityID: id,
				Detail: "custom query changed (hot-reloads on apply)",
			})
		}
	}
	for id := range oldQ {
		if _, ok := newQ[id]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind: KindCustomQueryRemoved, Class: ClassAdditive, EntityID: id,
				Detail: "custom query removed",
			})
		}
	}

	oldP, newP := indexProcedures(oldIR), indexProcedures(newIR)
	for id, p := range newP {
		old, ok := oldP[id]
		switch {
		case !ok:
			d.Additive = append(d.Additive, Change{
				Kind: KindProcedureAdded, Class: ClassAdditive, EntityID: id,
				Detail: "procedure added (its RPC registers on the next server restart)",
			})
		case !customProcContentEqual(old, p):
			d.Additive = append(d.Additive, Change{
				Kind: KindProcedureChanged, Class: ClassAdditive, EntityID: id,
				Detail: "procedure changed (hot-reloads on apply)",
			})
		}
	}
	for id := range oldP {
		if _, ok := newP[id]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind: KindProcedureRemoved, Class: ClassAdditive, EntityID: id,
				Detail: "procedure removed",
			})
		}
	}
}

func indexQueries(ir *dsl.IR) map[string]*dsl.CustomQuery {
	if ir == nil {
		return nil
	}
	out := make(map[string]*dsl.CustomQuery, len(ir.Queries))
	for i := range ir.Queries {
		q := &ir.Queries[i]
		out[q.ID()] = q
	}
	return out
}

func indexProcedures(ir *dsl.IR) map[string]*dsl.CustomProcedure {
	if ir == nil {
		return nil
	}
	out := make(map[string]*dsl.CustomProcedure, len(ir.Procedures))
	for i := range ir.Procedures {
		p := &ir.Procedures[i]
		out[p.ID()] = p
	}
	return out
}

// customQueryContentEqual / customProcContentEqual compare semantic content,
// ignoring SourcePath (a file move is not a content change) and Pos (already
// json:"-"). Canonical JSON is a stable fingerprint because every field
// marshals in declaration order.
func customQueryContentEqual(a, b *dsl.CustomQuery) bool {
	ca, cb := *a, *b
	ca.SourcePath, cb.SourcePath = "", ""
	ja, _ := json.Marshal(ca)
	jb, _ := json.Marshal(cb)
	return bytes.Equal(ja, jb)
}

func customProcContentEqual(a, b *dsl.CustomProcedure) bool {
	ca, cb := *a, *b
	ca.SourcePath, cb.SourcePath = "", ""
	ja, _ := json.Marshal(ca)
	jb, _ := json.Marshal(cb)
	return bytes.Equal(ja, jb)
}

// ---- per-entity diffs ----

func diffEntity(oldE, newE *dsl.Entity, d *Diff, ctx *diffCtx) {
	diffTableName(oldE, newE, d)
	diffFields(oldE, newE, d, ctx)
	diffIndexes(oldE, newE, d)
	diffUniques(oldE, newE, d)
	diffChecks(oldE, newE, d)
	diffChunkTimeInterval(oldE, newE, d)
	diffCache(oldE, newE, d)
	diffQueryTimeout(oldE, newE, d)
}

// diffChecks diffs CHECK constraints — both the entity-level `check "..."`
// declarations and the per-field `check` modifier.
//
// Adding a CHECK is backfill-required, not additive, and the distinction is the
// point: Postgres validates the predicate against every existing row when the
// constraint is added, so a table with one violating row takes an
// ACCESS EXCLUSIVE lock, scans, and fails. That is a data-dependent outcome the
// plan must surface before apply, which is exactly what a rehearsal probe would
// check — and could not, while this differ did not exist and the emitted DDL
// therefore contained no CHECK at all.
//
// Changing a predicate is modelled as one change rather than remove+add,
// because the emitted DDL must drop and re-add in that order within the same
// statement group; splitting it risks a re-ordering that leaves the table
// briefly unconstrained.
//
// A note for whoever writes the rehearsal probe for this kind: Postgres accepts
// a row when a CHECK evaluates TRUE *or* NULL, so `CHECK (total > 0)` does not
// reject rows where total IS NULL. The Detail below says the constraint is
// validated against every existing row, which is true — but the set of rows it
// can reject is narrower than the predicate reads. A probe counting
// `WHERE NOT (expr)` matches that semantics exactly, because NOT NULL is NULL
// and the row is excluded; a probe counting `WHERE expr IS NOT TRUE` would
// over-report.
//
// Entity-level checks are keyed by resolved constraint name, matching how
// EmitInitial names them, so an unnamed check that shifts position is not
// mistaken for a different constraint.
func diffChecks(oldE, newE *dsl.Entity, d *Diff) {
	oldChecks := resolvedChecks(oldE)
	newChecks := resolvedChecks(newE)

	// A field-level check on a column that is itself being added or dropped
	// travels with the column: columnDecl inlines CHECK (...) into ADD COLUMN,
	// and DROP COLUMN takes the constraint with it. Emitting a separate
	// ADD CONSTRAINT for it is not merely redundant — Postgres auto-names the
	// inline check <table>_<column>_check, which is exactly the name resolved
	// here, so the second statement fails with
	//
	//	ERROR:  constraint "shop_order_total_check" for relation "shop_order" already exists
	//
	// and takes the whole migration down. `total int check "total > 0"` on a
	// new column is the most ordinary thing in this grammar, so this guard
	// covers common usage rather than an edge case.
	//
	// diffUniques makes the same argument one level up, for a brand-new
	// entity whose constraints come from EmitInitial.
	oldFields := fieldsByName(oldE)
	newFields := fieldsByName(newE)
	travelsWithColumn := func(c checkRef) bool {
		if c.field == "" {
			return false
		}
		_, inOld := oldFields[c.field]
		_, inNew := newFields[c.field]
		return inOld != inNew
	}

	// Iterated in sorted key order, not map order. Emitted SQL is a persisted
	// artifact — migration files land in git, PreviewRollback renders it, and
	// tide hashes it — so the same inputs must produce the same bytes. Ranging
	// the maps directly produced four distinct up-scripts across sixty runs of
	// one four-check diff.
	for _, key := range sortedCheckKeys(newChecks) {
		now := newChecks[key]
		if travelsWithColumn(now) {
			continue
		}
		prior, existed := oldChecks[key]
		switch {
		case !existed:
			d.append(Change{
				Kind:       KindCheckAdded,
				Class:      ClassBackfillRequired,
				EntityID:   newE.ID(),
				Field:      now.field,
				Constraint: now.name,
				Detail: "CHECK added — Postgres validates it against every existing row " +
					"under an exclusive lock: " + now.expr,
				To: now.expr,
			})
		case prior.expr != now.expr:
			d.append(Change{
				Kind:       KindCheckChanged,
				Class:      ClassBackfillRequired,
				EntityID:   newE.ID(),
				Field:      now.field,
				Constraint: now.name,
				Detail:     "CHECK predicate changed — revalidated against every existing row",
				From:       prior.expr,
				To:         now.expr,
			})
		}
	}

	for _, key := range sortedCheckKeys(oldChecks) {
		prior := oldChecks[key]
		if _, still := newChecks[key]; still {
			continue
		}
		if travelsWithColumn(prior) {
			continue
		}
		// Dropping a constraint cannot fail on data and takes only a brief
		// lock, so it is additive in the plan-class sense — the rows it used to
		// reject simply become legal. Routed through append rather than
		// assigned to d.Additive directly so the unknown-class backstop there
		// applies to this kind too.
		d.append(Change{
			Kind:       KindCheckRemoved,
			Class:      ClassAdditive,
			EntityID:   oldE.ID(),
			Field:      prior.field,
			Constraint: prior.name,
			Detail:     "CHECK removed: " + prior.expr,
			From:       prior.expr,
		})
	}
}

// diffChunkTimeInterval reports a change to a hypertable's chunk sizing.
//
// Additive: set_chunk_time_interval affects only chunks created afterwards,
// leaving existing chunks and their data untouched, so it can neither fail on
// data nor take a disruptive lock.
func diffChunkTimeInterval(oldE, newE *dsl.Entity, d *Diff) {
	if oldE.ChunkTimeIntervalMS == newE.ChunkTimeIntervalMS {
		return
	}
	d.append(Change{
		Kind:     KindChunkTimeIntervalChanged,
		Class:    ClassAdditive,
		EntityID: newE.ID(),
		Detail: fmt.Sprintf("hypertable chunk_time_interval changed; applies to chunks " +
			"created from now on, existing chunks keep their sizing"),
		From: oldE.ChunkTimeIntervalMS,
		To:   newE.ChunkTimeIntervalMS,
	})
}

// checkRef is one resolved CHECK: its predicate, and the column it came from
// when it was written as a per-field `check` modifier.
type checkRef struct {
	// name is the constraint's identifier in the database — the same name
	// EmitInitial writes when it creates the table fresh.
	name  string
	expr  string
	field string // empty for entity-level checks
}

// resolvedChecks returns an entity's CHECK constraints keyed by the name they
// carry in the database.
//
// Identity is the constraint name, not the predicate, because the name is what
// a later ALTER has to target. Keying by predicate conflated constraints that
// Postgres considers distinct: two checks sharing a predicate collapsed to one
// map entry, so deleting one produced no change at all, and — worse — the
// emitter's predicate-scan resolved the survivor's name, emitting a DROP for
// the constraint the author had kept while the one they removed stayed live.
//
// The three name forms mirror what actually ends up in the catalog:
//
//   - field-level `check`, which codegen inlines into the column definition.
//     Postgres auto-names a single-column inline check <table>_<column>_check,
//     so that is the name here.
//   - entity-level `check "..." as name` — the author's name, verbatim.
//   - entity-level `check "..."` — derived from the predicate by
//     unnamedCheckName, matching EmitInitial.
//
// COMPATIBILITY: unnamed entity-level checks were previously named positionally
// (<table>_check_1, _2, …). A database created by an earlier build therefore
// holds the positional name, and a later removal emits DROP CONSTRAINT IF
// EXISTS against the predicate-derived one, which misses silently.
//
// Taken deliberately. Positional naming has a permanent defect — reordering two
// unnamed checks renames both, so the differ must either report a spurious
// DROP + ADD (an ACCESS EXCLUSIVE revalidation for an unchanged schema) or stay
// silent and let a migrated database diverge from a freshly created one.
// Predicate-derived naming has no such defect; its only cost is this one-time
// break.
//
// The 19 .atl files in this repository declare four entity-level checks, and
// all four are named (`check "..." as <name>`), which this scheme does not
// touch — an author-supplied name is used verbatim. Zero are unnamed, so the
// affected population here is empty. .atl files live in caller repositories too
// (see the schema-in-caller-repos convention), so this belongs in release notes
// rather than only in a comment.
func resolvedChecks(e *dsl.Entity) map[string]checkRef {
	refs := resolveCheckNames(e)
	out := make(map[string]checkRef, len(refs))
	for _, r := range refs {
		out[r.name] = r
	}
	return out
}

// resolveCheckNames assigns every CHECK on an entity the name it carries in the
// database, in a fixed order: entity-level checks in declaration order, then
// field-level checks in field order.
//
// This is the single source of those names. EmitInitial writes them when it
// creates the table and resolvedChecks derives constraint identity from them;
// if the two disagreed, a migrated database and a freshly created one would end
// up with differently-named constraints and every later ALTER would target the
// wrong one.
//
// Author-supplied names are reserved first, then generated names are assigned
// around them. The ordering matters: a user who copies a constraint name out of
// a live database can land on exactly the form unnamedCheckName produces, and
// whoever loses that collision disappears — two checks map to one name, one
// vanishes from the diff in both directions, and no DDL is emitted for it. The
// author asked for their name, so the generated one yields.
//
// Two identical author-supplied names DO collapse here — the result is a map
// keyed by name, and nothing can distinguish them. That is tolerable only
// because such a schema never reaches a database: checkNameCollisions rejects
// it in both EmitSQL and EmitInitial, so the collapsed diff is never emitted.
// The ordering matters and is easy to get wrong — if the guard were ever
// removed or bypassed, this map would silently lose a constraint.
func resolveCheckNames(e *dsl.Entity) []checkRef {
	taken := map[string]bool{}
	for _, c := range e.Checks {
		if c.Name != "" {
			taken[c.Name] = true
		}
	}

	// free returns the first unused variant of a generated name. Bumping the
	// occurrence rather than giving up is what makes collapse structurally
	// impossible: every check gets a distinct key no matter what names the
	// author chose.
	free := func(gen func(int) string) string {
		for occ := 1; ; occ++ {
			if n := gen(occ); !taken[n] {
				taken[n] = true
				return n
			}
		}
	}

	out := make([]checkRef, 0, len(e.Checks)+len(e.Fields))
	for _, c := range e.Checks {
		if c.Name != "" {
			out = append(out, checkRef{name: c.Name, expr: c.Expr})
			continue
		}
		expr := c.Expr
		out = append(out, checkRef{
			name: free(func(occ int) string { return unnamedCheckName(e, expr, occ) }),
			expr: expr,
		})
	}
	for i := range e.Fields {
		f := &e.Fields[i]
		if f.Check == "" {
			continue
		}
		// Postgres auto-names a single-column inline CHECK
		// <table>_<column>_check, and codegen inlines field checks into the
		// column definition, so that is the name in the catalog.
		field := f.Name
		out = append(out, checkRef{
			name: free(func(occ int) string {
				if occ == 1 {
					return truncateIdent(fmt.Sprintf("%s_%s_check", tableName(e), field))
				}
				return truncateIdent(fmt.Sprintf("%s_%s_check%d", tableName(e), field, occ))
			}),
			expr:  f.Check,
			field: field,
		})
	}
	return out
}

// sortedCheckKeys returns constraint names in a stable order, so an emitter
// walking the result produces the same bytes on every run. Emitted SQL is a
// persisted artifact — migration files land in git, PreviewRollback renders it,
// and tide hashes it — so ranging the map directly is not acceptable here.
func sortedCheckKeys(m map[string]checkRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkNameCollisions reports author-supplied constraint names used more than
// once on an entity. Postgres rejects the resulting CREATE TABLE, so emitting
// it would produce a migration that cannot apply; failing here names the actual
// problem instead.
func checkNameCollisions(e *dsl.Entity) []string {
	seen := map[string]int{}
	for _, c := range e.Checks {
		if c.Name != "" {
			seen[c.Name]++
		}
	}
	var dup []string
	for name, n := range seen {
		if n > 1 {
			dup = append(dup, name)
		}
	}
	sort.Strings(dup)
	return dup
}

// diffUniques diffs entity-level composite UNIQUE specs by order-independent
// column set. Runs only inside diffEntity — which fires only when an entity
// exists on BOTH sides — so a brand-new entity's composite uniques come from
// the table-create path (EmitInitial), never a spurious ADD CONSTRAINT here.
func diffUniques(oldE, newE *dsl.Entity, d *Diff) {
	oldKeys := uniqueSpecKeys(oldE.Uniques)
	newKeys := uniqueSpecKeys(newE.Uniques)
	for k, u := range newKeys {
		if _, ok := oldKeys[k]; !ok {
			d.append(Change{
				Kind:     KindCompositeUniqueAdded,
				Class:    ClassBackfillRequired,
				EntityID: newE.ID(),
				Detail:   "composite UNIQUE added — verify no duplicates exist: (" + strings.Join(u.Fields, ", ") + ")",
				To:       u,
			})
		}
	}
	for k, u := range oldKeys {
		if _, ok := newKeys[k]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind:     KindCompositeUniqueRemoved,
				Class:    ClassAdditive,
				EntityID: newE.ID(),
				Detail:   "composite UNIQUE removed: (" + strings.Join(u.Fields, ", ") + ")",
				From:     u,
			})
		}
	}
}

// uniqueSpecKeys maps each composite unique to an order-independent key so
// reordering `unique by a, b` → `unique by b, a` is a no-op.
func uniqueSpecKeys(specs []dsl.UniqueSpec) map[string]dsl.UniqueSpec {
	out := make(map[string]dsl.UniqueSpec, len(specs))
	for _, u := range specs {
		cols := slices.Clone(u.Fields)
		sort.Strings(cols)
		out[strings.Join(cols, "\x00")] = u
	}
	return out
}

// diffTableName flags moves of the `table "..."` modifier as cross-caller
// breaking. atlantis won't generate a rename automatically; the operator
// runs ALTER TABLE RENAME themselves before re-applying. Either direction
// counts: appearing, disappearing, or swapping schemas.
func diffTableName(oldE, newE *dsl.Entity, d *Diff) {
	if oldE.TableName == newE.TableName {
		return
	}
	d.Breaking = append(d.Breaking, Change{
		Kind:     KindEntityTableChanged,
		Class:    ClassCrossCallerBreaking,
		EntityID: newE.ID(),
		Detail:   fmt.Sprintf("table override changed: %q -> %q (manual ALTER TABLE RENAME required)", oldE.TableName, newE.TableName),
		From:     oldE.TableName,
		To:       newE.TableName,
	})
}

func diffFields(oldE, newE *dsl.Entity, d *Diff, ctx *diffCtx) {
	oldFields := fieldsByName(oldE)
	newFields := fieldsByName(newE)

	names := mergedNames(oldFields, newFields)
	for _, name := range names {
		of, hasOld := oldFields[name]
		nf, hasNew := newFields[name]
		switch {
		case hasOld && !hasNew:
			// Field removed. Check whether the owning caller is the
			// submitter and no other caller references this field.
			entityID := newE.ID()
			refKey := entityID + "." + name
			class := ctx.classifyRemoval(entityID, refKey)
			d.append(Change{
				Kind:     KindFieldRemoved,
				Class:    class,
				EntityID: entityID,
				Field:    name,
				Detail:   "field removed",
				From:     of,
			})
		case !hasOld && hasNew:
			// New field. Backfill-required iff NOT NULL with no DEFAULT and
			// the entity already existed (i.e., there may be existing rows).
			class := ClassAdditive
			detail := "field added"
			if nf.NotNull && nf.Default == nil {
				class = ClassBackfillRequired
				detail = "field added NOT NULL with no DEFAULT — requires backfill"
			}
			ch := Change{
				Kind:     KindFieldAdded,
				Class:    class,
				EntityID: newE.ID(),
				Field:    name,
				Detail:   detail,
				To:       nf,
			}
			d.append(ch)
			// Detect FK-on-populated-table separately. Adding a reference
			// after the column existed isn't covered here (the column itself
			// is new) — but we still emit the additive-reference change so
			// the SQL emitter can generate the FK constraint.
			if nf.Ref != nil {
				d.Additive = append(d.Additive, Change{
					Kind:     KindFieldReferenceAdded,
					Class:    ClassAdditive,
					EntityID: newE.ID(),
					Field:    name,
					Detail:   "reference added on newly-introduced field",
					To:       nf.Ref,
				})
			}
		default:
			diffField(newE.ID(), of, nf, d)
		}
	}
}

func diffField(entityID string, oldF, newF *dsl.Field, d *Diff) {
	// Type change. We classify type narrowing as backfill-required, widening
	// as additive. We treat any non-equal type as breaking unless we can
	// recognize it as a safe widening.
	if !typeEqual(oldF.Type, newF.Type) {
		class := classifyTypeChange(oldF.Type, newF.Type)
		d.append(Change{
			Kind:     KindFieldTypeChanged,
			Class:    class,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   fmt.Sprintf("type %s → %s", typeString(oldF.Type), typeString(newF.Type)),
			From:     oldF.Type,
			To:       newF.Type,
		})
	}

	// NOT NULL changes.
	switch {
	case !oldF.NotNull && newF.NotNull:
		// Tightening NOT NULL on an existing column requires backfill iff
		// there's no DEFAULT in the new schema (DEFAULT covers existing rows).
		class := ClassBackfillRequired
		detail := "NOT NULL tightened — requires backfill (or a DEFAULT)"
		if newF.Default != nil {
			class = ClassAdditive
			detail = "NOT NULL tightened with DEFAULT — safe"
		}
		d.append(Change{
			Kind:     KindFieldNotNullTightened,
			Class:    class,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   detail,
		})
	case oldF.NotNull && !newF.NotNull:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldNotNullLoosened,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "NOT NULL loosened",
		})
	}

	// UNIQUE changes.
	switch {
	case !oldF.Unique && newF.Unique:
		// Adding UNIQUE to an existing column may fail if data has duplicates.
		// We classify as backfill-required so a human verifies / dedupes.
		d.append(Change{
			Kind:     KindFieldUniqueAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "UNIQUE added — verify no duplicates exist",
		})
	case oldF.Unique && !newF.Unique:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldUniqueRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "UNIQUE removed",
		})
	}

	// DEFAULT changes.
	if !defaultsEqual(oldF.Default, newF.Default) {
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldDefaultChanged,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "DEFAULT changed",
			From:     oldF.Default,
			To:       newF.Default,
		})
	}

	// Backfill expression changes. The modifier is metadata for
	// `tide apply --backfill`; the schema itself doesn't change so every
	// transition is additive. A paired NOT-NULL tightening on the same
	// field still gets classified BackfillRequired by the existing
	// branches above — that's what gates plain `tide apply`.
	switch {
	case oldF.Backfill == "" && newF.Backfill != "":
		d.append(Change{
			Kind:     KindFieldBackfillAdded,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   fmt.Sprintf("backfill expression added: %s", newF.Backfill),
			To:       newF.Backfill,
		})
	case oldF.Backfill != "" && newF.Backfill == "":
		d.append(Change{
			Kind:     KindFieldBackfillRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "backfill expression removed",
			From:     oldF.Backfill,
		})
	case oldF.Backfill != "" && newF.Backfill != "" && oldF.Backfill != newF.Backfill:
		d.append(Change{
			Kind:     KindFieldBackfillChanged,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "backfill expression changed",
			From:     oldF.Backfill,
			To:       newF.Backfill,
		})
	}

	// SERIAL (BIGSERIAL) changes. Both directions are backfill-required
	// because either side needs out-of-band coordination: adding serial
	// needs the sequence seeded to MAX(col)+1 so the next auto-generated
	// id doesn't collide with existing rows; removing serial means every
	// caller that relied on auto-increment must now supply the column
	// explicitly on INSERT, which is a behaviour change verified by
	// hand rather than auto-detected by the diff engine.
	switch {
	case !oldF.Serial && newF.Serial:
		d.append(Change{
			Kind:     KindFieldSerialAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "SERIAL added — seed sequence to MAX(col)+1 before apply",
		})
	case oldF.Serial && !newF.Serial:
		d.append(Change{
			Kind:     KindFieldSerialRemoved,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "SERIAL removed — callers must now supply the column explicitly",
		})
	}

	// Reference changes.
	switch {
	case oldF.Ref == nil && newF.Ref != nil:
		// Adding an FK to an existing column may fail if existing rows violate
		// it — backfill-required.
		d.append(Change{
			Kind:     KindFieldReferenceAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "FK added to existing column — verify referential integrity",
			To:       newF.Ref,
		})
	case oldF.Ref != nil && newF.Ref == nil:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldReferenceRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "FK removed",
			From:     oldF.Ref,
		})
	case oldF.Ref != nil && newF.Ref != nil && !refsEqual(oldF.Ref, newF.Ref):
		// Distinguish three sub-cases:
		//   - Target entity/field changed       → breaking (callers may
		//     have integrity assumptions about the *target* row)
		//   - Action weakened (cascade ← restrict, set null ← restrict, …)
		//     → additive: existing operations that succeeded still succeed
		//   - Action strengthened (restrict ← cascade) → backfill: existing
		//     dependent rows may now block writes that previously cascaded
		switch {
		case oldF.Ref.TargetID != newF.Ref.TargetID || oldF.Ref.TargetField != newF.Ref.TargetField:
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassCrossCallerBreaking,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK target changed",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		case refActionStrengthened(oldF.Ref.OnDelete, newF.Ref.OnDelete) ||
			refActionStrengthened(oldF.Ref.OnUpdate, newF.Ref.OnUpdate):
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassBackfillRequired,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK action strengthened — verify dependent rows",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		default:
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassAdditive,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK action weakened",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		}
	}
}

func diffIndexes(oldE, newE *dsl.Entity, d *Diff) {
	oldKeys := indexKeys(oldE.Indexes)
	newKeys := indexKeys(newE.Indexes)
	for k, idx := range newKeys {
		if _, ok := oldKeys[k]; !ok {
			// Index changes are always additive and never phase-split — a
			// CREATE INDEX (even UNIQUE) either succeeds or fails cleanly; it
			// has no chunked-backfill action. A unique index can fail on
			// existing duplicates, so we warn in the detail rather than
			// routing it through the backfill-required banner.
			detail := "index added: " + k
			if idx.Unique {
				detail = "unique index added: " + k + " — CREATE UNIQUE INDEX fails if duplicates exist in the predicate subset"
			}
			d.Additive = append(d.Additive, Change{
				Kind:     KindIndexAdded,
				Class:    ClassAdditive,
				EntityID: newE.ID(),
				Detail:   detail,
				To:       idx,
			})
		}
	}
	for k, idx := range oldKeys {
		if _, ok := newKeys[k]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind:     KindIndexRemoved,
				Class:    ClassAdditive,
				EntityID: newE.ID(),
				Detail:   "index removed: " + k,
				From:     idx,
			})
		}
	}
}

func diffCache(oldE, newE *dsl.Entity, d *Diff) {
	if cacheEqual(oldE.Cache, newE.Cache) {
		return
	}
	// Cache changes are always additive — they affect server behavior, not
	// the wire contract or stored data.
	d.Additive = append(d.Additive, Change{
		Kind:     KindCacheChanged,
		Class:    ClassAdditive,
		EntityID: newE.ID(),
		Detail:   "cache config changed",
		From:     oldE.Cache,
		To:       newE.Cache,
	})
}

func diffQueryTimeout(oldE, newE *dsl.Entity, d *Diff) {
	if oldE.QueryTimeoutMS == newE.QueryTimeoutMS {
		return
	}
	d.Additive = append(d.Additive, Change{
		Kind:     KindQueryTimeoutChanged,
		Class:    ClassAdditive,
		EntityID: newE.ID(),
		Detail:   fmt.Sprintf("query_timeout %dms → %dms", oldE.QueryTimeoutMS, newE.QueryTimeoutMS),
		From:     oldE.QueryTimeoutMS,
		To:       newE.QueryTimeoutMS,
	})
}

// ---- helpers ----

// append files a Change under its class.
//
// The default is the whole point of the function. Without it, a Change whose
// class this switch does not handle is silently discarded: it appears in no
// bucket, so the plan reports no such change, no DDL is emitted for it, and
// rehearsal examines DDL that does not contain it. That is the same failure the
// coverage registry in diff_coverage.go exists to prevent, reached by a
// different route — there, a differ never produced the Change; here, the Change
// was produced and then dropped.
//
// Filing it as breaking rather than panicking is deliberate. An unknown class
// is a programming error, but this runs inside a server handler, and the
// correct response to "we do not know how dangerous this is" is to treat it as
// the most dangerous thing it could be. Breaking stops the apply and surfaces
// the change to a human, which is both loud and safe; a panic is loud and takes
// the request with it.
func (d *Diff) append(c Change) {
	switch c.Class {
	case ClassAdditive:
		d.Additive = append(d.Additive, c)
	case ClassBackfillRequired:
		d.BackfillRequired = append(d.BackfillRequired, c)
	case ClassCrossCallerBreaking:
		d.Breaking = append(d.Breaking, c)
	default:
		d.Breaking = append(d.Breaking, c)
	}
}

func indexByID(ir *dsl.IR) map[string]*dsl.Entity {
	out := map[string]*dsl.Entity{}
	if ir == nil {
		return out
	}
	for i := range ir.Entities {
		out[ir.Entities[i].ID()] = &ir.Entities[i]
	}
	return out
}

func fieldsByName(e *dsl.Entity) map[string]*dsl.Field {
	out := map[string]*dsl.Field{}
	for i := range e.Fields {
		out[e.Fields[i].Name] = &e.Fields[i]
	}
	return out
}

func mergedNames[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	sort.Strings(out)
	return out
}

// typeEqual compares two field types structurally.
func typeEqual(a, b dsl.FieldType) bool {
	if a.Name != b.Name || a.Array != b.Array || a.VecDim != b.VecDim ||
		a.Len != b.Len ||
		a.NumP != b.NumP || a.NumS != b.NumS || a.HasNumP != b.HasNumP {
		return false
	}
	if a.Array {
		if a.Elem == nil || b.Elem == nil {
			return a.Elem == b.Elem
		}
		return typeEqual(*a.Elem, *b.Elem)
	}
	return true
}

// typeString renders a type for human-readable diff details.
func typeString(t dsl.FieldType) string {
	if t.Array {
		if t.Elem != nil {
			return "[]" + typeString(*t.Elem)
		}
		return "[]" + t.Name
	}
	switch t.Name {
	case "vector":
		return fmt.Sprintf("vector(%d)", t.VecDim)
	case "numeric":
		if t.HasNumP {
			return fmt.Sprintf("numeric(%d,%d)", t.NumP, t.NumS)
		}
		return "numeric"
	}
	return t.Name
}

// classifyTypeChange decides whether moving from oldT to newT is safe (additive),
// requires backfill, or is breaking.
//
// Conservative rules:
//   - Same name with widening (smallint→int→bigint, int→bigint): additive
//   - Same name with narrowing (bigint→int): backfill-required
//   - Numeric precision increase: additive; decrease: backfill-required
//   - Vector dimension change: breaking (data must be re-embedded)
//   - Array vs scalar: breaking
//   - Anything else: backfill-required (force human review)
func classifyTypeChange(oldT, newT dsl.FieldType) ChangeClass {
	if oldT.Array != newT.Array {
		return ClassCrossCallerBreaking
	}
	if oldT.Name == "vector" && newT.Name == "vector" {
		if oldT.VecDim != newT.VecDim {
			return ClassCrossCallerBreaking
		}
		return ClassAdditive
	}
	if oldT.Name == newT.Name {
		// numeric precision/scale
		if oldT.Name == "numeric" {
			if newT.NumP < oldT.NumP || newT.NumS < oldT.NumS {
				return ClassBackfillRequired
			}
			return ClassAdditive
		}
		// varchar(N) length. Postgres widens a varchar in place (no table
		// rewrite, no scan), so widening is additive; narrowing can
		// truncate existing rows, so it needs validation. Len 0 is the
		// "no declared limit" (unbounded) sentinel — the widest — so a
		// change to unbounded never truncates and unbounded → bounded can.
		if oldT.Name == "varchar" {
			narrows := newT.Len != 0 && (oldT.Len == 0 || newT.Len < oldT.Len)
			if narrows {
				return ClassBackfillRequired
			}
			return ClassAdditive
		}
		return ClassAdditive
	}
	if isWidening(oldT.Name, newT.Name) {
		return ClassAdditive
	}
	if isNarrowing(oldT.Name, newT.Name) {
		return ClassBackfillRequired
	}
	// Different families (text → int, jsonb → text, etc.) — force review.
	return ClassBackfillRequired
}

var intRank = map[string]int{
	"smallint": 1,
	"int":      2,
	"bigint":   3,
}

func isWidening(oldName, newName string) bool {
	o, ok1 := intRank[oldName]
	n, ok2 := intRank[newName]
	return ok1 && ok2 && n > o
}

func isNarrowing(oldName, newName string) bool {
	o, ok1 := intRank[oldName]
	n, ok2 := intRank[newName]
	return ok1 && ok2 && n < o
}

func defaultsEqual(a, b *dsl.Default) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// refActionStrengthened returns true iff going from `old` to `new` would
// reject operations that previously succeeded. Order from "permissive" to
// "strict":
//
//	cascade < set null < restrict
//
// Adding a previously-unset action (RefActionUnset → anything) counts as a
// strengthening because the prior behavior was Postgres's default (NO
// ACTION, which equates to RESTRICT at commit). Moving in the other
// direction is a weakening.
func refActionStrengthened(old, newAct dsl.RefAction) bool {
	rank := func(a dsl.RefAction) int {
		switch a {
		case dsl.RefActionCascade:
			return 1
		case dsl.RefActionSetNull:
			return 2
		case dsl.RefActionRestrict, dsl.RefActionUnset:
			return 3 // unset == NO ACTION, behaviorally RESTRICT-ish
		}
		return 3
	}
	return rank(newAct) > rank(old)
}

func refsEqual(a, b *dsl.Ref) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// indexFieldKey returns a stable token representing one IndexField (either
// a column name or an expression). Used inside indexKey for diff stability.
func indexFieldKey(f dsl.IndexField) string {
	if f.IsExpr {
		return "expr:" + f.Expr
	}
	return f.Name
}

// indexKeys returns a stable identifier for each index in the slice. Indexes
// are compared by structural shape; reordering them in the .atl file produces
// no diff.
func indexKeys(idxs []dsl.Index) map[string]dsl.Index {
	out := map[string]dsl.Index{}
	for _, idx := range idxs {
		out[indexKey(idx)] = idx
	}
	return out
}

func indexKey(idx dsl.Index) string {
	switch idx.Kind {
	case dsl.IndexBtree:
		s := "btree:"
		for i, f := range idx.Fields {
			if i > 0 {
				s += ","
			}
			s += indexFieldKey(f)
			if f.Desc {
				s += " desc"
			}
		}
		return s
	case dsl.IndexPartial:
		// Unique partials key distinctly from non-unique ones so toggling
		// `unique` is detected as a drop+recreate (different index identity).
		s := "partial:"
		if idx.Unique {
			s = "unique_partial:"
		}
		for i, f := range idx.Fields {
			if i > 0 {
				s += ","
			}
			s += indexFieldKey(f)
			if f.Desc {
				s += " desc"
			}
		}
		// The predicate portion (prefixed `|`) is byte-identical to the
		// pre-tree encoding for the two legacy shapes, so already-applied
		// partial indexes never re-diff as drop+recreate; compound predicates
		// get a deterministic, commutativity-stable structural key.
		s += predsql.CanonicalKey(idx.Where)
		return s
	case dsl.IndexHNSW:
		return fmt.Sprintf("hnsw:%s:%s", idx.Field, idx.VecOps)
	case dsl.IndexGIN:
		return "gin:" + idx.Field
	}
	return "unknown"
}

func cacheEqual(a, b *dsl.Cache) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.HasReadThrough != b.HasReadThrough || a.TTLMS != b.TTLMS ||
		a.Tag != b.Tag || a.Consistency != b.Consistency {
		return false
	}
	if !sliceEq(a.TagFields, b.TagFields) {
		return false
	}
	if len(a.Invalidate) != len(b.Invalidate) {
		return false
	}
	for i := range a.Invalidate {
		ai, bi := a.Invalidate[i], b.Invalidate[i]
		if ai.Self != bi.Self || ai.TargetID != bi.TargetID {
			return false
		}
		if (ai.Where == nil) != (bi.Where == nil) {
			return false
		}
		if ai.Where != nil && *ai.Where != *bi.Where {
			return false
		}
	}
	return true
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
