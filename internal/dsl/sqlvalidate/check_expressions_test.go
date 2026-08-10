package sqlvalidate

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// A CHECK expression is caller-authored SQL, and it was not gated.
//
// The gate was wired into validateBlock, which sees query bodies and procedure
// steps. A `check "..."` takes a different path entirely — it is carried on the
// entity and emitted verbatim as `CONSTRAINT <name> CHECK (<expr>)` — so
// nothing inspected it. PostgreSQL does not require a CHECK expression to be
// IMMUTABLE, so a volatile call planted there runs on every INSERT and UPDATE.
//
// Reproduced on 17.8 by an adversarial review, as a NOSUPERUSER NOBYPASSRLS
// role: a transaction correctly bound to tenant A performed an INSERT, the
// CHECK fired, and the discriminator read tenant B for the rest of the
// transaction. It defeats the once-only guard as well, because set_config
// writes the parameter without going through the setter.
func TestValidateEntityExpressions_RejectsSetConfigInChecks(t *testing.T) {
	hostile := []string{
		`set_config('atlantis.tenant','victim',true) IS NOT NULL`,
		`pg_catalog.set_config('atlantis.tenant','victim',true) IS NOT NULL`,
		`atlantis.set_partition('victim') IS NOT NULL`,
		// Buried where a reviewer's eye slides past it.
		`status IN ('open','closed') OR coalesce(set_config('atlantis.tenant','victim',true),'') = ''`,
		`CASE WHEN status = 'x' THEN set_config('atlantis.tenant','victim',true) ELSE 'y' END IS NOT NULL`,
	}

	t.Run("entity-level check", func(t *testing.T) {
		for _, expr := range hostile {
			e := &dsl.Entity{
				Name: "Order", Namespace: "shop",
				Fields: []dsl.Field{{Name: "status", Type: dsl.FieldType{Name: "text"}}},
				Checks: []dsl.TableCheck{{Name: "ck", Expr: expr}},
			}
			assertRejected(t, ValidateEntityExpressions(e), expr)
		}
	})

	t.Run("field-level check", func(t *testing.T) {
		for _, expr := range hostile {
			e := &dsl.Entity{
				Name: "Order", Namespace: "shop",
				Fields: []dsl.Field{{
					Name: "status", Type: dsl.FieldType{Name: "text"}, Check: expr,
				}},
			}
			assertRejected(t, ValidateEntityExpressions(e), expr)
		}
	})

	t.Run("partial-index predicate", func(t *testing.T) {
		for _, expr := range hostile {
			e := &dsl.Entity{
				Name: "Order", Namespace: "shop",
				Fields: []dsl.Field{{Name: "status", Type: dsl.FieldType{Name: "text"}}},
				Indexes: []dsl.Index{{
					Kind:   dsl.IndexPartial,
					Fields: []dsl.IndexField{{Name: "status"}},
					Where:  &dsl.PredExpr{Kind: dsl.PredKindExpr, Text: expr},
				}},
			}
			// PostgreSQL refuses a volatile function in an index predicate on
			// its own, so this one is belt and braces — but the gate's coverage
			// must not depend on a rule atlantis does not control.
			assertRejected(t, ValidateEntityExpressions(e), expr)
		}
	})
}

func assertRejected(t *testing.T, err error, expr string) {
	t.Helper()
	if err == nil {
		t.Errorf("accepted an expression that rebinds the tenant:\n  %s", expr)
		return
	}
	if !strings.Contains(err.Error(), "set_config") &&
		!strings.Contains(err.Error(), "set_partition") {
		t.Errorf("rejected %q, but not for the reason that matters: %v", expr, err)
	}
}

// And ordinary constraints must still be accepted, or this is a grammar
// restriction wearing a security argument.
func TestValidateEntityExpressions_AcceptsOrdinaryChecks(t *testing.T) {
	e := &dsl.Entity{
		Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{
			{Name: "status", Type: dsl.FieldType{Name: "text"}},
			{Name: "quantity", Type: dsl.FieldType{Name: "int"}, Check: "quantity > 0"},
		},
		Checks: []dsl.TableCheck{
			{Name: "ck_status", Expr: "status IN ('draft', 'active', 'archived')"},
			{Name: "ck_ship", Expr: "shipping_method IS NULL OR shipping_method IN ('standard', 'express')"},
			// Reads a parameter; does not change one.
			{Name: "ck_read", Expr: "current_setting('atlantis.tenant', true) IS NOT NULL"},
		},
		Indexes: []dsl.Index{{
			Kind:   dsl.IndexPartial,
			Fields: []dsl.IndexField{{Name: "status"}},
			Where:  &dsl.PredExpr{Kind: dsl.PredKindExpr, Text: "status = 'active'"},
		}},
	}
	if err := ValidateEntityExpressions(e); err != nil {
		t.Errorf("rejected ordinary constraints: %v", err)
	}
}

// An entity with nothing to check must not error.
func TestValidateEntityExpressions_QuietOnEmpty(t *testing.T) {
	if err := ValidateEntityExpressions(&dsl.Entity{Name: "E", Namespace: "n"}); err != nil {
		t.Errorf("errored on an entity with no expressions: %v", err)
	}
}

// An expression that does not parse as a CHECK constraint must be REJECTED,
// not waved through.
//
// This test previously asserted the opposite — that a syntax error was
// PostgreSQL's to report, so skipping it here was safe. That was wrong, and an
// adversarial review turned it into a working cross-tenant leak. The gate
// parsed `SELECT (<expr>)` while codegen emits `CONSTRAINT c CHECK (<expr>)`,
// so an expression that closes the CHECK's parenthesis early fails as a SELECT
// — and was skipped — while remaining valid DDL that plants a second
// constraint. Executed against PostgreSQL 17.8 as a NOSUPERUSER NOBYPASSRLS
// role, an ordinary INSERT then fired the planted set_config and the rest of
// the transaction read another tenant's rows.
//
// The lesson is narrower than "always reject syntax errors": validate the text
// in the syntactic context it is emitted into, because a parse that succeeds
// somewhere else proves nothing about the place it lands.
func TestValidateEntityExpressions_RejectsExpressionsThatEscapeTheConstraint(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
	}{
		{
			// The reviewer's payload, verbatim.
			name: "closes its CHECK and opens another carrying set_config",
			expr: `true) , CONSTRAINT ck_evil CHECK (set_config('atlantis.tenant','victim',true) IS NOT NULL`,
		},
		{
			// The same escape without a forbidden call. It must still be
			// refused: atlantis emits this verbatim, so an expression that
			// declares constraints of its own is outside what `check` means,
			// whatever it happens to declare today.
			name: "closes its CHECK and opens a plain one",
			expr: `true) , CONSTRAINT ck_extra CHECK (1 = 1`,
		},
		{
			name: "escapes into a column-level constraint",
			expr: `true), extra_col integer CHECK (set_config('atlantis.tenant','victim',true) IS NOT NULL`,
		},
		{
			name: "plain syntax error",
			expr: `status IN (((`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Doc", Namespace: "shop",
				Fields: []dsl.Field{{Name: "status", Type: dsl.FieldType{Name: "text"}}},
				Checks: []dsl.TableCheck{{Name: "ck", Expr: tc.expr}},
			}
			if err := ValidateEntityExpressions(e); err == nil {
				t.Errorf("accepted an expression that does not stay inside its own "+
					"CHECK constraint. atlantis emits it verbatim into CREATE TABLE:\n  %s",
					tc.expr)
			}
		})
	}
}
