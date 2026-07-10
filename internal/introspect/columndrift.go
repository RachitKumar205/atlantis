package introspect

// columndrift.go detects divergence between a declared column's type/width and
// the type the live table actually has.
//
// The plan/diff path compares the new IR against the IR *checkpoint*, never
// the live DB. So a column whose live type drifted from the declaration at
// adoption (a pre-atlantis migration that created `vendor_cart_id varchar(10)`
// while the .atl says `varchar(255)`) stays divergent: the checkpoint matches
// the .atl, the diff is empty, and the mismatch only surfaces as a runtime
// 22001 (value too long) once real data exceeds the un-widened column. The
// varchar-length diff fix closed the checkpoint→.atl half; this closes the
// checkpoint→live half.
//
// Comparison is by Postgres's own format_type deparse, not by hand-mapping
// atlantis type names to PG ones: each declared column is rendered onto a
// throwaway TEMP table carrying its declared type, and format_type(atttypid,
// atttypmod) is read back and compared to the live column's format_type. Both
// sides are canonicalized by the same engine — `varchar(255)` ⇒ `character
// varying(255)`, `int` ⇒ `integer`, `timestamptz` ⇒ `timestamp with time
// zone`, `bigserial` ⇒ `bigint` (the serial-ness is a default, not a type) —
// so the only differences reported are genuine type/width divergences.
//
// Scope: columns present in BOTH the declaration and the live table. A
// declared column missing live is an ADD the normal plan emits; a live column
// missing from the declaration is handled elsewhere. Read-only; reports only.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// ColumnTypeDrift is one column whose live type differs from the declared one.
// JSON-tagged to round-trip through the admin PlanResponse to the CLI.
type ColumnTypeDrift struct {
	EntityID string `json:"entity_id"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
	Column   string `json:"column"`
	// Declared and Live are Postgres format_type deparses (e.g.
	// "character varying(255)" vs "character varying(10)").
	Declared string `json:"declared"`
	Live     string `json:"live"`
}

// Describe renders an operator-facing one-liner.
func (d ColumnTypeDrift) Describe() string {
	return fmt.Sprintf("%s.%s.%s: declared %s, live %s", d.Schema, d.Table, d.Column, d.Declared, d.Live)
}

// DetectColumnTypeDrift compares each declared column's type against the live
// column's type and reports the mismatches. Read-only; safe at plan time or
// inside the apply tx.
func DetectColumnTypeDrift(ctx context.Context, q DBTX, declaredIR *dsl.IR) ([]ColumnTypeDrift, []string, error) {
	if declaredIR == nil {
		return nil, nil, fmt.Errorf("introspect: declaredIR is required")
	}
	entities := make(map[physRef]*dsl.Entity, len(declaredIR.Entities))
	for i := range declaredIR.Entities {
		e := &declaredIR.Entities[i]
		s, t := physical(e)
		entities[physRef{s, t}] = e
	}
	if len(entities) == 0 {
		return nil, nil, nil
	}

	pairs := make([]physRef, 0, len(entities))
	for r := range entities {
		pairs = append(pairs, r)
	}
	live, err := loadLiveColumnTypes(ctx, q, pairs)
	if err != nil {
		return nil, nil, err
	}

	var drift []ColumnTypeDrift
	var notes []string
	for ref, e := range entities {
		liveCols := live[ref]
		if len(liveCols) == 0 {
			continue // table not in live DB — the plan emits CREATE TABLE
		}
		declared, ok := renderDeclaredColumnTypes(ctx, q, e)
		if !ok {
			notes = append(notes, fmt.Sprintf("%s: could not introspect declared column types for comparison", e.ID()))
			continue
		}
		for i := range e.Fields {
			name := e.Fields[i].Name
			dt, okD := declared[name]
			lt, okL := liveCols[name]
			if !okD || !okL {
				continue // ADD (declared-only) or unrelated live column
			}
			if dt != lt {
				drift = append(drift, ColumnTypeDrift{
					EntityID: e.ID(), Schema: ref.schema, Table: ref.table,
					Column: name, Declared: dt, Live: lt,
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
		return a.Column < b.Column
	})
	return drift, notes, nil
}

// renderDeclaredColumnTypes returns each declared field's type as Postgres
// stores it (format_type), by creating a throwaway TEMP table with the
// entity's columns and reading their format_type back. Rolled back; no
// persistent objects. ok=false on any DB error (the caller emits a note).
func renderDeclaredColumnTypes(ctx context.Context, db DBTX, e *dsl.Entity) (map[string]string, bool) {
	if len(e.Fields) == 0 {
		return nil, false
	}
	defs := make([]string, 0, len(e.Fields))
	for i := range e.Fields {
		f := &e.Fields[i]
		defs = append(defs, schema.QuoteIdent(f.Name)+" "+schema.SQLType(f.Type))
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "CREATE TEMP TABLE _atl_coltype ("+strings.Join(defs, ", ")+")"); err != nil {
		return nil, false
	}
	rows, err := tx.Query(ctx, `
SELECT attname, format_type(atttypid, atttypmod)
FROM pg_attribute
WHERE attrelid = '_atl_coltype'::regclass AND attnum > 0 AND NOT attisdropped`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := make(map[string]string, len(e.Fields))
	for rows.Next() {
		var name, ft string
		if err := rows.Scan(&name, &ft); err != nil {
			return nil, false
		}
		out[name] = ft
	}
	if rows.Err() != nil {
		return nil, false
	}
	return out, true
}

// loadLiveColumnTypes reads format_type for every column of the given physical
// tables, keyed by table then column name.
func loadLiveColumnTypes(ctx context.Context, q Querier, pairs []physRef) (map[physRef]map[string]string, error) {
	schemas, tables := splitPairs(pairs)
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a
JOIN pg_class c     ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN targets tg     ON tg.schema = n.nspname AND tg.table_name = c.relname
WHERE a.attnum > 0 AND NOT a.attisdropped`, schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[physRef]map[string]string)
	for rows.Next() {
		var s, t, col, ft string
		if err := rows.Scan(&s, &t, &col, &ft); err != nil {
			return nil, err
		}
		key := physRef{s, t}
		if out[key] == nil {
			out[key] = make(map[string]string)
		}
		out[key][col] = ft
	}
	return out, rows.Err()
}
