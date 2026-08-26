package codegen

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// replaceUserInfo swaps the credentials in a libpq URL, keeping everything
// else.
//
// By parsing rather than by string substitution. The substitution form —
// strings.Replace(url, "atlantis:atlantis@", ...) — silently returns the URL
// unchanged for any DSN not spelled exactly that way, and the test then
// connects as the ADMIN role while believing it is the tenant. Every isolation
// assertion after that point is running as the role RLS does not apply to. It
// cost me a wrong diagnosis before I noticed.
func replaceUserInfo(dsn, userinfo string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	user, pass, _ := strings.Cut(userinfo, ":")
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// The two properties migration 0024 exists for, executed rather than argued.
//
// The table discriminator it replaced was safe and expensive, and neither of
// these passes against it. A change made for a measurement needs the
// measurement under test.

func discriminatorConn(t *testing.T) (context.Context, *pgx.Conn, string) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the discriminator")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) })

	var haveFn bool
	if err := conn.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'atlantis' AND p.proname = 'current_partition')`).Scan(&haveFn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !haveFn {
		t.Skip("partition migrations not applied to this database")
	}
	return ctx, conn, url
}

// A bind must not consume a transaction ID.
//
// This is the entire reason the discriminator is not a table. set_partition
// used to INSERT, and every write assigns one — measured at 100 ids per 100
// bound reads. At the throughput that same benchmark sustained,
// autovacuum_freeze_max_age arrives in 3.2 hours and the 2^31 wraparound
// refusal in 1.43 days. The failure is not a slow query. It is a database that
// refuses writes.
//
// Asserted per bind rather than as an aggregate: a design that burns an id on
// most binds fails the same way as one that burns it on all of them, and an
// aggregate threshold cannot tell them apart.
func TestPartitionBindConsumesNoTransactionID(t *testing.T) {
	ctx, conn, _ := discriminatorConn(t)

	// Measured per transaction from inside it, NOT as a delta on the cluster's
	// xmax horizon.
	//
	// The horizon moves for every other backend too, so the delta form fails
	// both ways: one concurrent writer running `SELECT txid_current()` fails it
	// 3 times out of 3 on unmutated code, and a mutation burning an id on 45 of
	// 50 binds — wraparound in 1.6 days instead of 1.43 — passes silently. At a
	// full burn it lands on exactly the threshold.
	//
	// txid_current_if_assigned() answers the question directly: it returns this
	// transaction's id if one has been assigned and NULL if not, and no other
	// backend can influence it. Every bind must report NULL, and one that does
	// not is the write coming back.
	const binds = 50
	for i := 0; i < binds; i++ {
		tenant := fmt.Sprintf("tenant-%d", i)

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, tenant); err != nil {
			t.Fatalf("bind: %v", err)
		}

		var assigned *int64
		if err := tx.QueryRow(ctx,
			`SELECT txid_current_if_assigned()`).Scan(&assigned); err != nil {
			t.Fatalf("read own transaction id: %v", err)
		}
		if assigned != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("bind %d was assigned transaction id %d. A bind must not "+
				"write: at one id per bind the 32-bit space reaches the wraparound "+
				"refusal in under two days under load, and the database stops "+
				"accepting writes until an anti-wraparound vacuum completes",
				i, *assigned)
		}

		var got string
		if err := tx.QueryRow(ctx, `SELECT atlantis.current_partition()`).Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != tenant {
			_ = tx.Rollback(ctx)
			t.Fatalf("bound %q but current_partition() returned %q", tenant, got)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// current_partition() must return NULL, never the empty string.
//
// A transaction-local parameter does not revert to NULL when its transaction
// ends. It reverts to the empty string, verified on 17.8, and that is the
// difference between failing closed and not. On the second and every later
// request on a pooled connection, an unbound read compares the column against
// the empty string rather than against NULL. Comparing against the empty
// string can match; comparing against NULL cannot. A row whose discriminator
// is the empty string therefore becomes visible to a caller that bound
// nothing, and a NOT NULL column does not exclude such a row.
//
// The seeded row below is what makes this test able to fail. Without it the
// unbound read returns nothing either way, and the test passes for a reason
// unrelated to the thing it names.
func TestPartitionDiscriminatorNeverReturnsEmptyString(t *testing.T) {
	ctx, conn, url := discriminatorConn(t)

	cleanup := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.emptytenant_doc CASCADE`)
		pgcatalog.Exec(t, url,
			`DROP OWNED BY emptytenant_probe`,
			`DROP ROLE IF EXISTS emptytenant_probe`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := conn.Exec(ctx, `
CREATE TABLE atlantis.emptytenant_doc (id bigint primary key, tenant text not null, body text);
ALTER TABLE atlantis.emptytenant_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.emptytenant_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.emptytenant_doc
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Seeded by a role RLS does not apply to, because no legitimate bind can
	// produce this row: set_partition refuses an empty tenant.
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.emptytenant_doc VALUES (1, '', 'empty-tenant-secret')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pgcatalog.Do(t, url, func(lc *pgx.Conn) error {
		_, err := lc.Exec(ctx, `
CREATE ROLE emptytenant_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS;
GRANT USAGE ON SCHEMA atlantis TO emptytenant_probe;
GRANT SELECT ON atlantis.emptytenant_doc TO emptytenant_probe;`)
		return err
	})

	probeURL := replaceUserInfo(url, "emptytenant_probe:probe")
	probe, err := pgx.Connect(ctx, probeURL)
	if err != nil {
		t.Fatalf("connect as probe: %v", err)
	}
	defer probe.Close(ctx)

	// Bind and commit, so the parameter reverts the way it does in production
	// between two requests on one pooled connection.
	if _, err := probe.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
		t.Fatalf("bind: %v", err)
	}

	var reverted *string
	if err := probe.QueryRow(ctx, `SELECT atlantis.current_partition()`).Scan(&reverted); err != nil {
		t.Fatalf("read discriminator: %v", err)
	}
	if reverted != nil {
		t.Errorf("after its transaction ended the discriminator is %q, not NULL. "+
			"`col = ''` is a comparison that can match; `col = NULL` is not. "+
			"current_partition() must map the empty string back to NULL", *reverted)
	}

	var body *string
	if err := probe.QueryRow(ctx,
		`SELECT body FROM atlantis.emptytenant_doc`).Scan(&body); err != nil && err != pgx.ErrNoRows {
		t.Fatalf("unbound read: %v", err)
	}
	if body != nil {
		t.Errorf("an unbound read returned %q. The row's tenant is the empty "+
			"string and so was the discriminator, so the policy matched. Every "+
			"request after the first on a pooled connection reads this way",
			*body)
	}
}
