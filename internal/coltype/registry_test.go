package coltype

import "testing"

// A predicate message carries equality and range comparisons, so a type can
// only publish one if Postgres can both compare and sort it. Postgres defines
// `=` on box, circle, line, lseg and path and sorts none of them, so those
// five are comparable with no predicate.
//
// Without this, a filter or primary key on such a column is DDL-clean and
// fails at the database.
func TestAPredicateImpliesBothOperators(t *testing.T) {
	for name, i := range registry {
		if i.pred == "" {
			continue
		}
		if !i.comparable {
			t.Errorf("%s publishes predicate %q but is not comparable", name, i.pred)
		}
		if !i.orderable {
			t.Errorf("%s publishes predicate %q but is not orderable", name, i.pred)
		}
	}
}

// Every registered type renders a SQL spelling. A row with an empty sqlName
// reaches CREATE TABLE as a bare column name.
func TestEveryRegisteredTypeHasASQLSpelling(t *testing.T) {
	for name, i := range registry {
		if i.sqlName == "" {
			t.Errorf("%s has no Postgres spelling", name)
		}
		if i.param != paramNone && i.param != paramVecDim && i.unbounded == "" {
			t.Errorf("%s takes a parameter but has no spelling for the bare form", name)
		}
	}
}
