package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Expiry works on a tenant-isolated table.
//
// This is the property #50 exists for, and every previous attempt at it
// collided with a decision already made:
//
//   - bind a tenant: expiry must cover EVERY tenant, so there is no one correct
//     value to bind
//   - enumerate tenants: needs the cross-tenant read the policy prevents, and
//     there is no tenant registry
//   - a BYPASSRLS role: pg.RequireIsolatedRole refuses one at boot
//
// Dropping chunks sidesteps all three because it is DDL, and row-level security
// filters DML only. This test is the proof of that claim rather than the
// argument for it: the table below has FORCE ROW LEVEL SECURITY on with a
// policy the sweeper cannot satisfy, and the chunks go anyway.
//
// The equivalent DELETE is what TestSweeperSkipsRlsBlockedEntity covers — it
// matches nothing and succeeds, which is the whole defect.
func TestSweeperDropsChunksOnATenantIsolatedHypertable(t *testing.T) {
	pool, ctx := schedTestPool(t)

	var hasTimescale bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).
		Scan(&hasTimescale); err != nil {
		t.Fatalf("probe timescaledb: %v", err)
	}
	if !hasTimescale {
		t.Skip("timescaledb not installed in the test database")
	}

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.swp_event CASCADE`)
	}
	reset()
	t.Cleanup(reset)

	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// occurred_at is BOTH the time dimension and the ttl_field. That equality
	// is a correctness condition, not a convenience: drop_chunks selects
	// chunks by the time dimension, so a ttl on a different column could drop
	// a chunk holding rows that have not expired. schema.ExpiryFor enforces it.
	mustExec(`CREATE TABLE atlantis.swp_event (
	            tenant      text NOT NULL,
	            occurred_at timestamptz NOT NULL,
	            body        text)`)
	mustExec(`SELECT create_hypertable('atlantis.swp_event', 'occurred_at',
	            chunk_time_interval => INTERVAL '1 day', if_not_exists => TRUE)`)

	// Old rows across two tenants, in chunks that closed long ago, plus a
	// current row that must survive. Two tenants because the point is that
	// expiry covers every tenant without naming any of them.
	mustExec(`INSERT INTO atlantis.swp_event VALUES
	            ('acme',  now() - INTERVAL '30 days', 'old-acme'),
	            ('globex', now() - INTERVAL '20 days', 'old-globex'),
	            ('acme',  now(),                      'current')`)

	// The isolation the sweeper cannot satisfy: it binds no tenant, so
	// current_partition() is NULL and this policy admits nothing.
	mustExec(`ALTER TABLE atlantis.swp_event ENABLE ROW LEVEL SECURITY`)
	mustExec(`ALTER TABLE atlantis.swp_event FORCE ROW LEVEL SECURITY`)
	mustExec(`CREATE POLICY swp_event_tenant_isolation ON atlantis.swp_event
	            AS RESTRICTIVE USING (tenant = atlantis.current_partition())`)
	mustExec(`CREATE POLICY swp_event_default_access ON atlantis.swp_event
	            AS PERMISSIVE USING (true) WITH CHECK (true)`)

	// A role the policy actually applies to.
	//
	// The default test role is a superuser with BYPASSRLS, for which every
	// policy above is inert and the "blocked" DELETE removes rows.
	// pg.RequireIsolatedRole refuses a superuser at boot, so production is
	// never in that state and a test asserting an RLS property must not be.
	//
	// Ownership is transferred because FORCE ROW LEVEL SECURITY only binds the
	// owner, and drop_chunks requires ownership.
	sweeperPool := nonSuperuserPool(t, pool, ctx, "swpchunk_app", "atlantis.swp_event")

	// Prove the premise rather than assume it: an unbound DELETE really does
	// see nothing as this role. Without this the test could pass on a table
	// that was never actually isolated.
	tag, err := sweeperPool.Exec(ctx,
		`DELETE FROM atlantis.swp_event WHERE occurred_at < now() - INTERVAL '7 days'`)
	if err != nil {
		t.Fatalf("probe delete: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Fatalf("the probe DELETE removed %d rows, so this table is not actually "+
			"hiding rows from an unbound statement and the test proves nothing", n)
	}

	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Event", Namespace: "swp",
		Kind:           dsl.EntityKindHypertable,
		TimeField:      "occurred_at",
		TtlField:       "occurred_at",
		PartitionField: "tenant",
		Fields: []dsl.Field{
			{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "occurred_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}}}
	raw, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("marshal ir: %v", err)
	}
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-chunk-test')
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

	before := testutil.ToFloat64(sweptChunksTotal.WithLabelValues("swp.Event"))

	h := &SweepExpiredHandler{Pool: sweeperPool, Logger: quietLogger(), BatchLimit: 100}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("sweep returned an error: %v", err)
	}

	// Counted from the catalogue, not through the policy — a SELECT here would
	// be filtered the same way the DELETE was and would report zero whatever
	// happened.
	var chunks int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM timescaledb_information.chunks
 WHERE hypertable_schema = 'atlantis' AND hypertable_name = 'swp_event'`).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 1 {
		t.Errorf("%d chunks remain, want 1 (only the current one). Expiry on a "+
			"tenant-isolated table is the thing this mechanism exists to make "+
			"work", chunks)
	}

	after := testutil.ToFloat64(sweptChunksTotal.WithLabelValues("swp.Event"))
	if after-before != 2 {
		t.Errorf("chunks_dropped_total moved by %v, want 2. Both tenants' old "+
			"chunks should have gone, and the metric is the only way an operator "+
			"sees that it happened", after-before)
	}
}

