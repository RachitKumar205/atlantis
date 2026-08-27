package adopt

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Reading a whole database, one namespace per schema.
//
// The case that drove it: a public research database keeps all 186 of its
// tables in one non-public schema, so a pass that read "public" — the
// search_path default, and what a form offers — found nothing and reported "0
// tables" against a database full of them.

// generateAllIn creates the schemas, runs the whole-database pass, and drops
// them again.
func generateAllIn(t *testing.T, ddl []string, schemas ...string) Result {
	t.Helper()
	pool := adoptPool(t)
	ctx := context.Background()

	drop := func() {
		for _, s := range schemas {
			_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+s+`" CASCADE`)
		}
	}
	drop()
	t.Cleanup(drop)

	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	res, err := GenerateAll(ctx, tx, nil)
	if err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	return res
}

// nsOf returns the namespace a table was read into.
func nsOf(t *testing.T, res Result, table string) string {
	t.Helper()
	for _, e := range res.Entities {
		if e.Table == table {
			return e.Namespace
		}
	}
	t.Fatalf("no declaration for %s; got %d entities", table, len(res.Entities))
	return ""
}

// Every schema is read, and each lands in a namespace named after it.
func TestEverySchemaIsReadIntoItsOwnNamespace(t *testing.T) {
	res := generateAllIn(t, []string{
		`CREATE SCHEMA adopt_a`,
		`CREATE SCHEMA adopt_b`,
		`CREATE TABLE adopt_a.orders (id TEXT PRIMARY KEY)`,
		`CREATE TABLE adopt_b.invoices (id TEXT PRIMARY KEY)`,
	}, "adopt_a", "adopt_b")

	if got := nsOf(t, res, "adopt_a.orders"); got != "adopt_a" {
		t.Errorf("adopt_a.orders is in namespace %q, want adopt_a", got)
	}
	if got := nsOf(t, res, "adopt_b.invoices"); got != "adopt_b" {
		t.Errorf("adopt_b.invoices is in namespace %q, want adopt_b", got)
	}
	// The declaration has to say so too. The namespace on the struct is what
	// the console groups by; the `in` clause is what a caller commits.
	for _, e := range res.Entities {
		if !strings.Contains(e.Atl, " in "+e.Namespace) {
			t.Errorf("%s: the declaration does not say `in %s`:\n%s", e.Table, e.Namespace, e.Atl)
		}
	}
}

// Two schemas holding a table of the same name both survive.
//
// The reason for one namespace per schema. Merged into one, the second table
// would collide on the entity name and be dropped into Skipped — a table
// silently missing from a whole-database read.
func TestTheSameTableNameInTwoSchemasIsNotACollision(t *testing.T) {
	res := generateAllIn(t, []string{
		`CREATE SCHEMA adopt_x`,
		`CREATE SCHEMA adopt_y`,
		`CREATE TABLE adopt_x.orders (id TEXT PRIMARY KEY)`,
		`CREATE TABLE adopt_y.orders (id TEXT PRIMARY KEY)`,
	}, "adopt_x", "adopt_y")

	if nsOf(t, res, "adopt_x.orders") != "adopt_x" || nsOf(t, res, "adopt_y.orders") != "adopt_y" {
		t.Errorf("the two orders tables did not land in separate namespaces")
	}
	for _, s := range res.Skipped {
		if strings.Contains(s, "orders") {
			t.Errorf("a table was skipped in a whole-database read: %s", s)
		}
	}
	// Both declarations must lower together, which is what proves the two
	// entity IDs differ.
	var all strings.Builder
	for _, e := range res.Entities {
		if e.Namespace == "adopt_x" || e.Namespace == "adopt_y" {
			all.WriteString(e.Atl)
			all.WriteString("\n")
		}
	}
	mustLower(t, all.String())
}

// A schema whose name atlantis cannot address is named and left out.
//
// Sanitising it to `adopt_dash` was the first attempt and was wrong: the
// namespace lexes, but `table "adopt-dash.orders"` still does not, so the
// generated .atl parsed and failed to lower. A declaration that cannot be
// committed is worse than an absent one, because nothing says so until the
// caller runs `tide plan`.
func TestASchemaNameThatCannotBeAddressedIsSkipped(t *testing.T) {
	res := generateAllIn(t, []string{
		`CREATE SCHEMA "adopt-dash"`,
		`CREATE SCHEMA adopt_ok`,
		`CREATE TABLE "adopt-dash".orders (id TEXT PRIMARY KEY)`,
		`CREATE TABLE adopt_ok.invoices (id TEXT PRIMARY KEY)`,
	}, "adopt-dash", "adopt_ok")

	for _, e := range res.Entities {
		if strings.HasPrefix(e.Table, "adopt-dash") {
			t.Errorf("a declaration was generated for %s, which cannot be committed", e.Table)
		}
	}
	var told bool
	for _, s := range res.Skipped {
		if strings.Contains(s, "adopt-dash") {
			told = true
		}
	}
	if !told {
		t.Errorf("the schema was dropped without being named: %v", res.Skipped)
	}
	// The rest of the database is still read. A schema atlantis cannot address
	// must not take the readable ones down with it.
	mustLower(t, atlOf(t, res, "adopt_ok.invoices"))
}

// A table whose own name cannot be addressed is skipped, in a schema that can.
//
// The same defect one level down, and the one that survives every fix aimed at
// schemas: SuggestedName turns `orders-2024` into the perfectly valid entity
// name Orders2024, so the entity-name check passes and the table name is what
// fails to lower.
func TestATableNameThatCannotBeAddressedIsSkipped(t *testing.T) {
	res := generateAllIn(t, []string{
		`CREATE SCHEMA adopt_mixed`,
		`CREATE TABLE adopt_mixed."orders-2024" (id TEXT PRIMARY KEY)`,
		`CREATE TABLE adopt_mixed.invoices (id TEXT PRIMARY KEY)`,
	}, "adopt_mixed")

	for _, e := range res.Entities {
		if strings.Contains(e.Table, "orders-2024") {
			t.Errorf("a declaration was generated for %s, which cannot be committed:\n%s",
				e.Table, e.Atl)
		}
	}
	var told bool
	for _, s := range res.Skipped {
		if strings.Contains(s, "orders-2024") {
			told = true
		}
	}
	if !told {
		t.Errorf("the table was dropped without being named: %v", res.Skipped)
	}
	mustLower(t, atlOf(t, res, "adopt_mixed.invoices"))
}

// namespaceFor is the whole rename rule, so its edges are named here rather
// than being reachable only through a CREATE SCHEMA.
func TestNamespaceFor(t *testing.T) {
	for _, tc := range []struct{ schema, want string }{
		{"public", "public"},
		{"rnacen", "rnacen"},
		{"_private", "_private"},
		{"a1", "a1"},
		// Legal Postgres, and none of these can appear in a `table "..."`
		// value, so none can be adopted under any name.
		{"sales-eu", ""},
		{"sales eu", ""},
		{"2024", ""},
		{"Ünicode", ""},
		{"", ""},
	} {
		got, _ := namespaceFor(tc.schema)
		if got != tc.want {
			t.Errorf("namespaceFor(%q) = %q, want %q", tc.schema, got, tc.want)
		}
		// Whatever comes back has to be usable as both halves of the generated
		// declaration: the `in` clause and the schema part of `table "..."`.
		if got != "" && (!dsl.IsIdentifier(got) || !dsl.IsTableName(got+".t")) {
			t.Errorf("namespaceFor(%q) returned %q, which cannot be committed", tc.schema, got)
		}
	}
}
