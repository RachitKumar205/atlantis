package jobs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The reaper is the half of park-and-reap that costs money when it is missing
// and costs data when it is wrong, so it is tested against a real database on
// both counts: an object past its window is dropped, and an object still inside
// its window is not.
//
// The second assertion is the one that matters. A reaper that drops everything
// it finds passes any test that only checks "the due object is gone", and it
// turns a recovery window into a slower deletion.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./jobs/ -run Reap -v

// cleanExec runs a cleanup statement and fails the test if it fails, so a
// fixture left in the shared database is reported.
func cleanExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Errorf("clean up %q: %v", sql, err)
	}
}

func TestReapDropsOnlyWhatIsDue(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reaper")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	reset := func() {
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis_tombstone.reap_due__parked`)
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis_tombstone.reap_pending__parked`)
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis.reap_host`)
		cleanExec(t, ctx, pool, `DELETE FROM atlantis.parked_objects WHERE object_name LIKE 'reap_%'
		                        OR object_name LIKE '%__parked' AND parent_table = 'reap_host'`)
	}
	reset()
	t.Cleanup(reset)

	exec(`CREATE SCHEMA IF NOT EXISTS atlantis_tombstone`)
	// One table past its window, one still inside it.
	exec(`CREATE TABLE atlantis_tombstone.reap_due__parked (id int, body text)`)
	exec(`INSERT INTO atlantis_tombstone.reap_due__parked VALUES (1,'expendable')`)
	exec(`CREATE TABLE atlantis_tombstone.reap_pending__parked (id int, body text)`)
	exec(`INSERT INTO atlantis_tombstone.reap_pending__parked VALUES (1,'still protected')`)
	// And a parked column on a live table, past its window.
	exec(`CREATE TABLE atlantis.reap_host (id int PRIMARY KEY, keep text, gone__parked text)`)
	exec(`INSERT INTO atlantis.reap_host VALUES (1,'kept','expendable')`)

	exec(`INSERT INTO atlantis.parked_objects
	        (kind, schema_name, object_name, parent_table, original_schema, original_name, reap_after) VALUES
	        ('table','atlantis_tombstone','reap_due__parked',NULL,'atlantis','reap_due', now() - INTERVAL '1 day'),
	        ('table','atlantis_tombstone','reap_pending__parked',NULL,'atlantis','reap_pending', now() + INTERVAL '30 days'),
	        ('column','atlantis','gone__parked','reap_host','atlantis','gone', now() - INTERVAL '1 day')`)

	h := &ReapParkedHandler{Pool: pool}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("reap: %v", err)
	}

	exists := func(sql string) bool {
		var b bool
		if err := pool.QueryRow(ctx, sql).Scan(&b); err != nil {
			t.Fatalf("probe: %v", err)
		}
		return b
	}

	if exists(`SELECT EXISTS (SELECT 1 FROM information_schema.tables
	             WHERE table_schema='atlantis_tombstone' AND table_name='reap_due__parked')`) {
		t.Error("a table past its retention window was not reaped; tombstones accumulate forever")
	}

	// The assertion that stops the reaper being a slower deletion.
	if !exists(`SELECT EXISTS (SELECT 1 FROM information_schema.tables
	             WHERE table_schema='atlantis_tombstone' AND table_name='reap_pending__parked')`) {
		t.Error("a table still INSIDE its retention window was reaped. The window is " +
			"the entire recovery guarantee; reaping early turns park-and-reap into " +
			"a deletion with extra steps")
	}

	if exists(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
	             WHERE table_schema='atlantis' AND table_name='reap_host' AND column_name='gone__parked')`) {
		t.Error("a parked column past its window was not reaped")
	}
	if !exists(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
	             WHERE table_schema='atlantis' AND table_name='reap_host' AND column_name='keep')`) {
		t.Error("the reaper dropped a LIVE column. It must only ever touch names the " +
			"register lists")
	}

	// Reaped rows are stamped, not deleted: "this existed and was removed on
	// this date" is the question asked afterwards, and a deleted row cannot
	// answer it.
	var stamped, pending int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE reaped_at IS NOT NULL),
       count(*) FILTER (WHERE reaped_at IS NULL)
  FROM atlantis.parked_objects
 WHERE (schema_name = 'atlantis_tombstone' AND object_name LIKE 'reap_%__parked')
    OR parent_table = 'reap_host'`).Scan(&stamped, &pending); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stamped != 2 {
		t.Errorf("%d registrations stamped reaped, want 2", stamped)
	}
	if pending != 1 {
		t.Errorf("%d registrations still pending, want 1 (the one inside its window)", pending)
	}

	// Running again must be a no-op rather than re-attempting the drops.
	if err := h.Handle(ctx, nil); err != nil {
		t.Errorf("second run failed: %v", err)
	}
}

