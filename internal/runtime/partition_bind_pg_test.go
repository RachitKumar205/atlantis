package runtime_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/runtime"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// A thin adapter so the test can hand a real pgx transaction to BindPartition,
// which takes the runtime's Tx.
type pgxTx struct{ tx pgx.Tx }

func (t pgxTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}
func (t pgxTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}
func (t pgxTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}
func (t pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// The end-to-end property the whole of `partition by` exists for: a request
// bound to one tenant cannot read, write, or reach around to another's rows —
// including through SQL atlantis never generated.
//
//	ATLANTIS_TEST_PG=... go test ./internal/runtime/ -run BindPartition -v
func TestBindPartitionConfinesEverythingInTheTransaction(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise partition binding")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	clean := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.bindpart_doc`)
		pgcatalog.Exec(t, url,
			`DROP OWNED BY bindpart_app`,
			`DROP ROLE IF EXISTS bindpart_app`)
	}
	clean()
	t.Cleanup(clean)

	// Catalog writes first, under the shared lock. CREATE ROLE and
	// GRANT ON SCHEMA update pg_authid and pg_namespace; two packages
	// doing that at once fail with `tuple concurrently updated`.
	pgcatalog.Exec(t, url,
		`CREATE ROLE bindpart_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA atlantis TO bindpart_app`,
	)

	for _, sql := range []string{
		`CREATE TABLE atlantis.bindpart_doc (id int primary key, tenant text not null, body text)`,
		`INSERT INTO atlantis.bindpart_doc VALUES
		   (1,'acme','acme secret'), (2,'acme','acme other'), (3,'globex','globex secret')`,
		`ALTER TABLE atlantis.bindpart_doc ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.bindpart_doc FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY bindpart_pol ON atlantis.bindpart_doc
		   USING (tenant = atlantis.current_partition())
		   WITH CHECK (tenant = atlantis.current_partition())`,
		// Must be a role RLS applies to, or this measures nothing — the pool's
		// own role is a superuser and sees straight through the policy.
		`GRANT SELECT, INSERT, UPDATE, DELETE ON atlantis.bindpart_doc TO bindpart_app`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	// asApp opens a transaction as the restricted role, optionally binding a
	// tenant, and runs fn.
	asApp := func(t *testing.T, tenant string, fn func(t *testing.T, tx runtime.Tx)) {
		t.Helper()
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		raw, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = raw.Rollback(ctx) }()
		if _, err := raw.Exec(ctx, `SET LOCAL ROLE bindpart_app`); err != nil {
			t.Fatalf("set role: %v", err)
		}
		tx := pgxTx{raw}
		if tenant != "" {
			rctx := runtime.WithCallerPartition(ctx, tenant)
			if err := runtime.BindPartition(rctx, tx); err != nil {
				t.Fatalf("BindPartition(%q): %v", tenant, err)
			}
		}
		fn(t, tx)
	}

	count := func(t *testing.T, tx runtime.Tx, where string) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM atlantis.bindpart_doc `+where).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", where, err)
		}
		return n
	}

	t.Run("bound to acme sees only acme", func(t *testing.T) {
		asApp(t, "acme", func(t *testing.T, tx runtime.Tx) {
			if got := count(t, tx, ""); got != 2 {
				t.Errorf("acme saw %d rows, want its own 2", got)
			}
			// Reaching for another tenant explicitly must also find nothing —
			// this is the case a forgotten predicate produces.
			if got := count(t, tx, `WHERE tenant = 'globex'`); got != 0 {
				t.Errorf("acme read %d of globex's rows by naming the tenant "+
					"directly. The policy is not confining the transaction", got)
			}
		})
	})

	t.Run("unbound sees nothing", func(t *testing.T) {
		asApp(t, "", func(t *testing.T, tx runtime.Tx) {
			if got := count(t, tx, ""); got != 0 {
				t.Errorf("an unbound transaction saw %d rows. Absence of a tenant "+
					"must not mean access to every tenant", got)
			}
		})
	})

	// Each write gets its own transaction: a refused statement aborts the
	// transaction it ran in, so sharing one would report "current transaction
	// is aborted" for the later assertions rather than their own result.
	t.Run("cannot insert a row attributed to another tenant", func(t *testing.T) {
		asApp(t, "acme", func(t *testing.T, tx runtime.Tx) {
			if _, err := tx.Exec(ctx,
				`INSERT INTO atlantis.bindpart_doc VALUES (99,'globex','forged')`); err == nil {
				t.Error("acme inserted a row attributed to globex. WITH CHECK is not " +
					"applying, and a write leak is as much a breach as a read leak")
			}
		})
	})

	t.Run("cannot move its own row to another tenant", func(t *testing.T) {
		asApp(t, "acme", func(t *testing.T, tx runtime.Tx) {
			if _, err := tx.Exec(ctx,
				`UPDATE atlantis.bindpart_doc SET tenant='globex' WHERE id=1`); err == nil {
				t.Error("acme moved its own row into globex's tenant, which hands data " +
					"across a boundary without either side seeing it happen")
			}
		})
	})

	t.Run("cannot delete what it cannot see", func(t *testing.T) {
		asApp(t, "acme", func(t *testing.T, tx runtime.Tx) {
			tag, err := tx.Exec(ctx, `DELETE FROM atlantis.bindpart_doc WHERE tenant='globex'`)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("acme deleted %d of globex's rows", tag.RowsAffected())
			}
		})
	})

	t.Run("binding twice in one transaction is refused", func(t *testing.T) {
		asApp(t, "acme", func(t *testing.T, tx runtime.Tx) {
			rctx := runtime.WithCallerPartition(ctx, "globex")
			if err := runtime.BindPartition(rctx, tx); err == nil {
				t.Error("a second bind in the same transaction succeeded, so a request " +
					"could switch tenants midway through its own work")
			}
		})
	})
}

// With no tenant in context, binding must refuse rather than leave the
// transaction unbound. An unbound transaction reads as zero rows for a
// restricted role but returns everything to one that bypasses RLS, so falling
// through is only safe on a deployment that is already correct.
func TestBindPartitionFailsClosedWithNoTenant(t *testing.T) {
	err := runtime.BindPartition(context.Background(), nil)
	if !errors.Is(err, runtime.ErrNoCallerPartition) {
		t.Errorf("BindPartition with no tenant returned %v; want ErrNoCallerPartition. "+
			"Falling through would leave the transaction unbound", err)
	}
}
