package console

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The schema-import store, against the real catalogue.
//
// Every statement in schemaimportstore.go ran for the first time here. The
// table it writes holds a customer's table and column names, read from a
// database this console does not run, so the boundary and the expiry are the
// two properties worth proving rather than asserting.
//
// Two organisations throughout, and every scoped read paired with a count taken
// as the superuser: a scoped read returning nothing and a policy admitting
// nothing are the same observation, and only the ground truth separates them.
// See org_isolation_pg_test.go, which established the shape.

// sampleEntities spans two namespaces, because one pass now reads every schema
// and gives each its own. A fixture with one namespace on every row cannot tell
// a stored namespace from a hardcoded one.
func sampleEntities(prefix string) []SchemaImportEntity {
	return []SchemaImportEntity{
		{Table: "public." + prefix + "_orders", Entity: prefix + "Order",
			Namespace: "public", Atl: "entity " + prefix + "Order in public {}"},
		{Table: "sales." + prefix + "_users", Entity: prefix + "User",
			Namespace: "sales", Atl: "entity " + prefix + "User in sales {}"},
	}
}

// countImports reads the table as the superuser, past every policy.
func countImports(t *testing.T, f *consoleFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM console.schema_imports`).Scan(&n); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	return n
}

// An import is written and read back whole.
//
// The control for everything below: without it a boundary test passes against a
// store that never wrote anything.
func TestAnImportRoundTrips(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	org := f.srv.db.forOrg(orgAcme)

	want := sampleEntities("acme")
	id, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
		"db.example.com:5432", want)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(id, "imp_") {
		t.Errorf("id = %q, want an imp_ prefix", id)
	}

	list, err := org.schemaImports(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d imports, want 1", len(list))
	}
	got := list[0]
	if got.ID != id || got.Source != "db.example.com:5432" {
		t.Errorf("summary = %+v, want id %s and source db.example.com:5432", got, id)
	}
	// The count is stored on the row, so a wrong one is a page reporting a
	// number that disagrees with the list underneath it.
	if got.Entities != len(want) {
		t.Errorf("summary reports %d entities, want %d", got.Entities, len(want))
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at came back zero")
	}

	ents, err := org.schemaImportEntities(ctx, id)
	if err != nil {
		t.Fatalf("entities: %v", err)
	}
	if len(ents) != len(want) {
		t.Fatalf("read %d declarations, want %d", len(ents), len(want))
	}
	// Ordered by table, and carrying the .atl exactly as generated: a
	// declaration altered in storage is one that will not parse when committed.
	for i, e := range ents {
		if e != want[i] {
			t.Errorf("declaration %d = %+v, want %+v", i, e, want[i])
		}
	}
}

// One organisation's import is invisible to another.
func TestSchemaImportsDoNotCrossOrganisations(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	f.enrolOrg(t, orgGlobex)

	acmeID, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
		subjectFor(orgAcme, "a@example.com"), "acme.example.com:5432",
		sampleEntities("acme"))
	if err != nil {
		t.Fatalf("create as acme: %v", err)
	}
	if _, err := f.srv.db.forOrg(orgGlobex).createSchemaImport(ctx,
		subjectFor(orgGlobex, "g@example.com"), "globex.example.com:5432",
		sampleEntities("globex")); err != nil {
		t.Fatalf("create as globex: %v", err)
	}

	if total := countImports(t, f); total != 2 {
		t.Fatalf("the table holds %d imports, want 2; the reads below could not "+
			"tell a working boundary from an empty table", total)
	}

	acme, err := f.srv.db.forOrg(orgAcme).schemaImports(ctx)
	if err != nil {
		t.Fatalf("list as acme: %v", err)
	}
	globex, err := f.srv.db.forOrg(orgGlobex).schemaImports(ctx)
	if err != nil {
		t.Fatalf("list as globex: %v", err)
	}
	if len(acme) != 1 || len(globex) != 1 {
		t.Fatalf("acme saw %d and globex %d, want 1 each", len(acme), len(globex))
	}
	if acme[0].Source != "acme.example.com:5432" {
		t.Errorf("acme saw an import of %s", acme[0].Source)
	}
	if globex[0].Source != "globex.example.com:5432" {
		t.Errorf("globex saw an import of %s", globex[0].Source)
	}

	// The claim schemaImportEntities is written on: it puts no organisation in
	// its WHERE clause and relies on the policy for the boundary. An id from
	// elsewhere must select nothing.
	stolen, err := f.srv.db.forOrg(orgGlobex).schemaImportEntities(ctx, acmeID)
	if err != nil {
		t.Fatalf("read acme's import as globex: %v", err)
	}
	if len(stolen) != 0 {
		t.Errorf("globex read %d of acme's declarations with acme's import id", len(stolen))
	}
}

// A row past its date is not listed, whether or not anything has deleted it.
func TestAnExpiredImportIsNotListed(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	org := f.srv.db.forOrg(orgAcme)

	live, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
		"live.example.com:5432", sampleEntities("live"))
	if err != nil {
		t.Fatalf("create the live import: %v", err)
	}
	stale, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
		"stale.example.com:5432", sampleEntities("stale"))
	if err != nil {
		t.Fatalf("create the stale import: %v", err)
	}

	// Aged as the superuser, so the case does not depend on how long the TTL is.
	if _, err := f.pool.Exec(ctx,
		`UPDATE console.schema_imports SET expires_at = now() - interval '1 hour' WHERE id = $1`,
		stale); err != nil {
		t.Fatalf("age the stale import: %v", err)
	}

	list, err := org.schemaImports(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != live {
		ids := make([]string, len(list))
		for i, im := range list {
			ids[i] = im.ID
		}
		t.Fatalf("listed %v, want only %s", ids, live)
	}
	// Still in the table: the exclusion is the query's, not the sweep's.
	if total := countImports(t, f); total != 2 {
		t.Errorf("the table holds %d rows, want 2 — the expired row was deleted "+
			"by something other than the sweep, so this proves nothing about the query", total)
	}
}

// The sweep deletes expired rows, takes their declarations with them, and
// leaves live rows alone.
func TestTheSweepDeletesOnlyExpiredImports(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	org := f.srv.db.forOrg(orgAcme)

	live, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
		"live.example.com:5432", sampleEntities("live"))
	if err != nil {
		t.Fatalf("create the live import: %v", err)
	}
	stale, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
		"stale.example.com:5432", sampleEntities("stale"))
	if err != nil {
		t.Fatalf("create the stale import: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE console.schema_imports SET expires_at = now() - interval '1 hour' WHERE id = $1`,
		stale); err != nil {
		t.Fatalf("age the stale import: %v", err)
	}

	n, err := f.srv.db.deleteExpiredSchemaImports(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep reported %d rows, want 1", n)
	}

	// The declarations are what carry the customer's table names, so the
	// cascade is the half that matters: an import row deleted with its entities
	// left behind keeps the data the expiry exists to drop.
	var orphans int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.schema_import_entities WHERE import_id = $1`,
		stale).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d declarations of the expired import survived it", orphans)
	}

	if got, err := org.schemaImportEntities(ctx, live); err != nil {
		t.Fatalf("read the live import: %v", err)
	} else if len(got) == 0 {
		t.Error("the sweep took the live import's declarations")
	}
}

// A connection string in source is refused by the column.
//
// The one rule the store cannot enforce on its own: the handler passes a host
// built by net.JoinHostPort, and a later caller passing the DSN it already has
// is the mistake that writes a password into a table.
func TestSourceRefusesAConnectionString(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	for _, dsn := range []string{
		"postgres://user:hunter2@db.example.com:5432/app",
		"user@db.example.com",
	} {
		t.Run(dsn, func(t *testing.T) {
			_, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
				subjectFor(orgAcme, "a@example.com"), dsn, nil)
			if err == nil {
				t.Fatalf("stored %q as a source", dsn)
			}
			if total := countImports(t, f); total != 0 {
				t.Errorf("the table holds %d rows after the refusal", total)
			}
		})
	}
}

// An import that cannot store its declarations stores no row either.
//
// entity_name is NOT NULL and the primary key is (import_id, table_name), so
// two declarations naming one table fail the second insert. The import row must
// go with it: a row reporting two entities with none behind it reads as an
// empty database.
func TestAFailedDeclarationRollsBackTheImport(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	dup := []SchemaImportEntity{
		{Table: "public.orders", Entity: "Order", Atl: "entity Order {}"},
		{Table: "public.orders", Entity: "OrderAgain", Atl: "entity OrderAgain {}"},
	}
	if _, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
		subjectFor(orgAcme, "a@example.com"), "db.example.com:5432", dup); err == nil {
		t.Fatal("two declarations of one table were stored")
	}

	if total := countImports(t, f); total != 0 {
		t.Errorf("the table holds %d import rows after the failure, want 0", total)
	}
	var ents int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.schema_import_entities`).Scan(&ents); err != nil {
		t.Fatalf("count declarations: %v", err)
	}
	if ents != 0 {
		t.Errorf("%d declarations survived the failed import", ents)
	}
}

// Imports come back newest first.
func TestImportsAreListedNewestFirst(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	org := f.srv.db.forOrg(orgAcme)

	var ids []string
	for i := range 3 {
		id, err := org.createSchemaImport(ctx, subjectFor(orgAcme, "a@example.com"),
			fmt.Sprintf("db%d.example.com:5432", i), nil)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		// created_at defaults to now(), which inside one transaction is the
		// statement timestamp; separate calls are separate transactions, but
		// the clock can still repeat. Spread them so the order is decidable.
		if _, err := f.pool.Exec(ctx,
			`UPDATE console.schema_imports SET created_at = now() - make_interval(mins => $1) WHERE id = $2`,
			3-i, id); err != nil {
			t.Fatalf("age %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	list, err := org.schemaImports(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d imports, want 3", len(list))
	}
	for i, want := range []string{ids[2], ids[1], ids[0]} {
		if list[i].ID != want {
			t.Errorf("position %d is %s, want %s", i, list[i].ID, want)
		}
	}
}
