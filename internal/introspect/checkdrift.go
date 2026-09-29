package introspect

// checkdrift.go detects divergence between the CHECK constraints a caller
// declares (column-level `check "..."` and table-level `check "..." as
// name`) and the CHECK constraints actually enforced on the live table.
//
// The diff compares against the checkpoint, not the database, so a check that
// diverged outside atlantis — narrower at adoption, or edited by hand — stays
// whatever the database has, and the mismatch surfaces as SQLSTATE 23514 on a
// write the schema permits.
//
// Matching is by normalized expression, not by constraint name. A declared
// check is rendered onto a throwaway TEMP table carrying the entity's column
// types, and Postgres's own pg_get_constraintdef deparse is compared against
// the live one, so the engine that enforces the constraint is the one deciding
// equivalence. No Go-side canonicalization; the same argument as
// normalizePredicate. Read-only.
//
// Two checks that are semantically equal and textually different are reported
// as divergent — most often `col IS NULL OR col IN (...)` against a bare
// `col IN (...)`, which pass identically since a CHECK admits NULL. The apply
// refuses on drift; ATLANTIS_ALLOW_CHECK_DRIFT=1 turns the check off.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// CheckDriftKind distinguishes the two divergence directions.
type CheckDriftKind string

const (
	// CheckDeclaredNotEnforced: the .atl declares a CHECK whose normalized
	// form matches no live constraint — the declared contract isn't enforced
	// as written. The dangerous case: the .atl widened a value set but the
	// live constraint is still the narrower pre-adopt one, so writes of the
	// new value fail at runtime.
	CheckDeclaredNotEnforced CheckDriftKind = "declared_not_enforced"
	// CheckLiveNotDeclared: a live CHECK matches no declared one. The DB
	// enforces a constraint the schema doesn't describe — usually the live
	// half of a diverged pair, or an orphan from a pre-atlantis migration.
	CheckLiveNotDeclared CheckDriftKind = "live_not_declared"
)

// CheckConstraintDrift is one CHECK-constraint divergence between the
// declared schema and the live table. JSON-tagged so it round-trips through
// the admin PlanResponse to the CLI.
type CheckConstraintDrift struct {
	Kind     CheckDriftKind `json:"kind"`
	EntityID string         `json:"entity_id"`
	Schema   string         `json:"schema"`
	Table    string         `json:"table"`
	// ConstraintName is the live constraint name (CheckLiveNotDeclared);
	// empty for a declared-only check (the temp-table name is meaningless).
	ConstraintName string `json:"constraint_name,omitempty"`
	// Declared is the verbatim .atl check expression (CheckDeclaredNotEnforced).
	Declared string `json:"declared,omitempty"`
	// Definition is the normalized `CHECK (...)` deparse: the declared
	// check's Postgres-normalized form, or the live constraint's def.
	Definition string `json:"definition"`
}

// Describe renders an operator-facing one-liner.
func (d CheckConstraintDrift) Describe() string {
	switch d.Kind {
	case CheckDeclaredNotEnforced:
		return fmt.Sprintf("%s.%s: declared check %q is not enforced live (normalized: %s)", d.Schema, d.Table, d.Declared, d.Definition)
	case CheckLiveNotDeclared:
		return fmt.Sprintf("%s.%s: live constraint %q is not declared (%s)", d.Schema, d.Table, d.ConstraintName, d.Definition)
	}
	return fmt.Sprintf("%s.%s: %s", d.Schema, d.Table, d.Definition)
}

