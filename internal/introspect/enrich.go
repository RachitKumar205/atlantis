package introspect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Enrich fills in what FromPostgres leaves to the declaration.
//
// FromPostgres carries CHECK predicates and composite UNIQUEs across from the
// declaration it was given: a pass that compares needs both sides to describe
// the same thing, and reading one side live made every adopt of a database
// whose CHECKs already existed report them as drift to be added. See the note
// on Field.Check in assembleEntity.
//
// A pass that generates has no declaration to carry from — its input is a stub
// — so those arrive empty and the emitted .atl understates the database. This
// reads them from the catalogue instead, and belongs only on that path.
//
// Returns one note per index it read but could not spell; see
// loadSecondaryIndexes for what those are.
//
// Entities whose tables do not exist are left alone.
func Enrich(ctx context.Context, q Querier, ir *dsl.IR) ([]string, error) {
	if ir == nil || len(ir.Entities) == 0 {
		return nil, nil
	}

	pairs := make([]physRef, 0, len(ir.Entities))
	idx := make(map[physRef]int, len(ir.Entities))
	for i := range ir.Entities {
		s, t := physical(&ir.Entities[i])
		p := physRef{schema: s, table: t}
		pairs = append(pairs, p)
		idx[p] = i
	}

	checks, err := loadLiveChecks(ctx, q, pairs)
	if err != nil {
		return nil, fmt.Errorf("enrich: load live checks: %w", err)
	}
	for p, live := range checks {
		i, ok := idx[p]
		if !ok {
			continue
		}
		e := &ir.Entities[i]
		for _, c := range live {
			expr := checkBody(c.def)
			if expr == "" {
				continue
			}
			e.Checks = append(e.Checks, dsl.TableCheck{Name: c.name, Expr: expr})
		}
		sort.Slice(e.Checks, func(a, b int) bool { return e.Checks[a].Name < e.Checks[b].Name })
	}

	existing, err := loadExistingTables(ctx, q, pairs)
	if err != nil {
		return nil, fmt.Errorf("enrich: load existing tables: %w", err)
	}
	cons, err := loadConstraints(ctx, q, pairs, existing)
	if err != nil {
		return nil, fmt.Errorf("enrich: load constraints: %w", err)
	}
	for p, c := range cons {
		i, ok := idx[p]
		if !ok {
			continue
		}
		e := &ir.Entities[i]
		// Single-column uniqueness is already on the field, set from the live
		// catalogue by assembleEntity. Repeating it here would emit a column
		// marked `unique` and a redundant UNIQUE(col) beside it.
		for _, u := range c.uniqs {
			if len(u.cols) < 2 {
				continue
			}
			e.Uniques = append(e.Uniques, dsl.UniqueSpec{Fields: append([]string(nil), u.cols...)})
		}
		sort.Slice(e.Uniques, func(a, b int) bool {
			return strings.Join(e.Uniques[a].Fields, ",") < strings.Join(e.Uniques[b].Fields, ",")
		})
	}

	idxs, notes, err := loadSecondaryIndexes(ctx, q, pairs)
	if err != nil {
		return nil, fmt.Errorf("enrich: load secondary indexes: %w", err)
	}
	for p, live := range idxs {
		i, ok := idx[p]
		if !ok {
			continue
		}
		e := &ir.Entities[i]
		sort.Slice(live, func(a, b int) bool { return live[a].name < live[b].name })
		for _, li := range live {
			fields := make([]dsl.IndexField, len(li.cols))
			for c, col := range li.cols {
				fields[c] = dsl.IndexField{Name: col, Desc: c < len(li.descs) && li.descs[c]}
			}
			e.Indexes = append(e.Indexes, dsl.Index{Kind: dsl.IndexBtree, Fields: fields})
		}
	}

	return notes, nil
}

// liveIndex is one secondary index read from the catalogue.
type liveIndex struct {
	name  string
	cols  []string
	descs []bool
}

