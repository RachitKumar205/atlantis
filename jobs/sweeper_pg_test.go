package jobs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The sweeper must actually delete expired rows, and leave live ones alone.
//
// Its DELETE carried `LIMIT`, which is MySQL syntax that Postgres rejects, so
// every sweep failed on its first entity — and the failure was swallowed by a
// Warn and a continue, so it looked identical to a sweep with nothing to do.
// Nothing caught it because nothing ever ran the sweeper: it was registered by
// no call site and scheduled by no one, while docs/guides/row-ttl.md shipped a
// "Verify" step promising the deletion happens within a minute.
func TestSweeperDeletesExpiredRowsAndKeepsLiveOnes(t *testing.T) {
	pool, ctx := schedTestPool(t)

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.swp_session`)
	}
	reset()
	t.Cleanup(reset)

	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	mustExec(`CREATE TABLE atlantis.swp_session (
	            id bigint PRIMARY KEY, expires_at timestamptz NOT NULL)`)
	mustExec(`INSERT INTO atlantis.swp_session VALUES
	            (1, now() - INTERVAL '1 hour'),
	            (2, now() - INTERVAL '1 day'),
	            (3, now() + INTERVAL '1 hour')`)

	// The sweeper reads the IR checkpoint to discover ttl_field entities, so
	// the checkpoint has to describe this table.
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Session", Namespace: "swp", TtlField: "expires_at",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
		},
	}}}
	raw, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("marshal ir: %v", err)
	}
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-test')
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir`, raw); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	t.Cleanup(func() {
		if saved != nil {
			_, _ = pool.Exec(ctx, `UPDATE atlantis.ir_checkpoint SET ir = $1 WHERE id = 1`, saved)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM atlantis.ir_checkpoint WHERE id = 1`)
		}
	})

	h := &SweepExpiredHandler{Pool: pool, Logger: quietLogger(), BatchLimit: 100}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("sweep returned an error: %v", err)
	}

	var live, expired int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE expires_at > now()),
       count(*) FILTER (WHERE expires_at <= now())
  FROM atlantis.swp_session`).Scan(&live, &expired); err != nil {
		t.Fatalf("count: %v", err)
	}
	if expired != 0 {
		t.Errorf("%d expired rows survived the sweep. ttl_field is documented as "+
			"deleting them and has never deleted one", expired)
	}
	if live != 1 {
		t.Errorf("%d unexpired rows remain, want 1. The sweeper is deleting rows "+
			"whose TTL has not passed", live)
	}
}

// A sweep that cannot run must report the failure, not swallow it.
func TestSweeperSurfacesFailure(t *testing.T) {
	pool, ctx := schedTestPool(t)

	// An entity whose table does not exist stands in for any statement-level
	// failure. The old handler logged a Warn and returned nil, so the job
	// runtime recorded success and no retry or dead-letter ever happened.
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Missing", Namespace: "swp", TtlField: "expires_at",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}},
	}}}
	raw, _ := json.Marshal(ir)
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-test')
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir`, raw); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	t.Cleanup(func() {
		if saved != nil {
			_, _ = pool.Exec(ctx, `UPDATE atlantis.ir_checkpoint SET ir = $1 WHERE id = 1`, saved)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM atlantis.ir_checkpoint WHERE id = 1`)
		}
	})

	h := &SweepExpiredHandler{Pool: pool, Logger: quietLogger(), BatchLimit: 10}
	err := h.Handle(ctx, nil)
	if err == nil {
		t.Fatal("a sweep that could not delete anything reported success. The job " +
			"runtime marks it complete, no retry happens, nothing is dead-lettered, " +
			"and a sweeper broken for months looks exactly like one with no work")
	}
	// And it must COUNT correctly. One entity exists and it failed, so the only
	// truthful denominator is 1. The denominator used to be len(failures)+1,
	// which is a count of nothing: this case reported "1 of 2 entities failed",
	// telling an operator that something swept when nothing had.
	if !strings.Contains(err.Error(), "1 of 1 entities failed") {
		t.Errorf("the failure says %q. One entity was swept and it failed, so the "+
			"denominator must be 1 — anything larger claims a success that did "+
			"not happen", err)
	}
}

