package jobs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The reaper's metrics must actually move.
//
// A declared-and-never-incremented metric is the same failure as a
// registered-and-never-scheduled job: it exists, it scrapes, it reads as
// healthy, and it carries no information. `atlantis_reaper_objects_due` is the
// number an operator would alert on, so a version of it that is always zero is
// worse than not publishing it.
func TestReaperMetricsMove(t *testing.T) {
	pool, ctx := schedTestPool(t)

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.met_due__parked`)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'met_%'`)
	}
	reset()
	t.Cleanup(reset)

	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS atlantis_tombstone`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE atlantis_tombstone.met_due__parked (id int)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// One due and reapable, one still inside its window so the pending gauge
	// has something to hold after the run.
	if _, err := pool.Exec(ctx, `INSERT INTO atlantis.parked_objects
	   (kind, schema_name, object_name, original_schema, original_name, reap_after) VALUES
	   ('table','atlantis_tombstone','met_due__parked','atlantis','met_due',  now() - INTERVAL '1 day'),
	   ('table','atlantis_tombstone','met_wait__parked','atlantis','met_wait', now() + INTERVAL '30 days')`); err != nil {
		t.Fatalf("register: %v", err)
	}

	before := testutil.ToFloat64(reapedTotal.WithLabelValues("table"))

	h := &ReapParkedHandler{Pool: pool, Logger: quietLogger()}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if got := testutil.ToFloat64(reapedTotal.WithLabelValues("table")); got <= before {
		t.Errorf("atlantis_reaper_objects_reaped_total did not move after a "+
			"successful reap (%v -> %v). The counter is declared and never "+
			"incremented, which scrapes as a permanently healthy system", before, got)
	}

	// The gauges are read from the register at the end of every run, so the
	// property worth asserting is that they agree with the database — not a
	// fixed number, since this register is shared with every other test in the
	// package. A gauge derived only from what THIS process reaped would read
	// zero on a replica that never wins a job while the register fills up.
	var wantPending, wantDue float64
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE reap_after <= now())
  FROM atlantis.parked_objects WHERE reaped_at IS NULL`).Scan(&wantPending, &wantDue); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got := testutil.ToFloat64(parkedPending); got != wantPending {
		t.Errorf("atlantis_reaper_objects_pending = %v, database says %v. A "+
			"backlog gauge that disagrees with the register is worse than none",
			got, wantPending)
	}
	if got := testutil.ToFloat64(parkedDue); got != wantDue {
		t.Errorf("atlantis_reaper_objects_due = %v, database says %v. This is "+
			"the number that stays high when reaping is stuck, so it has to "+
			"track the register", got, wantDue)
	}
	// And this test's own due object must be gone from that count.
	if wantPending < 1 {
		t.Error("the object still inside its retention window is not counted as " +
			"pending")
	}
}

// A failed reap must be counted separately from a successful one.
func TestReaperFailureMetricMoves(t *testing.T) {
	pool, ctx := schedTestPool(t)

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP VIEW IF EXISTS atlantis_tombstone.met_dep`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.met_stuck__parked`)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'met_%'`)
	}
	reset()
	t.Cleanup(reset)

	for _, sql := range []string{
		`CREATE SCHEMA IF NOT EXISTS atlantis_tombstone`,
		`CREATE TABLE atlantis_tombstone.met_stuck__parked (id int)`,
		`CREATE VIEW atlantis_tombstone.met_dep AS SELECT id FROM atlantis_tombstone.met_stuck__parked`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO atlantis.parked_objects
	   (kind, schema_name, object_name, original_schema, original_name, reap_after)
	   VALUES ('table','atlantis_tombstone','met_stuck__parked','atlantis','met_stuck', now() - INTERVAL '1 day')`); err != nil {
		t.Fatalf("register: %v", err)
	}

	before := testutil.ToFloat64(reapFailuresTotal.WithLabelValues("table"))
	h := &ReapParkedHandler{Pool: pool, Logger: quietLogger()}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := testutil.ToFloat64(reapFailuresTotal.WithLabelValues("table")); got <= before {
		t.Errorf("atlantis_reaper_reap_failures_total did not move after a drop "+
			"that could not succeed (%v -> %v). Failures and no-ops are then "+
			"indistinguishable, which is the state this metric exists to end",
			before, got)
	}
}
