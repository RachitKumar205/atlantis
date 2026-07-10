package introspect

// checkdrift.go detects divergence between the CHECK constraints a caller
// declares (column-level `check "..."` and table-level `check "..." as
// name`) and the CHECK constraints actually enforced on the live table.
//
// Like the unique-index detector, the plan/diff path is structurally blind
// to CHECK drift: the differ never manages CHECK constraints, so a check
// that was narrower at adoption (a pre-atlantis migration) — or that the
// .atl later widened — silently stays whatever the DB had, and the mismatch
// only surfaces as a runtime 23514 check violation (the carts
// `awaiting_checkout` outage was exactly this).
//
// Matching is by NORMALIZED EXPRESSION, not by constraint name: a declared
// check is rendered onto a throwaway TEMP table carrying the entity's column
// types, and Postgres's own pg_get_constraintdef deparse is compared to the
// live pg_get_constraintdef. Equivalence is decided by the same engine that
// enforces the constraint — no Go-side expression canonicalization, the same
// soundness argument as normalizePredicate. Read-only; reports, never alters.
//
// A known, accepted limitation: two checks that are *semantically* equal but
// *textually* different — most commonly `col IS NULL OR col IN (...)` vs the
// bare `col IN (...)` (a CHECK passes on NULL either way) — are reported as a
// divergence. That's why this surfaces as an advisory, not a hard refusal:
// the operator reads the declared-vs-live defs and decides.

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

	// Declared checks per physical table — column-level (Field.Check) and
	// table-level (Entity.Checks). We record every entity (even with no
	// declared checks) so live orphans on those tables still surface.
	type declTable struct {
		entity *dsl.Entity
		exprs  []string
	}
	decl := make(map[physRef]*declTable, len(declaredIR.Entities))
	for i := range declaredIR.Entities {
		e := &declaredIR.Entities[i]
		s, t := physical(e)
		ref := physRef{s, t}
		dt := decl[ref]
		if dt == nil {
			dt = &declTable{entity: e}
			decl[ref] = dt
		}
		for j := range e.Fields {
			if c := strings.TrimSpace(e.Fields[j].Check); c != "" {
				dt.exprs = append(dt.exprs, c)
			}
		}
		for _, tc := range e.Checks {
			if c := strings.TrimSpace(tc.Expr); c != "" {
				dt.exprs = append(dt.exprs, c)
			}
		}
	}
	if len(decl) == 0 {
		return nil, nil, nil
	}

	pairs := make([]physRef, 0, len(decl))
	for r := range decl {
		pairs = append(pairs, r)
	}
	live, err := loadLiveChecks(ctx, q, pairs)
	if err != nil {
		return nil, nil, err
	}

	var drift []CheckConstraintDrift
	var notes []string
	for ref, dt := range decl {
		// Normalize every declared check to Postgres's deparse form.
		declaredDefs := make(map[string]string, len(dt.exprs)) // normDef → verbatim expr
		for _, expr := range dt.exprs {
			norm, ok := normalizeCheck(ctx, q, dt.entity, expr)
			if !ok {
				notes = append(notes, fmt.Sprintf("%s: could not normalize declared check %q for comparison — audit it out-of-band", dt.entity.ID(), expr))
				continue
			}
			declaredDefs[norm] = expr
		}
		liveDefs := make(map[string]bool, len(live[ref]))
		for _, lc := range live[ref] {
			liveDefs[lc.def] = true
		}

		// Declared, but no equivalent live constraint.
		for norm, expr := range declaredDefs {
			if !liveDefs[norm] {
				drift = append(drift, CheckConstraintDrift{
					Kind:       CheckDeclaredNotEnforced,
					EntityID:   dt.entity.ID(),
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
					EntityID:       dt.entity.ID(),
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
	return drift, notes, nil
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
func loadLiveChecks(ctx context.Context, q Querier, pairs []physRef) (map[physRef][]liveCheck, error) {
	schemas, tables := splitPairs(pairs)
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT n.nspname, c.relname, con.conname, pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_class c     ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN targets tg     ON tg.schema = n.nspname AND tg.table_name = c.relname
WHERE con.contype = 'c'`, schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[physRef][]liveCheck)
	for rows.Next() {
		var s, t, name, def string
		if err := rows.Scan(&s, &t, &name, &def); err != nil {
			return nil, err
		}
		key := physRef{s, t}
		out[key] = append(out[key], liveCheck{name: name, def: def})
	}
	return out, rows.Err()
}