// The denominator counts what was attempted, across a mix.
//
// The single-entity case above pins it at 1 of 1, and that alone catches
// len(failures)+1. This adds the case where the two disagree by more than one,
// so a fix that swapped one wrong expression for another — say len(ir.Entities)
// — does not pass: two of these three entities sweep fine, and one of them has
// no ttl_field at all and is never a candidate.
func TestSweeperCountsEntitiesItActuallyAttempted(t *testing.T) {
	pool, ctx := schedTestPool(t)

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.swpn_alpha, atlantis.swpn_beta`)
	}
	reset()
	t.Cleanup(reset)

	for _, sql := range []string{
		`CREATE TABLE atlantis.swpn_alpha (id bigint PRIMARY KEY, expires_at timestamptz NOT NULL)`,
		`CREATE TABLE atlantis.swpn_beta  (id bigint PRIMARY KEY, expires_at timestamptz NOT NULL)`,
		`INSERT INTO atlantis.swpn_alpha VALUES (1, now() - INTERVAL '1 hour')`,
		`INSERT INTO atlantis.swpn_beta  VALUES (1, now() - INTERVAL '1 hour')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	ttl := func(name string) dsl.Entity {
		return dsl.Entity{
			Name: name, Namespace: "swpn", TtlField: "expires_at",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			},
		}
	}
	ir := &dsl.IR{Entities: []dsl.Entity{
		ttl("Alpha"),
		ttl("Beta"),
		ttl("Gone"), // no table — the one failure
		// No ttl_field, so never a sweep candidate and never part of the
		// denominator. Its presence is what makes len(ir.Entities) wrong.
		{Name: "Bystander", Namespace: "swpn",
			Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}},
	}}
	raw, merr := json.Marshal(ir)
	if merr != nil {
		t.Fatalf("marshal ir: %v", merr)
	}
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-test')
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir`, raw); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	t.Cleanup(func() {
		if saved != nil {
			_, _ = pool.Exec(ctx, `UPDATE atlantis.ir_checkpoint SET ir = $1 WHERE id = 1`, saved)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM atlantis.ir_checkpoint WHERE id = 1`)
		}
	})

	h := &SweepExpiredHandler{Pool: pool, Logger: quietLogger(), BatchLimit: 100}
	err := h.Handle(ctx, nil)
	if err == nil {
		t.Fatal("a sweep with a missing table reported success")
	}
	if !strings.Contains(err.Error(), "1 of 3 entities failed") {
		t.Errorf("the failure says %q, want \"1 of 3 entities failed\".\n"+
			"  \"1 of 2\" → the denominator is still len(failures)+1\n"+
			"  \"1 of 4\" → it counts every entity, including one with no ttl_field "+
			"that was never a sweep candidate", err)
	}

	// The two healthy entities must still have swept. A denominator is only
	// meaningful if the work it counts happened.
	var remaining int
	if qerr := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM atlantis.swpn_alpha) + (SELECT count(*) FROM atlantis.swpn_beta)`).Scan(&remaining); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if remaining != 0 {
		t.Errorf("%d expired rows survived on the healthy entities; one broken "+
			"entity stopped the others being swept", remaining)
	}
}

// A sweep that row-level security would silence must SAY SO, and must keep
// sweeping everything else.
//
// # The bug
//
// The sweeper owns the tables it sweeps and binds no tenant. Under FORCE ROW
// LEVEL SECURITY the tenant policy applies to the owner too, so
// atlantis.current_partition() is NULL, `tenant = NULL` is NULL, and the
// subquery selects no ctids. The DELETE is legal. It succeeds. It matches
// nothing. Expired rows then accumulate forever.
//
// Since the differ learned to read `partition by`, an entity that already
// declares a ttl_field acquires this by adding one line to a .atl file.
//
// # Why the row counts alone cannot catch it
//
// Broken and fixed leave IDENTICAL rows behind: the broken version deletes
// nothing because RLS hides everything, the fixed version deletes nothing
// because it skipped. So the load-bearing assertion here is the COUNTER —
// sweeps_blocked_total is the only observable that distinguishes "nothing to
// expire" from "expiry silently stopped", and it is the series an operator
// alerts on.
//
// # Why a second role
//
// atlantis is a superuser on a development database, and row-level security
// does not apply to superusers. Run as the default role this test passes with
// the entire guard deleted, which is the failure mode it exists to prevent.
func TestSweeperReportsWhenRowLevelSecurityHidesItsRows(t *testing.T) {
	pool, ctx := schedTestPool(t)
	dsn := os.Getenv("ATLANTIS_TEST_PG")

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.swprls_blocked CASCADE`)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.swprls_open CASCADE`)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.swprls_quiet CASCADE`)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.swprls_declared CASCADE`)
		pgcatalog.Exec(t, dsn,
			`DROP OWNED BY swprls_probe`,
			`DROP ROLE IF EXISTS swprls_probe`)
	}
	drop()
	t.Cleanup(drop)

	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// Two entities, both with a ttl_field and both with expired rows. They
	// differ only in whether the tenant policy is enforced, which is the whole
	// question.
	mustExec(`CREATE TABLE atlantis.swprls_blocked (
	            id bigint PRIMARY KEY, tenant text NOT NULL,
	            expires_at timestamptz NOT NULL)`)
	mustExec(`ALTER TABLE atlantis.swprls_blocked ENABLE ROW LEVEL SECURITY`)
	mustExec(`ALTER TABLE atlantis.swprls_blocked FORCE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY p ON atlantis.swprls_blocked
	            USING (tenant = atlantis.current_partition())
	            WITH CHECK (tenant = atlantis.current_partition())`)
	mustExec(`INSERT INTO atlantis.swprls_blocked VALUES
	            (1, 'acme', now() - INTERVAL '1 hour'),
	            (2, 'acme', now() - INTERVAL '1 day')`)

	mustExec(`CREATE TABLE atlantis.swprls_open (
	            id bigint PRIMARY KEY, expires_at timestamptz NOT NULL)`)
	mustExec(`INSERT INTO atlantis.swprls_open VALUES
	            (1, now() - INTERVAL '1 hour'),
	            (2, now() - INTERVAL '1 day'),
	            (3, now() + INTERVAL '1 hour')`)

	// A third entity with NOTHING expired. It exists to pin the other half of
	// the fix: rows_swept_total used to be incremented only when n > 0, so an
	// entity that swept zero produced no series at all — and "nothing has
	// expired yet" was indistinguishable from "this has not been swept in a
	// month" from outside the process. The assertion is on series COUNT rather
	// than value, because calling WithLabelValues to read a value would create
	// the series and prove nothing.
	mustExec(`CREATE TABLE atlantis.swprls_quiet (
	            id bigint PRIMARY KEY, expires_at timestamptz NOT NULL)`)
	mustExec(`INSERT INTO atlantis.swprls_quiet VALUES (1, now() + INTERVAL '1 day')`)

	// A fourth entity where the SCHEMA and the CATALOGUE disagree, which is the
	// case that decides whether the guard asks the right question.
	//
	// It declares `partition by tenant` but carries no policy — the modal shape
	// for an entity that gained the clause after it was created, before any
	// migration ran. Row-level security is not enforced, so the sweep works
	// fine and must happen. A guard written as `e.PartitionField != ""` skips
	// it and expired rows pile up for no reason; asking pg_class gets it right.
	mustExec(`CREATE TABLE atlantis.swprls_declared (
	            id bigint PRIMARY KEY, tenant text NOT NULL,
	            expires_at timestamptz NOT NULL)`)
	mustExec(`INSERT INTO atlantis.swprls_declared VALUES
	            (1, 'acme', now() - INTERVAL '1 hour')`)

	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE swprls_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO swprls_probe;
GRANT SELECT ON atlantis.ir_checkpoint TO swprls_probe;
GRANT SELECT, DELETE ON atlantis.swprls_blocked, atlantis.swprls_open,
                         atlantis.swprls_quiet, atlantis.swprls_declared TO swprls_probe;`)
		return err
	})
	// Ownership matters: atlantis creates and owns the tables it serves, and
	// FORCE is what makes the policy apply to the owner. Without this the
	// blocked table would not be blocked and the test would prove nothing.
	for _, tbl := range []string{"swprls_blocked", "swprls_open", "swprls_quiet", "swprls_declared"} {
		mustExec(`ALTER TABLE atlantis.` + tbl + ` OWNER TO swprls_probe`)
	}

	ir := &dsl.IR{Entities: []dsl.Entity{
		{
			Name: "Blocked", Namespace: "swprls", TtlField: "expires_at",
			PartitionField: "tenant",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			},
		},
		{
			Name: "Open", Namespace: "swprls", TtlField: "expires_at",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			},
		},
		{
			Name: "Quiet", Namespace: "swprls", TtlField: "expires_at",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			},
		},
		{
			// Declared partitioned, no policy on the table. The schema says
			// blocked; the catalogue says sweepable, and the catalogue is right.
			Name: "Declared", Namespace: "swprls", TtlField: "expires_at",
			PartitionField: "tenant",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "expires_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			},
		},
	}}
	raw, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("marshal ir: %v", err)
	}
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-rls-test')
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir`, raw); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	t.Cleanup(func() {
		if saved != nil {
			_, _ = pool.Exec(context.Background(), `UPDATE atlantis.ir_checkpoint SET ir = $1 WHERE id = 1`, saved)
		} else {
			_, _ = pool.Exec(context.Background(), `DELETE FROM atlantis.ir_checkpoint WHERE id = 1`)
		}
	})

	restricted, err := pgxpool.New(ctx,
		strings.Replace(dsn, "//atlantis:atlantis@", "//swprls_probe:probe@", 1))
	if err != nil {
		t.Fatalf("connect as the restricted role: %v", err)
	}
	t.Cleanup(restricted.Close)

	blockedBefore := testutil.ToFloat64(sweepBlockedTotal.WithLabelValues("swprls.Blocked"))
	sweptBefore := testutil.ToFloat64(sweptTotal.WithLabelValues("swprls.Open"))
	// Counted, not read: touching swprls.Quiet's series here would create it.
	seriesBefore := testutil.CollectAndCount(sweptTotal)

	h := &SweepExpiredHandler{Pool: restricted, Logger: quietLogger(), BatchLimit: 100}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("the sweep failed outright: %v. One unsweepable entity must not "+
			"stop the others, and must not dead-letter a job that runs every five "+
			"minutes — an operator disables a handler that fails forever", err)
	}

	// THE assertion. Without the guard this counter never moves, because the
	// DELETE succeeds and reports zero.
	if got := testutil.ToFloat64(sweepBlockedTotal.WithLabelValues("swprls.Blocked")); got != blockedBefore+1 {
		t.Errorf("sweeps_blocked_total for swprls.Blocked = %v, want %v. Row-level "+
			"security hides every row from this sweep, so the DELETE succeeds and "+
			"matches nothing while expired rows pile up. Nothing else distinguishes "+
			"that from an entity with nothing to expire", got, blockedBefore+1)
	}

	// The other entity must still be swept. A guard that stopped the whole job
	// would trade a silent failure for a louder one.
	var openExpired int
	if err := restricted.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.swprls_open WHERE expires_at <= now()`).Scan(&openExpired); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openExpired != 0 {
		t.Errorf("%d expired rows survived on the unpartitioned entity. One entity "+
			"that cannot be swept has stopped the entities that can", openExpired)
	}
	if got := testutil.ToFloat64(sweptTotal.WithLabelValues("swprls.Open")); got != sweptBefore+2 {
		t.Errorf("rows_swept_total for swprls.Open = %v, want %v", got, sweptBefore+2)
	}

	// An entity that swept zero must still have produced a series.
	//
	// Exactly one more than the baseline, and the arithmetic is worth stating:
	// seriesBefore is taken AFTER reading swprls.Open's value, and reading it
	// creates its child, so Open is already counted. swprls.Blocked is skipped
	// before the counter and never gets one. That leaves swprls.Quiet — the
	// entity with nothing expired — as the only new series, which is precisely
	// the case the old `if n > 0` guard produced nothing for.
	if got := testutil.CollectAndCount(sweptTotal); got < seriesBefore+1 {
		t.Errorf("rows_swept_total has %d series, want at least %d. An entity that "+
			"swept zero rows produced no series, so from outside the process "+
			"'nothing has expired' and 'this entity stopped being swept a month "+
			"ago' look identical", got, seriesBefore+1)
	}

	// The declared-but-unenforced entity must have been swept. This is the
	// assertion that pins the CHOICE OF AUTHORITY: `e.PartitionField != ""` is
	// true here and the catalogue says sweepable, so a schema-based guard skips
	// a table it had no reason to skip.
	var declaredExpired int
	if err := restricted.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.swprls_declared WHERE expires_at <= now()`).Scan(&declaredExpired); err != nil {
		t.Fatalf("count declared: %v", err)
	}
	if declaredExpired != 0 {
		t.Errorf("%d expired rows survived on an entity that declares `partition by` "+
			"but carries no policy. Nothing hides its rows, so the sweep works — the "+
			"guard asked the schema when it should have asked the catalogue", declaredExpired)
	}

	// And the blocked table keeps its rows — stated so the skip is not confused
	// with a delete that happened to be invisible.
	var blockedRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.swprls_blocked`).Scan(&blockedRows); err != nil {
		t.Fatalf("count blocked: %v", err)
	}
	if blockedRows != 2 {
		t.Errorf("the blocked table has %d rows, want 2 — the sweeper deleted rows "+
			"it was supposed to have skipped", blockedRows)
	}
}
