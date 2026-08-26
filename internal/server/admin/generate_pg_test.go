package admin

import (
	"context"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// GenerateSchema is the onboarding path: point atlantis at a database and get
// declarations back. Everything it produces is something a customer will
// commit, so the properties are "does it parse" and "does it describe the
// database", not "does it look right".

func createLegacySchema(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS legacy CASCADE`,
		`CREATE SCHEMA legacy`,
		`CREATE TABLE legacy.customer (
			id     bigint PRIMARY KEY,
			email  varchar(255) NOT NULL UNIQUE,
			joined timestamptz NOT NULL DEFAULT now()
		)`,
		// A foreign key between two discovered tables. Introspecting the two
		// separately drops this reference.
		`CREATE TABLE legacy.order_line (
			id          bigint PRIMARY KEY,
			customer_id bigint NOT NULL REFERENCES legacy.customer(id) ON DELETE CASCADE,
			qty         int NOT NULL DEFAULT 1
		)`,
	} {
		if _, err := svc.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = svc.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS legacy CASCADE`)
	})
}

func TestGenerateSchemaProducesUsableDeclarations(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createLegacySchema(t, svc)

	resp, err := svc.GenerateSchema(ctx, &adminpb.GenerateSchemaRequest{
		Namespace: "shop",
		Schemas:   []string{"legacy"},
	})
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}
	if n := len(resp.GetEntities()); n != 2 {
		t.Fatalf("generated %d entities, want 2 (customer, order_line); skipped=%v",
			n, resp.GetSkipped())
	}

	byTable := map[string]*adminpb.GeneratedEntity{}
	var all []*dsl.File
	for _, e := range resp.GetEntities() {
		byTable[e.GetTable()] = e

		// Every generated file must parse. A customer's first act is to commit
		// these and run `tide plan`; a file that does not parse fails there,
		// naming a generated file rather than the table it came from.
		f, err := dsl.Parse(e.GetEntityName()+".atl", []byte(e.GetAtl()))
		if err != nil {
			t.Fatalf("generated .atl for %s does not parse: %v\n%s", e.GetTable(), err, e.GetAtl())
		}
		all = append(all, f)
	}

	// Together they must lower — which is where a foreign key between two
	// generated entities either resolves or does not.
	ir, err := dsl.Lower(all)
	if err != nil {
		t.Fatalf("generated .atl does not lower as a set: %v", err)
	}
	if len(ir.Entities) != 2 {
		t.Fatalf("lowered %d entities, want 2", len(ir.Entities))
	}

	t.Run("the table override points at the real table", func(t *testing.T) {
		e := byTable["legacy.customer"]
		if e == nil {
			t.Fatal("legacy.customer was not generated")
		}
		if !strings.Contains(e.GetAtl(), `table "legacy.customer"`) {
			t.Errorf("no table override; without it the entity resolves to "+
				"atlantis.shop_customer, which does not exist\n%s", e.GetAtl())
		}
	})

	t.Run("the cross-table foreign key survives", func(t *testing.T) {
		// This is why the tables are introspected together rather than one at
		// a time: a reference can only resolve to an entity in the same IR.
		var found bool
		for _, e := range ir.Entities {
			for _, f := range e.Fields {
				if f.Ref != nil && f.Name == "customer_id" {
					found = true
					if f.Ref.TargetID != "shop.Customer" {
						t.Errorf("customer_id references %q, want shop.Customer", f.Ref.TargetID)
					}
				}
			}
		}
		if !found {
			t.Error("the foreign key from order_line to customer was lost — " +
				"introspecting the discovered tables separately would do this")
		}
	})

	t.Run("the header says what was not checked", func(t *testing.T) {
		// Risk #1 from the adopt rework: a generated file understates the
		// schema, and a customer who believes it complete will later add an
		// index the database already has.
		atl := byTable["legacy.customer"].GetAtl()
		for _, want := range []string{"Indexes", "not present", "Review before committing"} {
			if !strings.Contains(atl, want) {
				t.Errorf("generated header does not mention %q:\n%s", want, atl)
			}
		}
	})
}

// TestGenerateSchemaSkipsWhatIsAlreadyDeclared stops onboarding proposing a
// duplicate of something the customer already has.
func TestGenerateSchemaSkipsWhatIsAlreadyDeclared(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createLegacySchema(t, svc)

	const declared = `
entity Customer in shop {
  table "legacy.customer"
  id     bigint primary
  email  varchar(255) not null unique
  joined timestamptz not null default now()
}
`
	resp, err := svc.GenerateSchema(ctx, &adminpb.GenerateSchemaRequest{
		Namespace: "shop",
		Schemas:   []string{"legacy"},
		Submissions: []*adminpb.CallerSubmission{
			{Caller: "shop", Files: depScopeFiles("customer.atl", declared)},
		},
	})
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}
	for _, e := range resp.GetEntities() {
		if e.GetTable() == "legacy.customer" {
			t.Error("generated a declaration for legacy.customer, which is already " +
				"declared — the customer would end up with two entities on one table")
		}
	}
	if n := len(resp.GetEntities()); n != 1 {
		t.Errorf("generated %d entities, want 1 (order_line only)", n)
	}
}

