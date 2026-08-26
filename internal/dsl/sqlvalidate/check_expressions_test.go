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

// Declarations PostgreSQL accepts, through the real entry point.
//
// The gate refuses anything it cannot parse, and AuditForbiddenCalls runs it
// over the ALREADY-STORED IR at boot, where a refusal is fatal under
// ATL_REQUIRE_TENANT_ISOLATION. So a false rejection does not fail a `tide
// apply` — it stops a server that started yesterday from starting today, over
// a constraint the database has been enforcing for months. Both shapes below
// were rejected: named-parameter rewriting is applied before the parse, and it
// was not aware of dollar-quoting or of comments.
//
// Note the contrast with auditBody, which deliberately SKIPS an unparseable
// stored query body for exactly this reason. The entity path did not.
//
// A trailing `--` comment is NOT in this list: see the test below.
func TestValidateEntityExpressions_AcceptsDollarQuotesAndComments(t *testing.T) {
	e := &dsl.Entity{
		Name: "Person", Namespace: "hr",
		Fields: []dsl.Field{
			{Name: "email", Type: dsl.FieldType{Name: "text"}},
			{Name: "age", Type: dsl.FieldType{Name: "int"}, Check: "age > 0"},
		},
		Checks: []dsl.TableCheck{
			{Name: "ck_at", Expr: "position($tag$@$tag$ in email) > 0"},
			{Name: "ck_dd", Expr: "email <> $$$$"},
			{Name: "ck_cmt", Expr: "length(email) > 3 /* don't allow stubs */"},
		},
		Indexes: []dsl.Index{{
			Kind:   dsl.IndexBtree,
			Fields: []dsl.IndexField{{Expr: "position($tag$@$tag$ in email)", IsExpr: true}},
		}},
	}
	if err := ValidateEntityExpressions(e); err != nil {
		t.Errorf("rejected constraints PostgreSQL accepts: %v.\nThis refuses boot "+
			"and every hot reload on a healthy deployment", err)
	}
}

// A trailing `--` comment is rejected, and that is correct.
//
// It looks like the same false-rejection class as the dollar-quote above, and
// it is not. The check is emitted inline — `CHECK (<expr>)` on one line — so
// the comment swallows the closing parenthesis. PostgreSQL rejects the emitted
// text with the identical "syntax error at end of input":
//
//	CREATE TABLE t (age int, CONSTRAINT ck CHECK (age > 0 -- nope));
//	ERROR:  syntax error at end of input
//
// The gate is agreeing with the database rather than contradicting it, which
// is the whole point of parsing in the emitted shape. Block comments are
// accepted because they close.
func TestValidateEntityExpressions_RejectsTrailingLineComment(t *testing.T) {
	e := &dsl.Entity{
		Name: "Person", Namespace: "hr",
		Fields: []dsl.Field{
			{Name: "age", Type: dsl.FieldType{Name: "int"},
				Check: "age > 0 -- don't allow zero"},
		},
	}
	if err := ValidateEntityExpressions(e); err == nil {
		t.Error("accepted a check whose trailing line comment removes the closing " +
			"parenthesis. PostgreSQL rejects the emitted DDL, so accepting it here " +
			"moves the failure from `tide apply` to the migration")
	}
}

// An entity with nothing to check must not error.
func TestValidateEntityExpressions_QuietOnEmpty(t *testing.T) {
	if err := ValidateEntityExpressions(&dsl.Entity{Name: "E", Namespace: "n"}); err != nil {
		t.Errorf("errored on an entity with no expressions: %v", err)
	}
}

// An expression that does not parse as a CHECK constraint is rejected, not
// skipped as PostgreSQL's problem to report.
//
// A gate that parses `SELECT (<expr>)` while codegen emits
// `CONSTRAINT c CHECK (<expr>)` skips exactly the expressions that close the
// CHECK's parenthesis early: they fail as a SELECT while remaining valid DDL
// that plants a second constraint. Executed against PostgreSQL 17.8 as a
// NOSUPERUSER NOBYPASSRLS role, an ordinary INSERT then fires the planted
// set_config and the rest of the transaction reads another tenant's rows.
//
// The text is validated in the syntactic context it is emitted into.
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

