package adopt

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The generated .atl has to parse. Everything else this package does is
// worthless if the file it hands back cannot be committed, and nothing between
// the catalogue and the emitter type-checks that: the pipeline builds strings.

func adoptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise adoption against a real database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// generateFrom runs the pipeline against a private schema in a READ ONLY
// transaction, the way both callers do.
func generateFrom(t *testing.T, ddl []string, schema string) Result {
	t.Helper()
	pool := adoptPool(t)
	ctx := context.Background()

	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	res, err := Generate(ctx, tx, "shop", []string{schema}, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return res
}

// atlOf returns the declaration generated for one table.
func atlOf(t *testing.T, res Result, table string) string {
	t.Helper()
	for _, e := range res.Entities {
		if e.Table == table {
			return e.Atl
		}
	}
	t.Fatalf("no declaration for %s; got %d entities, skipped %v", table, len(res.Entities), res.Skipped)
	return ""
}

// mustLower parses and lowers a generated declaration.
//
// Lower is what `tide plan` runs, so a file failing here fails there. Parse
// alone is not enough: it accepts a type name or a reference that Lower then
// rejects, and both are things introspection can emit.
func mustLower(t *testing.T, src string) {
	t.Helper()
	f, err := dsl.Parse("generated.atl", []byte(src))
	if err != nil {
		t.Fatalf("generated .atl does not parse: %v\n\n%s", err, src)
	}
	if _, err := dsl.Lower([]*dsl.File{f}); err != nil {
		t.Fatalf("generated .atl does not lower: %v\n\n%s", err, src)
	}
}

func TestGeneratedAtlParses(t *testing.T) {
	res := generateFrom(t, []string{
		`DROP SCHEMA IF EXISTS adoptfix CASCADE`,
		`CREATE SCHEMA adoptfix`,
		`CREATE TABLE adoptfix.customer (
			id      bigint PRIMARY KEY,
			email   text NOT NULL UNIQUE,
			country text NOT NULL,
			region  text NOT NULL,
			total   numeric NOT NULL CONSTRAINT total_positive CHECK (total > 0),
			CONSTRAINT customer_place UNIQUE (country, region)
		)`,
	}, "adoptfix")

	src := atlOf(t, res, "adoptfix.customer")

	// The whole point: what comes back is committable. Lower is what `tide
	// plan` runs, so a file that fails here fails there.
	mustLower(t, src)
}

// A secondary index has no constraint behind it, so nothing else in the
// pipeline reads one. Committed without them, the first `tide plan` proposes
// creating an index the database already has.
//
// The kinds .atl cannot spell are named instead of dropped, for the same
// reason: an index that appears nowhere is one nobody knows to keep.
func TestGeneratedAtlCarriesSecondaryIndexes(t *testing.T) {
	res := generateFrom(t, []string{
		`DROP SCHEMA IF EXISTS adoptidx CASCADE`,
		`CREATE SCHEMA adoptidx`,
		`CREATE TABLE adoptidx.event (
			id         bigint PRIMARY KEY,
			actor      text NOT NULL,
			kind       text NOT NULL,
			created_at timestamptz NOT NULL,
			deleted_at timestamptz
		)`,
		`CREATE INDEX event_actor ON adoptidx.event (actor)`,
		`CREATE INDEX event_recent ON adoptidx.event (kind, created_at DESC)`,
		`CREATE INDEX event_lower_actor ON adoptidx.event (lower(actor))`,
		`CREATE INDEX event_live ON adoptidx.event (kind) WHERE deleted_at IS NULL`,
	}, "adoptidx")

	src := atlOf(t, res, "adoptidx.event")

	if !strings.Contains(src, "index by actor") {
		t.Errorf("the plain index is missing:\n%s", src)
	}
	// DESC is part of the index's identity. Emitting `index by kind,
	// created_at` for an index on (kind, created_at DESC) declares a different
	// index, which reads as drift forever.
	if !strings.Contains(src, "index by kind, created_at desc") {
		t.Errorf("the descending index lost its sort order:\n%s", src)
	}

	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"event_lower_actor", "event_live"} {
		if !strings.Contains(joined, want) {
			t.Errorf("index %s is neither declared nor mentioned in the warnings:\n%s", want, joined)
		}
	}

	mustLower(t, src)
}

// A CHECK and a composite UNIQUE exist only in the catalogue: FromPostgres
// carries both from the declaration, and a generating pass supplies a stub that
// has neither. Enrich is what reads them.
//
// Committed without these, `tide plan` proposes adding a constraint the
// database already has.
func TestGeneratedAtlCarriesChecksAndCompositeUniques(t *testing.T) {
	res := generateFrom(t, []string{
		`DROP SCHEMA IF EXISTS adoptenrich CASCADE`,
		`CREATE SCHEMA adoptenrich`,
		`CREATE TABLE adoptenrich.invoice (
			id      bigint PRIMARY KEY,
			ref     text NOT NULL UNIQUE,
			year    int NOT NULL,
			seq     int NOT NULL,
			total   numeric NOT NULL CONSTRAINT total_positive CHECK (total > 0),
			CONSTRAINT invoice_number UNIQUE (year, seq)
		)`,
	}, "adoptenrich")

	src := atlOf(t, res, "adoptenrich.invoice")

	// Matched on the emitted clause, not on the column names. `year`, `seq` and
	// `unique` all appear in this declaration anyway — two columns and one
	// field modifier — so a substring test on those passes whether or not the
	// composite constraint was read at all.
	// Postgres's own deparse, not the author's `total > 0`: it renders that as
	// `(total > (0)::numeric)`, and carrying the canonical form is what makes
	// the predicate compare equal on the next introspection instead of reading
	// as drift on every plan.
	if !strings.Contains(src, `check "(total > (0)::numeric)" as total_positive`) {
		t.Errorf("the CHECK is missing from the declaration:\n%s", src)
	}
	if !strings.Contains(src, "unique by year, seq") {
		t.Errorf("the composite UNIQUE is missing from the declaration:\n%s", src)
	}
	mustLower(t, src)
}
