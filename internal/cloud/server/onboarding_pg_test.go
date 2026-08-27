package server

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/adopt"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The whole path, minus the guard.
//
// dsnguard refuses every address a test database is reachable at — that is what
// it is for — so these open the pool directly and exercise what happens once
// connected. The refusals themselves are covered in internal/cloud/dsnguard,
// where they can be tested without a database at all.

func importTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise schema import against a real database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A read of a real database produces declarations that lower, which is what
// `tide plan` would do with them.
func TestIntrospectReadsALiveDatabase(t *testing.T) {
	pool := importTestPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS impfix CASCADE`,
		`CREATE SCHEMA impfix`,
		`CREATE TABLE impfix.account (
			id        bigint PRIMARY KEY,
			tenant_id text NOT NULL,
			email     text NOT NULL UNIQUE
		)`,
		`CREATE INDEX account_tenant ON impfix.account (tenant_id)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS impfix CASCADE`) })

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	res, err := adopt.Generate(ctx, tx, "shop", []string{"impfix"}, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Entities) != 1 {
		t.Fatalf("entities = %d, want 1 (skipped %v)", len(res.Entities), res.Skipped)
	}

	src := res.Entities[0].Atl
	f, err := dsl.Parse("generated.atl", []byte(src))
	if err != nil {
		t.Fatalf("generated .atl does not parse: %v\n\n%s", err, src)
	}
	if _, err := dsl.Lower([]*dsl.File{f}); err != nil {
		t.Fatalf("generated .atl does not lower: %v\n\n%s", err, src)
	}

	// The reward, on a table that has the problem.
	var found bool
	for _, s := range res.Suggestions {
		if s.Kind == adopt.SuggestTenantIsolation && s.Line == "partition by tenant_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("tenant isolation was not suggested for a table with tenant_id and no policy: %+v", res.Suggestions)
	}
}

// A read-only transaction refuses a write even where the role could perform
// one, so the guarantee does not rest on the customer having supplied a
// read-only role.
func TestIntrospectTransactionRefusesWrites(t *testing.T) {
	pool := importTestPool(t)
	ctx := context.Background()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	_, err = tx.Exec(ctx, `CREATE TABLE impwrite_should_not_exist (id int)`)
	if err == nil {
		t.Fatal("a write succeeded inside the read-only transaction")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Errorf("the write failed for some other reason: %v", err)
	}
}

// Storing an import keeps the host and never the credential, and the account
// owning it is what reads it back.
func TestSchemaImportStoresNoCredential(t *testing.T) {
	pool := importTestPool(t)
	ctx := context.Background()

	db, err := store.New(ctx, os.Getenv("ATLANTIS_TEST_PG"), slog.Default())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	const userID = "usr_importtest"
	if _, err := pool.Exec(ctx, `
		INSERT INTO cloud.users (id, email) VALUES ($1, 'importtest@example.test')
		ON CONFLICT (id) DO NOTHING`, userID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM cloud.users WHERE id = $1`, userID)
	})

	id, err := db.CreateSchemaImport(ctx, userID, "db.example.com:5432", "shop",
		[]store.SchemaImportEntity{{Table: "public.account", Entity: "Account", Atl: "entity Account in shop {}"}})
	if err != nil {
		t.Fatalf("CreateSchemaImport: %v", err)
	}

	got, err := db.SchemaImportEntities(ctx, userID, id)
	if err != nil {
		t.Fatalf("SchemaImportEntities: %v", err)
	}
	if len(got) != 1 || got[0].Table != "public.account" {
		t.Fatalf("read back %+v", got)
	}

	// An identifier is not an authorisation.
	if _, err := db.SchemaImportEntities(ctx, "usr_someoneelse", id); err == nil {
		t.Error("another account read this import by knowing its id")
	}

	// The column refuses a connection string outright, so a handler that
	// passed one through would fail the insert rather than store it.
	if _, err := db.CreateSchemaImport(ctx, userID, "postgres://u:pw@db.example.com/x", "shop", nil); err == nil {
		t.Error("a connection string was accepted as the source")
	}
}
