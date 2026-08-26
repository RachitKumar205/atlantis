package pg

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Reading a partitioned entity must not consume a transaction ID.
//
// atlantis.current_partition() runs inside every row-level security policy, so
// it executes on every read of every partitioned entity. Scoping it with
// txid_current() had a side effect that is invisible until it is catastrophic:
// txid_current() assigns a transaction ID when the transaction has none, and
// PostgreSQL withholds IDs from read-only transactions because the 32-bit space
// is consumable. Exhausting it stops the database accepting writes until an
// anti-wraparound vacuum finishes.
//
// Measured with txid_current(): 100 reads consumed 100 IDs, against 0 for the
// same reads on a non-RLS table. At ten thousand reads per second that is the
// whole space in about five days.
//
// set_partition() writes, so a transaction with no ID assigned cannot have
// called it and NULL is the answer for exactly those transactions. Asking costs
// nothing that demanding saves.
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
		pgcatalog.Exec(t, testDSN(t),
			`DROP OWNED BY xidburn_reader`,
			`DROP ROLE IF EXISTS xidburn_reader`)
	}
	clean()
	t.Cleanup(clean)

	// Catalog writes first, under the shared lock. CREATE ROLE and
	// GRANT ON SCHEMA update pg_authid and pg_namespace; two packages
	// doing that at once fail with `tuple concurrently updated`.
	pgcatalog.Exec(t, testDSN(t),
		`CREATE ROLE xidburn_reader NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA atlantis TO xidburn_reader`,
	)

	for _, sql := range []string{
		`CREATE TABLE atlantis.xidburn (id int primary key, tenant text not null)`,
		`INSERT INTO atlantis.xidburn VALUES (1,'acme'), (2,'globex')`,
		`ALTER TABLE atlantis.xidburn ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.xidburn FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY xidburn_pol ON atlantis.xidburn
		   USING (tenant = atlantis.current_partition())`,
		// The reads run as a role row-level security applies to. Measured as
		// the pool's own role — a superuser — the policy never evaluates,
		// current_partition() is never called, and the count is zero whether or
		// not the defect is present.
		`GRANT SELECT ON atlantis.xidburn TO xidburn_reader`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	const reads = 100

	readerConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer readerConn.Release()

	// Asked of each transaction directly rather than measured against the
	// cluster's transaction-ID horizon.
	//
	// The horizon is cluster-wide. Reading it before and after the loop with
	// reads/2 of slack for anything else touching the database holds until
	// `go test` runs enough packages in parallel to burn 90 IDs during the run,
	// and then reports a defect that is not there.
	//
	// txid_current_if_assigned() returns NULL when the calling transaction has
	// been assigned no ID, which is precisely the property: it is local, exact,
	// and unaffected by every other backend on the instance.
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
		var assigned *int64
		if err := tx.QueryRow(ctx, `SELECT txid_current_if_assigned()`).Scan(&assigned); err != nil {
			t.Fatalf("read assigned xid %d: %v", i, err)
		}
		if assigned != nil {
			t.Fatalf("read %d of a partitioned entity was assigned transaction ID %d. "+
				"Reads must not assign transaction IDs: doing so makes read volume "+
				"drive wraparound pressure one-for-one, and the end state is a "+
				"database that refuses writes. Check that atlantis.current_partition() "+
				"uses txid_current_if_assigned() rather than txid_current()", i, *assigned)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	t.Logf("%d RLS reads, none assigned a transaction ID", reads)

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
