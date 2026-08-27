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
// Entities whose tables do not exist are left alone.
func Enrich(ctx context.Context, q Querier, ir *dsl.IR) error {
	if ir == nil || len(ir.Entities) == 0 {
		return nil
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
		return fmt.Errorf("enrich: load live checks: %w", err)
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
		return fmt.Errorf("enrich: load existing tables: %w", err)
	}
	cons, err := loadConstraints(ctx, q, pairs, existing)
	if err != nil {
		return fmt.Errorf("enrich: load constraints: %w", err)
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

	return nil
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