// TestGenerateSchemaWillNotReuseADeclaredEntityName covers the collision the
// skip path exists for, and the half of it that was missing.
//
// DiscoverTables excludes declared TABLES, so a declaration pointing at
// legacy.customer hides that table. It does not hide the NAME: a differently
// named table whose SuggestedName collides with a declared entity was offered
// anyway, with skipped empty. Committing that file makes dsl.Lower fail on a
// duplicate entity ID, which breaks every later plan, apply and inspect for
// the caller — and the customer's only clue is an error about their own file.
func TestGenerateSchemaWillNotReuseADeclaredEntityName(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS legdup CASCADE`,
		`CREATE SCHEMA legdup`,
		// Declared below under a DIFFERENT physical table, so discovery does
		// not exclude this one — only the name collides.
		`CREATE TABLE legdup.customer (id bigint PRIMARY KEY, note text)`,
		`CREATE TABLE legdup.other (id bigint PRIMARY KEY)`,
	} {
		if _, err := svc.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = svc.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS legdup CASCADE`)
	})

	// Declares shop.Customer against a table other than legdup.customer.
	const declared = `
entity Customer in shop {
  table "legdup.other"
  id bigint primary
}
`
	resp, err := svc.GenerateSchema(ctx, &adminpb.GenerateSchemaRequest{
		Namespace: "shop",
		Schemas:   []string{"legdup"},
		Submissions: []*adminpb.CallerSubmission{
			{Caller: "shop", Files: depScopeFiles("customer.atl", declared)},
		},
	})
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}

	for _, e := range resp.GetEntities() {
		if e.GetEntityName() == "Customer" {
			t.Errorf("generated a second entity named Customer (table %s) while "+
				"shop.Customer is already declared — committing both makes "+
				"dsl.Lower fail on a duplicate entity ID", e.GetTable())
		}
	}

	// Dropped is only half of it. A table that vanishes with no explanation
	// reads as a table atlantis did not find.
	var named bool
	for _, s := range resp.GetSkipped() {
		if strings.Contains(s, "legdup.customer") && strings.Contains(s, "already declared") {
			named = true
		}
	}
	if !named {
		t.Errorf("legdup.customer was skipped without being reported as a name "+
			"collision with an existing declaration; skipped = %v", resp.GetSkipped())
	}
}

// TestGenerateSchemaRefusesNamesThatAreNotIdentifiers stops the server
// proposing an entity whose .atl could never parse.
//
// SuggestedName treats only `_`, `-` and ` ` as separators and copies every
// other rune through, so `a/b` yields "A/b" and `2024_events` yields
// "2024Events". Neither lexes as an identifier. The CLI does check before
// writing a file, but it is one consumer of this RPC and the console is
// another — a server that returns an entity it knows cannot parse is
// proposing work that cannot succeed, and the CLI's check then reads as the
// only thing standing between the customer and a broken file.
func TestGenerateSchemaRefusesNamesThatAreNotIdentifiers(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS legbad CASCADE`,
		`CREATE SCHEMA legbad`,
		// Both are legal Postgres identifiers inside quotes.
		`CREATE TABLE legbad."a/b" (id bigint PRIMARY KEY)`,
		`CREATE TABLE legbad."2024_events" (id bigint PRIMARY KEY)`,
		// A control, so a failure here means the refusal is too broad rather
		// than the fixture being empty.
		`CREATE TABLE legbad.good_rows (id bigint PRIMARY KEY)`,
	} {
		if _, err := svc.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = svc.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS legbad CASCADE`)
	})

	resp, err := svc.GenerateSchema(ctx, &adminpb.GenerateSchemaRequest{
		Namespace: "shop",
		Schemas:   []string{"legbad"},
	})
	if err != nil {
		t.Fatalf("GenerateSchema: %v", err)
	}

	for _, e := range resp.GetEntities() {
		if !dsl.IsIdentifier(e.GetEntityName()) {
			t.Errorf("generated entity name %q for table %s is not a usable .atl "+
				"identifier — the file written for it will not parse",
				e.GetEntityName(), e.GetTable())
		}
	}

	// Dropped is half the job; naming them is the other half. A table that
	// vanishes with no explanation reads as a table atlantis did not find.
	var namedBad, namedDigit bool
	for _, s := range resp.GetSkipped() {
		if strings.Contains(s, "a/b") {
			namedBad = true
		}
		if strings.Contains(s, "2024") {
			namedDigit = true
		}
	}
	if !namedBad || !namedDigit {
		t.Errorf("unusable table names were dropped without being reported; "+
			"skipped = %v", resp.GetSkipped())
	}

	// The control must still come through, or the refusal is rejecting
	// ordinary tables.
	var sawGood bool
	for _, e := range resp.GetEntities() {
		if e.GetTable() == "legbad.good_rows" {
			sawGood = true
		}
	}
	if !sawGood {
		t.Error("legbad.good_rows was not generated — the identifier check is " +
			"refusing names it should accept")
	}
}

// TestGenerateSchemaRequiresANamespace pins that the server does not invent
// one. The namespace becomes part of every entity ID and therefore of the
// caller's generated API.
func TestGenerateSchemaRequiresANamespace(t *testing.T) {
	svc := depScopeService(t)
	if _, err := svc.GenerateSchema(context.Background(),
		&adminpb.GenerateSchemaRequest{Schemas: []string{"legacy"}}); err == nil {
		t.Error("generated with no namespace; the server picked a name that becomes " +
			"the customer's public API")
	}
}
