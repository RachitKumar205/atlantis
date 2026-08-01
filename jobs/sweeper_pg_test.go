package jobs

import (
	"encoding/json"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
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
	if err := h.Handle(ctx, nil); err == nil {
		t.Error("a sweep that could not delete anything reported success. The job " +
			"runtime marks it complete, no retry happens, nothing is dead-lettered, " +
			"and a sweeper broken for months looks exactly like one with no work")
	}
}
