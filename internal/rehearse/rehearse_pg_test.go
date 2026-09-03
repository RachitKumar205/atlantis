package rehearse

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The clone engine against a live server: schema, rows, sequences, foreign
// keys with a cycle, and a FORCE ROW LEVEL SECURITY table — the shape that
// reads as empty without row_security off.

const rehearseFixtureDDL = `
CREATE SCHEMA IF NOT EXISTS atlantis;
CREATE TABLE atlantis.alpha (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    beta_id bigint,
    v       int NOT NULL
);
CREATE TABLE atlantis.beta (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alpha_id bigint REFERENCES atlantis.alpha (id)
);
ALTER TABLE atlantis.alpha ADD CONSTRAINT alpha_beta_fk FOREIGN KEY (beta_id) REFERENCES atlantis.beta (id);
CREATE TABLE atlantis.tenant_rows (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant text NOT NULL
);
ALTER TABLE atlantis.tenant_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.tenant_rows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON atlantis.tenant_rows AS RESTRICTIVE USING (false) WITH CHECK (false);
`

func rehearseFixture(t *testing.T) (*pgxpool.Pool, *pgx.ConnConfig) {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the clone engine")
	}
	srcDSN := pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_rehearse_src")
	pool, err := pgxpool.New(context.Background(), srcDSN)
	if err != nil {
		t.Fatalf("connect source: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, rehearseFixtureDDL); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	// A cycle in the data too: alpha 1 ↔ beta 1.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.alpha (beta_id, v) VALUES (NULL, 1), (NULL, -5);
INSERT INTO atlantis.beta (alpha_id) VALUES (1);
UPDATE atlantis.alpha SET beta_id = 1 WHERE id = 1;
SET row_security = off;
INSERT INTO atlantis.tenant_rows (tenant) VALUES ('t1'), ('t2'), ('t3');
RESET row_security;`); err != nil {
		t.Fatalf("seed fixture rows: %v", err)
	}

	target, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	return pool, target
}

func ddlSetup(ddl string) func(context.Context, *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, ddl)
		return err
	}
}

func dropClone(t *testing.T, target *pgx.ConnConfig, name string) {
	t.Helper()
	t.Cleanup(func() {
		_ = Drop(context.Background(), target, name)
	})
}

func TestCloneCarriesRowsSequencesAndForeignKeys(t *testing.T) {
	pool, target := rehearseFixture(t)
	ctx := context.Background()
	const cloneName = "atlantis_rehearsal_t_full"
	dropClone(t, target, cloneName)

	if err := Clone(ctx, pool, target, cloneName, ddlSetup(rehearseFixtureDDL),
		Options{CloneTimeout: 2 * time.Minute}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	conn, err := connectTo(ctx, target, cloneName)
	if err != nil {
		t.Fatalf("connect clone: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `SET row_security = off`); err != nil {
		t.Fatal(err)
	}

	for rel, want := range map[string]int64{
		"atlantis.alpha":       2,
		"atlantis.beta":        1,
		"atlantis.tenant_rows": 3,
	} {
		var n int64
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+rel).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", rel, err)
		}
		if n != want {
			t.Errorf("%s carries %d rows, want %d", rel, n, want)
		}
	}

	// The sequence continues past the copied rows rather than colliding.
	var next int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO atlantis.tenant_rows (tenant) VALUES ('t4') RETURNING id`).Scan(&next); err != nil {
		t.Fatalf("insert on clone: %v", err)
	}
	if next != 4 {
		t.Errorf("the clone's identity continued at %d, want 4", next)
	}

	// The foreign keys are back and enforcing.
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.beta (alpha_id) VALUES (999)`); err == nil {
		t.Error("the clone accepted an orphan row; the FK re-add did not happen")
	}
}

func TestExecuteReportsWhatPostgresDid(t *testing.T) {
	pool, target := rehearseFixture(t)
	ctx := context.Background()
	const cloneName = "atlantis_rehearsal_t_exec"
	dropClone(t, target, cloneName)
	if err := Clone(ctx, pool, target, cloneName, ddlSetup(rehearseFixtureDDL),
		Options{CloneTimeout: 2 * time.Minute}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	// A CHECK the seeded data violates (v = -5): the real integrity error.
	out, err := Execute(ctx, target, cloneName,
		`ALTER TABLE atlantis.alpha ADD CONSTRAINT alpha_v_positive CHECK (v > 0)`, time.Minute)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.PgErr == nil || out.PgErr.Code != "23514" {
		t.Fatalf("outcome = %+v, want SQLSTATE 23514", out)
	}

	// The failed attempt rolled back; a clean statement then succeeds, and
	// its own rollback leaves the clone's data untouched.
	out, err = Execute(ctx, target, cloneName,
		`ALTER TABLE atlantis.alpha ADD COLUMN note text`, time.Minute)
	if err != nil || out.Err != nil {
		t.Fatalf("a clean execute failed: %v / %v", err, out.Err)
	}
	n, err := CountWhere(ctx, target, cloneName, "atlantis.alpha", "v <= 0")
	if err != nil {
		t.Fatalf("CountWhere: %v", err)
	}
	if n != 1 {
		t.Errorf("diagnostic count = %d, want 1", n)
	}
}

func TestCloneRefusesPastTheSizeCeiling(t *testing.T) {
	pool, target := rehearseFixture(t)
	const cloneName = "atlantis_rehearsal_t_size"
	dropClone(t, target, cloneName)
	err := Clone(context.Background(), pool, target, cloneName, ddlSetup(rehearseFixtureDDL),
		Options{MaxBytes: 1})
	if err == nil {
		t.Fatal("a clone past the ceiling was built")
	}
	if !errorsIs(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}
	// Refused before CREATE DATABASE: nothing to reap.
	names, err := ListClones(context.Background(), target, cloneName)
	if err != nil {
		t.Fatalf("ListClones: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("a refused clone left %v behind", names)
	}
}

func errorsIs(err, target error) bool { return err != nil && (err == target || contains(err, target)) }

func contains(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
