package embedded_test

// Real-Postgres tests. These spin up a fresh embedded-postgres
// instance per test, so they're slow (~4–8 s startup) compared to
// the rest of the suite. They're not gated behind a build tag —
// they run by default — but each test is structured so failures are
// loud and obvious.
//
// Where embedded-postgres cannot start — no network for the first-run binary
// download, a firewall on loopback — every test here fails under a "start:"
// prefix rather than skipping. The published fidelity matrix says embedded
// works, so an environment that cannot run it is a CI failure.

import (
	"context"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/embedded"
)

// userIR shapes one entity that drives the embedded backend's DDL apply path.
//
// A plain bigint PK rather than Identity, so the test supplies explicit IDs: an
// IDENTITY column rejects a non-DEFAULT insert without OVERRIDING SYSTEM VALUE,
// which codegen-emitted handlers write and this does not.
func userIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "User",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "active", Type: dsl.FieldType{Name: "boolean"}, NotNull: true},
			},
		}},
	}
}

func TestEmbeddedBoots(t *testing.T) {
	// Bench the cold start so we have a published number per the
	// performance budget table.
	start := time.Now()
	be, err := embedded.New(context.Background(), userIR(), embedded.Options{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	t.Logf("embedded cold start: %v on port %d", time.Since(start), be.Port())
}

func TestEmbeddedCRUDRoundTrip(t *testing.T) {
	be, err := embedded.New(context.Background(), userIR(), embedded.Options{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	pool := be.Pool()
	ctx := context.Background()

	// The DDL emitter we reuse is the same one production migrations
	// use, so the table name is "atlantis.consumer_user" — same name
	// every codegen-emitted handler uses.
	const ins = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	var id int64
	if err := pool.QueryRow(ctx, ins, int64(1), "x@y.com", true).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 1 {
		t.Fatalf("RETURNING: got %d want 1", id)
	}

	const sel = `SELECT "email", "active" FROM "atlantis"."consumer_user" WHERE "id" = $1`
	var email string
	var active bool
	if err := pool.QueryRow(ctx, sel, int64(1)).Scan(&email, &active); err != nil {
		t.Fatalf("get: %v", err)
	}
	if email != "x@y.com" || !active {
		t.Fatalf("get: email=%q active=%v", email, active)
	}

	const upd = `UPDATE "atlantis"."consumer_user" SET "email" = $1 WHERE "id" = $2`
	tag, err := pool.Exec(ctx, upd, "new@y.com", int64(1))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("update RowsAffected: %d", tag.RowsAffected())
	}
}

// TestEmbeddedUserAuthoredSQL exercises the path the auto-router
// targets: SQL the sim's whitelist can't parse but real PG executes
// without complaint. Recursive CTE is a representative example.
func TestEmbeddedUserAuthoredSQL(t *testing.T) {
	be, err := embedded.New(context.Background(), userIR(), embedded.Options{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	pool := be.Pool()
	ctx := context.Background()
	const recursive = `
		WITH RECURSIVE seq(n) AS (
			SELECT 1
			UNION ALL
			SELECT n + 1 FROM seq WHERE n < 5
		)
		SELECT count(*)::bigint FROM seq`
	var n int64
	if err := pool.QueryRow(ctx, recursive).Scan(&n); err != nil {
		t.Fatalf("recursive CTE: %v", err)
	}
	if n != 5 {
		t.Fatalf("recursive CTE: got %d want 5", n)
	}
}

func TestEmbeddedClose(t *testing.T) {
	be, err := embedded.New(context.Background(), userIR(), embedded.Options{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := be.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Double-close is idempotent.
	if err := be.Close(); err != nil {
		t.Fatalf("double-close: %v", err)
	}
}