// `default raw "<sql>"` is the fifth caller-authored SQL surface.
//
// codegen emits it into the column definition, and internal/server/entity
// inlines it into every generated INSERT as `COALESCE($n::type, <raw expr>)`.
// So it runs on every insert that omits the column, not only at CREATE TABLE.
// An adversarial review put set_config in one and rebound the tenant
// mid-transaction on PostgreSQL 17.8 while this gate reported nothing.
func TestValidateEntityExpressions_RejectsSetConfigInRawDefaults(t *testing.T) {
	for _, expr := range []string{
		`set_config('atlantis.tenant','victim',true)`,
		`coalesce(set_config('atlantis.tenant','victim',true), 'x')`,
		`atlantis.set_partition('victim')`,
	} {
		e := &dsl.Entity{
			Name: "Doc", Namespace: "shop", PartitionField: "tenant",
			Fields: []dsl.Field{
				{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{
					Name: "body", Type: dsl.FieldType{Name: "text"},
					Default: &dsl.Default{Kind: dsl.DefaultIRRaw, Str: expr},
				},
			},
		}
		assertRejected(t, ValidateEntityExpressions(e), expr)
	}
}

// Ordinary raw defaults must still pass, or the gate is a grammar restriction.
func TestValidateEntityExpressions_AcceptsOrdinaryRawDefaults(t *testing.T) {
	for _, expr := range []string{"now()", "gen_random_uuid()", "0", "''::text"} {
		e := &dsl.Entity{
			Name: "Doc", Namespace: "shop",
			Fields: []dsl.Field{{
				Name: "created_at", Type: dsl.FieldType{Name: "timestamptz"},
				Default: &dsl.Default{Kind: dsl.DefaultIRRaw, Str: expr},
			}},
		}
		if err := ValidateEntityExpressions(e); err != nil {
			t.Errorf("rejected an ordinary raw default %q: %v", expr, err)
		}
	}
}

// `index by expr "<sql>"` is emitted verbatim into CREATE INDEX and was gated
// by nothing at all.
//
// internal/dsl/ir.go says outright that PostgreSQL validates it at migration
// time. That is not a gate: the escape is a STATEMENT boundary, not a bad
// expression, so PostgreSQL applies it happily. A review closed the emitted
// parenthesis and appended DDL that drops the tenant-isolation policy — the
// policy every other check in this package exists to protect.
func TestValidateEntityExpressions_RejectsIndexExpressionEscapes(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{
			// The reviewer's payload, generalised.
			name: "closes CREATE INDEX and drops the policy",
			expr: `lower(email)); DROP POLICY IF EXISTS p ON atlantis.doc; CREATE INDEX zz ON atlantis.doc ((1`,
		},
		{
			name: "closes CREATE INDEX and appends any DDL",
			expr: `lower(email)); ALTER TABLE atlantis.doc DISABLE ROW LEVEL SECURITY; CREATE INDEX zz ON atlantis.doc ((1`,
		},
		{
			name: "rebinds the tenant from an index expression",
			expr: `coalesce(set_config('atlantis.tenant','victim',true), email)`,
		},
		{
			name: "plain syntax error",
			expr: `lower(email`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Doc", Namespace: "shop",
				Fields: []dsl.Field{{Name: "email", Type: dsl.FieldType{Name: "text"}}},
				Indexes: []dsl.Index{{
					Kind:   dsl.IndexBtree,
					Fields: []dsl.IndexField{{Expr: tc.expr, IsExpr: true}},
				}},
			}
			if err := ValidateEntityExpressions(e); err == nil {
				t.Errorf("accepted an index expression that does not stay inside its "+
					"own CREATE INDEX. atlantis emits it verbatim:\n  %s", tc.expr)
			}
		})
	}
}

// Ordinary index expressions must still pass.
func TestValidateEntityExpressions_AcceptsOrdinaryIndexExpressions(t *testing.T) {
	for _, expr := range []string{
		`lower(email)`,
		`(email || '-' || status)`,
		`coalesce(status, 'draft')`,
		`date_trunc('day', created_at)`,
	} {
		e := &dsl.Entity{
			Name: "Doc", Namespace: "shop",
			Fields: []dsl.Field{{Name: "email", Type: dsl.FieldType{Name: "text"}}},
			Indexes: []dsl.Index{{
				Kind:   dsl.IndexBtree,
				Fields: []dsl.IndexField{{Expr: expr, IsExpr: true}},
			}},
		}
		if err := ValidateEntityExpressions(e); err != nil {
			t.Errorf("rejected an ordinary index expression %q: %v", expr, err)
		}
	}
}