// A registration the reaper cannot render safely must be skipped, not guessed
// at. Dropping the wrong object is worse than dropping nothing.
func TestReapRefusesIncompleteRegistrations(t *testing.T) {
	for _, p := range []parkedRow{
		{kind: "table", schemaName: "", objectName: "x"},
		{kind: "column", schemaName: "atlantis", parentTable: "", objectName: "x"},
		{kind: "sequence", schemaName: "atlantis", objectName: "x"},
	} {
		if _, err := reapStatement(p); err == nil {
			t.Errorf("rendered DDL for an incomplete registration %+v", p)
		}
	}
}

// Dropping a parked column is ALTER TABLE on a live table, so it needs ACCESS
// EXCLUSIVE. A reaper waiting for that lock does more than fail to reap: a
// pending ACCESS EXCLUSIVE request blocks every reader queueing behind it, so a
// background job with no deadline takes the table down to drop a hidden column.
//
// This drives the real conflict: another session holds the table, the reaper
// runs, and the assertions are that it gives up quickly, changes nothing, and
// leaves its connection usable.
func TestReapGivesUpRatherThanBlockingTheTable(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reaper")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	reset := func() {
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis.reap_locked`)
		cleanExec(t, ctx, pool, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'reap_locked'`)
	}
	reset()
	t.Cleanup(reset)

	if _, err := pool.Exec(ctx,
		`CREATE TABLE atlantis.reap_locked (id int PRIMARY KEY, gone__parked text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO atlantis.parked_objects
	        (kind, schema_name, object_name, parent_table, original_schema, original_name, reap_after)
	 VALUES ('column','atlantis','gone__parked','reap_locked','atlantis','gone', now() - INTERVAL '1 day')`); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A separate connection holds the table for longer than the reaper's
	// patience. Acquired before the reaper starts and released after it
	// returns, so the conflict is guaranteed rather than raced for.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE atlantis.reap_locked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	// The reaper gets its own single-connection pool so that "was the
	// connection destroyed" is directly observable. On a larger pool it is not:
	// pgxpool discards a connection left in a failed transaction, but only
	// opens a replacement when something next needs one, so NewConnsCount lags
	// and an assertion on it passes whether or not the connection survived.
	// Capped at one, a destroyed connection MUST be rebuilt for the next
	// checkout, and the counter moves the moment it happens.
	reapCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	reapCfg.MaxConns = 1
	reapPool, err := pgxpool.NewWithConfig(ctx, reapCfg)
	if err != nil {
		t.Fatalf("reap pool: %v", err)
	}
	defer reapPool.Close()
	if _, err := reapPool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("warm: %v", err)
	}

	h := &ReapParkedHandler{Pool: reapPool, LockTimeout: 300 * time.Millisecond}
	connsBefore := reapPool.Stat().NewConnsCount()

	// Run it off the test goroutine so that a reaper which DOES wait fails this
	// test in five seconds with the message below. Called inline, an unbounded
	// wait blocks here until the package-wide `go test` timeout, which panics,
	// kills every other test in the package mid-flight, skips t.Cleanup, and
	// never prints the diagnosis.
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- h.Handle(ctx, nil) }()

	var waited time.Duration
	select {
	case err = <-done:
		waited = time.Since(start)
	case <-time.After(5 * time.Second):
		_ = tx.Rollback(ctx)
		holder.Release()
		t.Fatal("the reaper is still waiting for a lock it should have abandoned " +
			"after 300ms. While it waits, every reader of the table queues behind " +
			"its pending ACCESS EXCLUSIVE request — the background job takes the " +
			"table down to delete a column nobody can see")
	}

	_ = tx.Rollback(ctx)
	holder.Release()

	if err != nil {
		t.Fatalf("a lock conflict is an expected outcome and must not fail the job: %v", err)
	}
	if waited > 5*time.Second {
		t.Errorf("the reaper waited %s for a lock it should have abandoned after 300ms", waited)
	}

	// A failed reap must not cost a connection. As a multi-statement
	// simple-protocol string, a failure makes Postgres skip the COMMIT, the
	// connection returns to the pool inside a failed transaction and pgxpool
	// destroys it. Nothing else observably breaks — pgx rebuilds the backend —
	// so this asserts the churn rather than that later queries still work.
	if _, err := reapPool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("post-reap query: %v", err)
	}
	if churn := reapPool.Stat().NewConnsCount() - connsBefore; churn > 0 {
		t.Errorf("a failed reap destroyed %d pooled connection(s). This pool also "+
			"serves auth, the admin service and the rate limiter, and a stuck "+
			"batch reaps on every run forever", churn)
	}

	var live, pending int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM information_schema.columns
         WHERE table_schema='atlantis' AND table_name='reap_locked'
           AND column_name='gone__parked'),
       (SELECT count(*) FROM atlantis.parked_objects
         WHERE parent_table='reap_locked' AND reaped_at IS NULL)`).Scan(&live, &pending); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if live != 1 {
		t.Error("the column was dropped despite the reap failing to get its lock")
	}
	// The stamp is inside the same transaction as the drop, so a refused reap
	// must leave the registration exactly as it was — otherwise the register
	// records a reap that never happened and the object is orphaned forever.
	if pending != 1 {
		t.Error("a reap that never ran was recorded as done. The object still " +
			"exists and nothing will ever try to remove it again")
	}

	// The failed reap ran BEGIN ... COMMIT over the simple protocol. If the
	// abort left the connection inside a failed transaction, the pool would
	// hand it out in that state and unrelated work would fail with "current
	// transaction is aborted".
	for i := 0; i < 5; i++ {
		var n int
		if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil {
			t.Fatalf("the pool is poisoned after a failed reap: %v", err)
		}
	}

	// A lock conflict counts as a failed attempt and backs off, so the row is
	// not retried immediately and a contended table is not hammered every run.
	// Clearing the backoff stands in for the wait; the same registration then
	// reaps normally, since a lock timeout may not wedge the reaper for good.
	if _, err := pool.Exec(ctx, `UPDATE atlantis.parked_objects
	     SET next_attempt_after = NULL WHERE parent_table = 'reap_locked'`); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema='atlantis' AND table_name='reap_locked' AND column_name='gone__parked'`).Scan(&live); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if live != 0 {
		t.Error("the column survived a reap with no lock contention, so the " +
			"lock timeout made the reaper permanently unable to reap")
	}
}

