package introspect

// columndrift.go detects divergence between a declared column's type/width and
// the type the live table actually has.
//
// The plan/diff path compares the new IR against the IR checkpoint, never the
// live database. A column whose live type diverged from the declaration at
// adoption — a pre-atlantis migration creating a column `varchar(10)` where
// the .atl says `varchar(255)` — therefore stays divergent: the
// checkpoint matches the .atl, the diff is empty, and the mismatch surfaces as
// SQLSTATE 22001 once data exceeds the un-widened column.
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
// Scope is columns present in both the declaration and the live table. A
// declared column absent live is an ADD the normal plan emits, and a live
// column absent from the declaration is handled elsewhere. Read-only.

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
	declaredAll := renderDeclaredColumnTypes(ctx, q, entities)
	for ref, e := range entities {
		liveCols := live[ref]
		if len(liveCols) == 0 {
			continue // table not in live DB — the plan emits CREATE TABLE
		}
		declared, ok := declaredAll[e.ID()]
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
// stores it (format_type), for every entity at once, keyed by entity ID then
// column name.
//
// One temp table per entity, all created in a single DO block, then one read
// covering all of them: four round trips for the whole schema rather than four
// per entity. Against a database on the other side of the internet that is the
// difference between a plan that returns and one that looks hung — 97 entities
// at 165ms to EBI's public mirror is 64 seconds of latency alone.
//
// A DO block rather than one wide table because a table is capped at 1600
// columns and a schema of this size passes that. Rolled back; no persistent
// objects.
//
// A missing entity is absent from the result rather than failing the batch, and
// the caller emits a note for it.
func renderDeclaredColumnTypes(ctx context.Context, db DBTX, entities map[physRef]*dsl.Entity) map[string]map[string]string {
	type target struct {
		id    string
		table string
	}
	var stmts []string
	targets := make([]target, 0, len(entities))
	i := 0
	for _, e := range entities {
		if len(e.Fields) == 0 {
			continue
		}
		defs := make([]string, 0, len(e.Fields))
		for j := range e.Fields {
			f := &e.Fields[j]
			defs = append(defs, schema.QuoteIdent(f.Name)+" "+schema.SQLType(f.Type))
		}
		name := fmt.Sprintf("_atl_coltype_%d", i)
		i++
		stmts = append(stmts, fmt.Sprintf("CREATE TEMP TABLE %s (%s);", name, strings.Join(defs, ", ")))
		targets = append(targets, target{id: e.ID(), table: name})
	}
	if len(stmts) == 0 {
		return nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "DO $atl$ BEGIN\n"+strings.Join(stmts, "\n")+"\nEND $atl$;"); err != nil {
		return nil
	}

	names := make([]string, len(targets))
	byTable := make(map[string]string, len(targets))
	for k, t := range targets {
		names[k] = t.table
		byTable[t.table] = t.id
	}
	rows, err := tx.Query(ctx, `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname LIKE 'pg\_temp%'
  AND c.relname = ANY($1)
  AND a.attnum > 0 AND NOT a.attisdropped`, names)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make(map[string]map[string]string, len(targets))
	for rows.Next() {
		var table, col, ft string
		if err := rows.Scan(&table, &col, &ft); err != nil {
			return nil
		}
		id, ok := byTable[table]
		if !ok {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][col] = ft
	}
	if rows.Err() != nil {
		return nil
	}
	return out
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