// DetectCheckConstraintDrift compares declared CHECK constraints against the
// live ones on each declared table and reports divergences in both
// directions, plus advisory notes (declared checks that couldn't be
// normalized). Read-only; safe at plan time or inside the apply tx.
func DetectCheckConstraintDrift(ctx context.Context, q DBTX, declaredIR *dsl.IR) ([]CheckConstraintDrift, []string, error) {
	if declaredIR == nil {
		return nil, nil, fmt.Errorf("introspect: declaredIR is required")
	}
	r := renderChecks(ctx, q, declaredIR)
	r.settle(ctx, q, false)
	drift, err := r.drift(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	return drift, r.notes, nil
}

// renderedChecks is a schema's declared CHECK constraints in Postgres's
// deparse form, per physical table. A table with no declared check is
// present, so its live orphans still surface.
type renderedChecks struct {
	tables  map[physRef]*renderedCheckTable
	pending []pendingCheck // checks that did not normalize
	notes   []string
}

type renderedCheckTable struct {
	entityID string
	defs     map[string]string // normalized def → verbatim expr
}

type pendingCheck struct {
	ref    physRef
	entity *dsl.Entity
	expr   string
}

// settle normalizes the pending checks again when retry is set, and records
// a note for each that still does not normalize.
func (r *renderedChecks) settle(ctx context.Context, q DBTX, retry bool) {
	for _, p := range r.pending {
		if retry {
			if norm, ok := normalizeCheck(ctx, q, p.entity, p.expr); ok {
				r.tables[p.ref].defs[norm] = p.expr
				continue
			}
		}
		r.notes = append(r.notes, fmt.Sprintf("%s: could not normalize declared check %q for comparison — audit it out-of-band", p.entity.ID(), p.expr))
	}
	r.pending = nil
}

// renderChecks normalizes every column-level and table-level CHECK
// declaredIR declares. It reads no user table.
func renderChecks(ctx context.Context, q DBTX, declaredIR *dsl.IR) *renderedChecks {
	r := &renderedChecks{tables: make(map[physRef]*renderedCheckTable, len(declaredIR.Entities))}
	for i := range declaredIR.Entities {
		e := &declaredIR.Entities[i]
		s, t := physical(e)
		ref := physRef{s, t}
		rt := r.tables[ref]
		if rt == nil {
			rt = &renderedCheckTable{entityID: e.ID(), defs: map[string]string{}}
			r.tables[ref] = rt
		}
		var exprs []string
		for j := range e.Fields {
			if c := strings.TrimSpace(e.Fields[j].Check); c != "" {
				exprs = append(exprs, c)
			}
		}
		for _, tc := range e.Checks {
			if c := strings.TrimSpace(tc.Expr); c != "" {
				exprs = append(exprs, c)
			}
		}
		for _, expr := range exprs {
			norm, ok := normalizeCheck(ctx, q, e, expr)
			if !ok {
				r.pending = append(r.pending, pendingCheck{ref, e, expr})
				continue
			}
			rt.defs[norm] = expr
		}
	}
	return r
}

// drift compares r with the live CHECK constraints q reads, in both
// directions.
func (r *renderedChecks) drift(ctx context.Context, q Querier) ([]CheckConstraintDrift, error) {
	if len(r.tables) == 0 {
		return nil, nil
	}
	pairs := make([]physRef, 0, len(r.tables))
	for ref := range r.tables {
		pairs = append(pairs, ref)
	}
	live, exists, err := loadLiveChecks(ctx, q, pairs)
	if err != nil {
		return nil, err
	}

	var drift []CheckConstraintDrift
	for ref, rt := range r.tables {
		if !exists[ref] {
			continue // table not in the live database
		}
		declaredDefs := rt.defs
		liveDefs := make(map[string]bool, len(live[ref]))
		for _, lc := range live[ref] {
			liveDefs[lc.def] = true
		}

		// Declared, but no equivalent live constraint.
		for norm, expr := range declaredDefs {
			if !liveDefs[norm] {
				drift = append(drift, CheckConstraintDrift{
					Kind:       CheckDeclaredNotEnforced,
					EntityID:   rt.entityID,
					Schema:     ref.schema,
					Table:      ref.table,
					Declared:   expr,
					Definition: norm,
				})
			}
		}
		// Live, but no equivalent declared check.
		for _, lc := range live[ref] {
			if _, ok := declaredDefs[lc.def]; !ok {
				drift = append(drift, CheckConstraintDrift{
					Kind:           CheckLiveNotDeclared,
					EntityID:       rt.entityID,
					Schema:         ref.schema,
					Table:          ref.table,
					ConstraintName: lc.name,
					Definition:     lc.def,
				})
			}
		}
	}

	sort.Slice(drift, func(i, j int) bool {
		a, b := drift[i], drift[j]
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ConstraintName != b.ConstraintName {
			return a.ConstraintName < b.ConstraintName
		}
		return a.Definition < b.Definition
	})
	return drift, nil
}

// normalizeCheck returns the pg_get_constraintdef deparse of a declared check
// as Postgres itself would store it — the same normalized form the live side
// is read in. It renders the check onto a throwaway TEMP table carrying the
// entity's real column types, reads the stored constraint def back, and rolls
// everything back. Mirror of normalizePredicate. ok=false means the check
// couldn't be normalized (illegal expression, unknown column, DB error); the
// caller treats that as "no match" and emits an advisory note.
func normalizeCheck(ctx context.Context, db DBTX, e *dsl.Entity, expr string) (string, bool) {
	if len(e.Fields) == 0 {
		return "", false
	}
	defs := make([]string, 0, len(e.Fields))
	for i := range e.Fields {
		f := &e.Fields[i]
		defs = append(defs, schema.QuoteIdent(f.Name)+" "+schema.SQLType(f.Type))
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "CREATE TEMP TABLE _atl_checknorm ("+strings.Join(defs, ", ")+")"); err != nil {
		return "", false
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE _atl_checknorm ADD CONSTRAINT _atl_chk CHECK ("+expr+")"); err != nil {
		return "", false
	}
	var def string
	if err := tx.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = '_atl_chk' AND conrelid = '_atl_checknorm'::regclass`).Scan(&def); err != nil {
		return "", false
	}
	return def, true
}

type liveCheck struct {
	name string
	def  string
}

// loadLiveChecks reads every CHECK constraint (contype='c') on the given
// physical tables, keyed by table, with Postgres's canonical
// pg_get_constraintdef deparse for each.
func loadLiveChecks(ctx context.Context, q Querier, pairs []physRef) (checks map[physRef][]liveCheck, exists map[physRef]bool, err error) {
	schemas, tables := splitPairs(pairs)
	// One row per existing table with no CHECK, the constraint columns NULL.
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT n.nspname, c.relname, con.conname, pg_get_constraintdef(con.oid)
FROM targets tg
JOIN pg_namespace n ON n.nspname = tg.schema
JOIN pg_class c     ON c.relnamespace = n.oid AND c.relname = tg.table_name
LEFT JOIN pg_constraint con ON con.conrelid = c.oid AND con.contype = 'c'`, schemas, tables)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	checks = make(map[physRef][]liveCheck)
	exists = make(map[physRef]bool)
	for rows.Next() {
		var s, t string
		var name, def *string
		if err := rows.Scan(&s, &t, &name, &def); err != nil {
			return nil, nil, err
		}
		key := physRef{s, t}
		exists[key] = true
		if name != nil {
			checks[key] = append(checks[key], liveCheck{name: *name, def: *def})
		}
	}
	return checks, exists, rows.Err()
}