// loadSecondaryIndexes reads the indexes an `index by` line can spell, keyed by
// table, plus one note per index it could not.
//
// Excluded by the query: primary keys, invalid indexes, and anything a
// pg_constraint row owns — those are UNIQUE and PRIMARY KEY constraints, which
// loadConstraints already reads and assembleEntity already emits. Reading them
// here as well would declare the same thing twice.
//
// Excluded by the scan, each with a note, because .atl has no way to say them
// from a discovered table:
//
//   - expression indexes — `index by expr "lower(email)"` exists, but the
//     expression lives in pg_get_indexdef rather than in indkey, and attnum 0
//     is all the catalogue gives here
//   - partial indexes — `Index.Where` is a parsed predicate tree, not the
//     pg_get_expr text
//   - unique indexes not backed by a constraint — `unique` and `unique by` both
//     emit constraints, and the only partial form is `unique index partial`
//   - anything that is not btree — GIN and HNSW have their own clauses and
//     their own operator classes
//
// A note rather than silence: an index that is neither declared nor mentioned
// is one the first plan proposes creating on a table that already has it.
func loadSecondaryIndexes(ctx context.Context, q Querier, pairs []physRef) (map[physRef][]liveIndex, []string, error) {
	schemas, tables := splitPairs(pairs)
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT
    n.nspname,
    c.relname,
    ic.relname AS index_name,
    i.indisunique,
    (i.indpred IS NOT NULL) AS is_partial,
    am.amname,
    (SELECT array_agg(a.attname ORDER BY x.ord)
       FROM unnest(string_to_array(i.indkey::text, ' ')::int[]) WITH ORDINALITY AS x(attnum, ord)
       JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = x.attnum
      WHERE x.attnum <> 0) AS col_names,
    (SELECT array_agg((o.opt & 1) = 1 ORDER BY o.ord)
       FROM unnest(string_to_array(i.indoption::text, ' ')::int[]) WITH ORDINALITY AS o(opt, ord)) AS descs,
    EXISTS (
        SELECT 1
          FROM unnest(string_to_array(i.indkey::text, ' ')::int[]) AS y(attnum)
         WHERE y.attnum = 0
    ) AS has_expr
FROM pg_index i
JOIN pg_class c       ON c.oid = i.indrelid
JOIN pg_class ic      ON ic.oid = i.indexrelid
JOIN pg_am am         ON am.oid = ic.relam
JOIN pg_namespace n   ON n.oid = c.relnamespace
JOIN targets tg       ON tg.schema = n.nspname AND tg.table_name = c.relname
WHERE i.indisprimary = false
  AND i.indisvalid   = true
  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid)`,
		schemas, tables)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	out := make(map[physRef][]liveIndex)
	var notes []string
	for rows.Next() {
		var (
			s, t, name, amName string
			isUnique, isPart   bool
			hasExpr            bool
			cols               []string
			descs              []bool
		)
		if err := rows.Scan(&s, &t, &name, &isUnique, &isPart, &amName, &cols, &descs, &hasExpr); err != nil {
			return nil, nil, err
		}

		switch {
		case hasExpr || len(cols) == 0:
			notes = append(notes, fmt.Sprintf("%s.%s: index %s indexes an expression and is not declared", s, t, name))
		case isPart:
			notes = append(notes, fmt.Sprintf("%s.%s: index %s is partial and is not declared", s, t, name))
		case isUnique:
			notes = append(notes, fmt.Sprintf("%s.%s: index %s is a unique index with no constraint behind it and is not declared", s, t, name))
		case amName != "btree":
			notes = append(notes, fmt.Sprintf("%s.%s: index %s is %s, not btree, and is not declared", s, t, name, amName))
		default:
			p := physRef{schema: s, table: t}
			out[p] = append(out[p], liveIndex{name: name, cols: cols, descs: descs})
		}
	}
	sort.Strings(notes)
	return out, notes, rows.Err()
}

// checkBody strips the CHECK wrapper off a pg_get_constraintdef deparse.
//
// Postgres renders `total > 0` as `CHECK ((total > 0))`. The .atl `check`
// clause carries the predicate alone, so the wrapper has to come off; the inner
// parentheses are kept, being part of the expression Postgres will re-deparse
// to the same string.
//
// A deparse this does not recognise yields "", and the caller drops it. A
// half-stripped predicate would emit .atl that does not parse, which is worse
// than a declaration missing a constraint the warnings already mention.
func checkBody(def string) string {
	const prefix = "CHECK ("
	s := strings.TrimSpace(def)

	// NOT VALID marks a constraint Postgres has not verified against existing
	// rows. The .atl clause has no way to say so, and emitting the predicate
	// without it would claim a guarantee the database does not make.
	if strings.HasSuffix(s, " NOT VALID") {
		return ""
	}
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, ")") {
		return ""
	}
	return strings.TrimSpace(s[len(prefix) : len(s)-1])
}
