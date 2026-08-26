package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The rebuild bracket must never leave the table open.
//
// PostgreSQL refuses to alter a column a policy depends on, so a migration that
// changes the tenant column's type has to take the boundary down, alter, and put
// it back. Dropping the boundary alone does not shut the table: the permissive
// `<table>_default_access USING (true)` grant is still there, and so is whatever
// the operator replaced it with. Row-level security stays ENABLED with nothing
// restricting it, which is every tenant's rows to every caller. With the
// boundary in the restrictive slot, dropping it admits everything rather than
// denying everything.
//
// The end state is correct and every other test passes on it. The exposure is
// reachable only between two statements: inside ApplyMigration's transaction
// the ALTER holds ACCESS EXCLUSIVE and no reader observes either state, so it
// takes a hand-run script or a --no-transaction runner dying mid-way. This
// executes the script in two halves and reads the table at the seam.
//
// A string assertion on statement order would not catch it — the order can be
// right while a policy nobody thought about is still admitting rows. The
// question is what a caller can read, so the test reads.
func TestTheRebuildBracketNeverOpensTheTable(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the rebuild window")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.rbw_doc CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY rbw_probe`, `DROP ROLE IF EXISTS rbw_probe`)
	}
	drop()
	t.Cleanup(drop)

	entity := func(width int) *dsl.IR {
		return &dsl.IR{Entities: []dsl.Entity{{
			Name: "Doc", Namespace: "rbw", PartitionField: "tenant",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "tenant", Type: dsl.FieldType{Name: "varchar", Len: 16}, NotNull: true},
				{Name: "body", Type: dsl.FieldType{Name: "text"}},
			},
		}}}
	}
	before, after := entity(16), entity(32)
	after.Entities[0].Fields[1].Type.Len = 32

	AssignProtoNumbers(nil, before)
	AssignProtoNumbers(before, after)

	initial, err := EmitInitial(before)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v\n%s", err, initial.Up)
	}
	if _, err := admin.Exec(ctx, `
INSERT INTO atlantis.rbw_doc VALUES (1,'acme','acme-secret'), (2,'globex','globex-secret')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A role the policies actually apply to, owning the table so FORCE bites.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE rbw_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO rbw_probe;
GRANT SELECT ON atlantis.rbw_doc TO rbw_probe;`)
		return err
	})
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.rbw_doc OWNER TO rbw_probe`); err != nil {
		t.Fatalf("hand the table over: %v", err)
	}
	probe, err := pg.New(ctx, pg.DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//rbw_probe:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the probe role: %v", err)
	}
	t.Cleanup(probe.Close)

	visible := func(t *testing.T, tenant string) int {
		t.Helper()
		tx, err := probe.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if tenant != "" {
			if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, tenant); err != nil {
				t.Fatalf("bind %s: %v", tenant, err)
			}
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM atlantis.rbw_doc`).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		return n
	}

	scripts, err := EmitSQL(before, after, ComputeDiff(before, after))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}

	// Split at the boundary drop — the exact statement after which the table
	// used to be wide open. Built from the same helpers the emitter uses, so a
	// rename of the policy cannot leave this silently splitting nothing.
	seam := "DROP POLICY IF EXISTS " + quoteIdent(partitionPolicyName(&before.Entities[0])) +
		" ON " + qualifiedTable(&before.Entities[0]) + ";"
	at := strings.Index(scripts.Up, seam)
	if at < 0 {
		t.Fatalf("the emitted script no longer contains the boundary drop %q, so this "+
			"test is not splitting where it thinks it is:\n%s", seam, scripts.Up)
	}
	head, tail := scripts.Up[:at+len(seam)], scripts.Up[at+len(seam):]

	// The whole migration up to and including the moment the boundary is gone.
	if _, err := admin.Exec(ctx, head); err != nil {
		t.Fatalf("apply the first half: %v\n%s", err, head)
	}

	// THE ASSERTION. The boundary is down. Nothing may be readable — not to an
	// unbound caller, and not to a bound one either, because the lock denies
	// everything rather than scoping anything.
	if n := visible(t, ""); n != 0 {
		t.Errorf("an UNBOUND caller reads %d rows midway through the rebuild. The "+
			"boundary is down and `%s_default_access USING (true)` is still "+
			"admitting rows, so this table is serving every tenant to everyone "+
			"until the epilogue runs", n, "rbw_doc")
	}
	if n := visible(t, "acme"); n != 0 {
		t.Errorf("a caller bound to acme reads %d rows midway through the rebuild; "+
			"the table is meant to be closed outright while the boundary is down", n)
	}

	// And the second half must reopen it correctly — a lock that never lifts is
	// a permanent outage, which is the failure this fix could introduce.
	if _, err := admin.Exec(ctx, tail); err != nil {
		t.Fatalf("apply the second half: %v\n%s", err, tail)
	}
	if n := visible(t, "acme"); n != 1 {
		t.Errorf("after the rebuild a caller bound to acme reads %d rows, want 1. "+
			"The rebuild lock was not lifted, or the boundary did not come back", n)
	}
	if n := visible(t, "globex"); n != 1 {
		t.Errorf("after the rebuild a caller bound to globex reads %d rows, want 1", n)
	}
	if n := visible(t, ""); n != 0 {
		t.Errorf("after the rebuild an unbound caller reads %d rows, want 0", n)
	}

	// The column really was altered, or the bracket bracketed nothing.
	var typ string
	if err := admin.QueryRow(ctx, `
SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
 WHERE a.attrelid = 'atlantis.rbw_doc'::regclass AND a.attname = 'tenant'`).Scan(&typ); err != nil {
		t.Fatalf("read the column type: %v", err)
	}
	if !strings.Contains(typ, "32") {
		t.Errorf("the tenant column is %s, so the ALTER this bracket exists to "+
			"permit never ran and the test proved nothing about it", typ)
	}
}