// A chunk still holding unexpired rows is left alone.
//
// drop_chunks removes a chunk only when its whole time range has passed, so
// the current chunk survives even though older_than is now(). Worth pinning
// separately: a bound that dropped the straddling chunk would delete live
// data, and the failure would look like "expiry works" until someone noticed
// recent rows missing.
func TestSweeperKeepsTheChunkHoldingLiveRows(t *testing.T) {
	pool, ctx := schedTestPool(t)

	var hasTimescale bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).
		Scan(&hasTimescale); err != nil {
		t.Fatalf("probe timescaledb: %v", err)
	}
	if !hasTimescale {
		t.Skip("timescaledb not installed in the test database")
	}

	reset := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.swp_live CASCADE`)
	}
	reset()
	t.Cleanup(reset)

	for _, sql := range []string{
		`CREATE TABLE atlantis.swp_live (
		   occurred_at timestamptz NOT NULL, body text)`,
		`SELECT create_hypertable('atlantis.swp_live', 'occurred_at',
		   chunk_time_interval => INTERVAL '1 day', if_not_exists => TRUE)`,
		`INSERT INTO atlantis.swp_live VALUES (now(), 'current')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Live", Namespace: "swp",
		Kind:      dsl.EntityKindHypertable,
		TimeField: "occurred_at",
		TtlField:  "occurred_at",
		Fields: []dsl.Field{
			{Name: "occurred_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}}}
	raw, _ := json.Marshal(ir)
	var saved []byte
	_ = pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&saved)
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'sweeper-chunk-test')
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

	// Counted BEFORE the sweep, and by series rather than by value.
	//
	// The obvious assertion — ToFloat64(sweptChunksTotal.WithLabelValues(
	// "swp.Live")) == 0 — proves nothing: WithLabelValues CREATES the child it
	// looks up, so it reads 0 whether the sweeper published a zero or never
	// touched the metric at all. Counting series either side of the sweep is the
	// version that can tell those apart.
	seriesBefore := testutil.CollectAndCount(sweptChunksTotal)

	h := &SweepExpiredHandler{Pool: pool, Logger: quietLogger(), BatchLimit: 100}
	if err := h.Handle(ctx, nil); err != nil {
		t.Fatalf("sweep returned an error: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlantis.swp_live`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d rows remain, want 1. The chunk holding the current row was "+
			"dropped, which deletes data whose ttl has not passed", rows)
	}

	// A sweep that drops nothing still has to publish a zero.
	//
	// A counter added only when n > 0 reports an entity with nothing to expire
	// and one whose expiry broke months ago identically: no series at all.
	// `rate() == 0` is answerable on a zero; a missing series is not.
	if got := testutil.CollectAndCount(sweptChunksTotal); got != seriesBefore+1 {
		t.Errorf("the sweep dropped no chunks and published %d series where %d "+
			"were expected, so an entity whose expiry has silently stopped looks "+
			"exactly like one with nothing to expire",
			got-seriesBefore, 1)
	}
}

// nonSuperuserPool returns a pool connected as a role the RLS policies on
// `table` actually apply to, and hands it ownership so FORCE ROW LEVEL
// SECURITY binds it and drop_chunks is permitted.
//
// The default test role is a superuser with BYPASSRLS, for which every policy
// is inert, so a test asserting an RLS property as that role asserts nothing.
//
// pg.RequireIsolatedRole refuses a superuser at boot, so production is never in
// the state the default test role is in; this closes the gap between them.
func nonSuperuserPool(
	t *testing.T, admin *pgxpool.Pool, ctx context.Context, role, table string,
) *pgxpool.Pool {
	t.Helper()

	for _, sql := range []string{
		fmt.Sprintf(`DROP OWNED BY %s`, role),
		fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role),
	} {
		_, _ = admin.Exec(ctx, sql)
	}
	for _, sql := range []string{
		fmt.Sprintf(
			`CREATE ROLE %s LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
			role),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA atlantis TO %s`, role),
		// The sweeper reads the IR checkpoint before it can sweep anything.
		fmt.Sprintf(`GRANT SELECT ON atlantis.ir_checkpoint TO %s`, role),
		// Ownership: FORCE ROW LEVEL SECURITY binds the owner, and drop_chunks
		// requires it. Both halves of what this pool is for.
		fmt.Sprintf(`ALTER TABLE %s OWNER TO %s`, table, role),
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		// Ownership goes back before the role can be dropped, and the table is
		// dropped by the caller's own cleanup either way.
		_, _ = admin.Exec(context.Background(),
			fmt.Sprintf(`ALTER TABLE %s OWNER TO CURRENT_USER`, table))
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP OWNED BY %s`, role))
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	})

	cfg := admin.Config().Copy()
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "probe"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	return p
}