// No reap may ever CASCADE — including the column branch, which is the one
// that touches a LIVE table.
//
// The table branch is the safe one: a tombstone in atlantis_tombstone has
// almost nothing depending on it by construction. ALTER TABLE ... DROP COLUMN
// ... CASCADE silently drops dependent views, foreign keys and indexes on a
// table that is in production use.
func TestNoReapCascades(t *testing.T) {
	for _, p := range []parkedRow{
		{kind: "table", schemaName: "atlantis_tombstone", objectName: "t__parked"},
		{kind: "column", schemaName: "atlantis", parentTable: "live", objectName: "c__parked"},
	} {
		stmt, err := reapStatement(p)
		if err != nil {
			t.Fatalf("%s: %v", p.kind, err)
		}
		if strings.Contains(stmt, "CASCADE") {
			t.Errorf("the %s reap cascades, so a failed drop destroys whatever "+
				"depended on the object instead of stopping to be looked at: %s",
				p.kind, stmt)
		}
		if !strings.Contains(stmt, "IF EXISTS") {
			t.Errorf("the %s reap has no IF EXISTS. An object removed by hand then "+
				"aborts the transaction, the stamp never lands, and the "+
				"registration is stuck failing forever: %s", p.kind, stmt)
		}
	}
}

// quoteIdent is the only thing standing between a register row and DDL.
func TestQuoteIdentEscapesHostileIdentifiers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`a"b`, `"a""b"`},
		{`"; DROP TABLE users; --`, `"""; DROP TABLE users; --"`},
		{`Mixed Case`, `"Mixed Case"`},
	} {
		if got := quoteIdent(tc.in); got != tc.want {
			t.Errorf("quoteIdent(%q) = %s, want %s. Every reap interpolates this "+
				"straight into DDL", tc.in, got, tc.want)
		}
	}
}

