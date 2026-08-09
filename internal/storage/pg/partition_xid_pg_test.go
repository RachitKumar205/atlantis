package pg

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reading a partitioned entity must not consume a transaction ID.
//
// atlantis.current_partition() runs inside every row-level security policy, so
// it executes on every read of every partitioned entity. Scoping it with
// txid_current() had a side effect that is invisible until it is catastrophic:
// txid_current() ASSIGNS a transaction ID when the transaction has none, and
// PostgreSQL deliberately withholds IDs from read-only transactions precisely
// because the 32-bit space is consumable. Exhaust it and the database stops
// accepting writes until an anti-wraparound vacuum finishes.
//
// Measured before the fix: 100 reads consumed 100 IDs, against 0 for the same
// reads on a non-RLS table. At ten thousand reads per second that is the whole
// space in about five days.
//
// This is a regression test rather than a benchmark. The correctness argument
// is that set_partition() writes, so a transaction with no ID assigned cannot
// have called it, so NULL is the right answer for exactly those transactions —
// which is why asking instead of demanding costs nothing.
//
//	ATLANTIS_TEST_PG=... go test ./internal/storage/pg/ -run PartitionRead -v
func TestPartitionReadDoesNotConsumeTransactionIDs(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to measure transaction-ID consumption")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	clean := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.xidburn`)
		_, _ = pool.Exec(ctx, `DROP OWNED BY xidburn_reader`)
		_, _ = pool.Exec(ctx, `DROP ROLE IF EXISTS xidburn_reader`)
	}
	clean()
	t.Cleanup(clean)

	for _, sql := range []string{
		`CREATE TABLE atlantis.xidburn (id int primary key, tenant text not null)`,
		`INSERT INTO atlantis.xidburn VALUES (1,'acme'), (2,'globex')`,
		`ALTER TABLE atlantis.xidburn ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.xidburn FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY xidburn_pol ON atlantis.xidburn
		   USING (tenant = atlantis.current_partition())`,
		// The reads MUST run as a role row-level security applies to. Measured
		// as the pool's own role — a superuser — the policy never evaluates,
		// current_partition() is never called, and the measurement is zero
		// whether or not the bug is present. That is not a hypothetical: the
		// first version of this test passed with the defect reintroduced.
		`CREATE ROLE xidburn_reader NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA atlantis TO xidburn_reader`,
		`GRANT SELECT ON atlantis.xidburn TO xidburn_reader`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	// snapshot reads the current ID WITHOUT assigning one, so the measurement
	// does not perturb what it measures.
	snapshot := func() int64 {
		t.Helper()
		var v *int64
		if err := pool.QueryRow(ctx,
			`SELECT pg_snapshot_xmax(pg_current_snapshot())::text::bigint`).Scan(&v); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if v == nil {
			t.Fatal("could not read the transaction-ID horizon")
		}
		return *v
	}

	const reads = 100

	readerConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer readerConn.Release()

	before := snapshot()
	for i := 0; i < reads; i++ {
		// One explicit transaction per read, each as the restricted role. A
		// read-only transaction is assigned no transaction ID unless something
		// inside it demands one — which is the whole property under test.
		tx, err := readerConn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE xidburn_reader`); err != nil {
			t.Fatalf("set role %d: %v", i, err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM atlantis.xidburn`).Scan(&n); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("read %d saw %d rows as a restricted role with no partition set; "+
				"the policy is not being applied, so this measures nothing", i, n)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	burned := snapshot() - before

	// Some slack: anything else touching this database during the run advances
	// the horizon too. The failure being guarded against is one-per-read, so a
	// generous ceiling still catches it by a wide margin.
	if burned > reads/2 {
		t.Errorf("%d reads of a partitioned entity consumed %d transaction IDs. "+
			"Reads must not assign transaction IDs: doing so makes read volume "+
			"drive wraparound pressure one-for-one, and the end state is a "+
			"database that refuses writes. Check that atlantis.current_partition() "+
			"uses txid_current_if_assigned() rather than txid_current()",
			reads, burned)
	}
	t.Logf("%d RLS reads consumed %d transaction IDs", reads, burned)

	// And the mechanism still works: the discriminator is still transaction
	// scoped, so the cheaper form did not buy performance with a leak.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
		t.Fatalf("set_partition: %v", err)
	}
	var got string
	if err := tx.QueryRow(ctx, `SELECT coalesce(atlantis.current_partition(),'')`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "acme" {
		t.Fatalf("current_partition() = %q inside the transaction that set it", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// A later transaction on the SAME backend must not inherit it.
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(atlantis.current_partition(),'')`).Scan(&got); err != nil {
		t.Fatalf("post-commit read: %v", err)
	}
	if got != "" {
		t.Errorf("current_partition() returned %q in a later transaction on the same "+
			"connection. The discriminator must not survive its transaction, or a "+
			"pooled connection carries one tenant's context into another's request", got)
	}
}