// A stuck registration must not stop everything behind it.
//
// The reaper selects the oldest due rows and a failed drop leaves reaped_at
// NULL, so without backoff the same unreapable rows are selected on every run
// and nothing else is ever reached. In production that is BatchLimit 50: fifty
// dependency-blocked tombstones permanently halt all reaping, while every run
// logs "batch complete" and returns success.
func TestOneStuckObjectDoesNotStarveTheRest(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reaper")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	reset := func() {
		cleanExec(t, ctx, pool, `DROP VIEW IF EXISTS atlantis_tombstone.starve_dep`)
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis_tombstone.starve_stuck__parked`)
		cleanExec(t, ctx, pool, `DROP TABLE IF EXISTS atlantis_tombstone.starve_free__parked`)
		cleanExec(t, ctx, pool, `DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'starve_%'`)
	}
	reset()
	t.Cleanup(reset)

	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	mustExec(`CREATE SCHEMA IF NOT EXISTS atlantis_tombstone`)
	mustExec(`CREATE TABLE atlantis_tombstone.starve_stuck__parked (id int)`)
	// A dependency added after the park — the failure mode the reaper's own
	// comment calls the usual cause.
	mustExec(`CREATE VIEW atlantis_tombstone.starve_dep AS
	            SELECT id FROM atlantis_tombstone.starve_stuck__parked`)
	mustExec(`CREATE TABLE atlantis_tombstone.starve_free__parked (id int)`)

	// The stuck one is OLDER, so ORDER BY reap_after puts it first.
	mustExec(`INSERT INTO atlantis.parked_objects
	   (kind, schema_name, object_name, parent_table, original_schema, original_name, reap_after) VALUES
	   ('table','atlantis_tombstone','starve_stuck__parked',NULL,'atlantis','starve_stuck', now() - INTERVAL '9 days'),
	   ('table','atlantis_tombstone','starve_free__parked', NULL,'atlantis','starve_free',  now() - INTERVAL '1 day')`)

	// BatchLimit 1 makes the starvation deterministic: one slot, and the stuck
	// row is first. Without backoff it takes that slot on every run forever.
	h := &ReapParkedHandler{Pool: pool, BatchLimit: 1}
	for i := 0; i < 3; i++ {
		if err := h.Handle(ctx, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var freeGone bool
	if err := pool.QueryRow(ctx, `
SELECT NOT EXISTS (SELECT 1 FROM information_schema.tables
   WHERE table_schema='atlantis_tombstone' AND table_name='starve_free__parked')`).Scan(&freeGone); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !freeGone {
		t.Error("a perfectly droppable tombstone was never reaped across three " +
			"runs, because one undroppable registration ahead of it took the " +
			"batch every time. Nothing else will ever be reaped either")
	}

	// And the stuck one must be backed off with its reason recorded, not
	// silently retried forever with no trace.
	var attempts int
	var lastErr *string
	var backedOff bool
	if err := pool.QueryRow(ctx, `
SELECT attempts, last_error, next_attempt_after > now()
  FROM atlantis.parked_objects WHERE original_name = 'starve_stuck'`).Scan(
		&attempts, &lastErr, &backedOff); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if attempts == 0 || !backedOff {
		t.Errorf("the stuck registration has attempts=%d backed_off=%v; it is "+
			"being retried on every run with nothing to show for it", attempts, backedOff)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "depend") {
		t.Error("the failure reason was not recorded. An operator asking why a " +
			"tombstone is still there has only an hourly WARN line to go on")
	}
}

// BatchLimit is a documented bound; nothing exercised it.
func TestReapHonoursItsBatchLimit(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reaper")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	reset := func() {
		for i := 0; i < 3; i++ {
			cleanExec(t, ctx, pool, fmt.Sprintf(
				`DROP TABLE IF EXISTS atlantis_tombstone.batch%d__parked`, i))
		}
		cleanExec(t, ctx, pool, `DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'batch%'`)
	}
	reset()
	t.Cleanup(reset)

	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS atlantis_tombstone`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE atlantis_tombstone.batch%d__parked (id int)`, i)); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO atlantis.parked_objects
		   (kind, schema_name, object_name, original_schema, original_name, reap_after)
		   VALUES ('table','atlantis_tombstone',$1,'atlantis',$2, now() - INTERVAL '1 day')`,
			fmt.Sprintf("batch%d__parked", i), fmt.Sprintf("batch%d", i)); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	h := &ReapParkedHandler{Pool: pool, BatchLimit: 2}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("reap: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlantis.parked_objects
	  WHERE original_name LIKE 'batch%' AND reaped_at IS NULL`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d registrations left pending after a batch of 2 over 3 due "+
			"objects, want 1. The bound that keeps a reap from holding locks "+
			"across a huge batch is not being applied", remaining)
	}
}
