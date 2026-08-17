package admin

import (
	"context"
	"strings"
	"testing"
)

// The refusal has to be reached by the real ApplyMigration, not merely exist.
//
// unexpirable_test.go proves unexpirableEntities classifies correctly. That is a
// different claim from "apply consults it", and the difference is not academic:
// deleting the two lines in ApplyMigration that call it left the whole suite
// green. A guard nothing calls is the shape this repo has shipped before — a
// feature complete in every respect except being wired to anything.
//
// Both directions are driven. A gate that refused every declaration would pass
// the refusal test and make the mechanism #50 exists to provide unreachable.

// Partitioned, has a ttl, and is not a hypertable: the sweeper's DELETE would
// match nothing and succeed.
const unexpDeleteSweep = `
entity Doc in unex {
  id         bigint primary
  tenant     varchar(16) not null
  expires_at timestamptz not null
  partition by tenant
  ttl_field expires_at
}
`

// The same declaration as a hypertable on its ttl column: expiry drops chunks,
// which RLS does not filter.
//
// `primary by id, occurred_at` rather than `id primary` because TimescaleDB
// refuses a unique index that does not include the partitioning column, and a
// single-column primary key is one. The first draft of this fixture used
// `id primary` and the apply failed with SQLSTATE TS103 — which is what every
// hypertable example in the docs would do.
const unexpChunkDrop = `
hypertable Event in unex on occurred_at {
  id          bigint not null
  tenant      varchar(16) not null
  occurred_at timestamptz not null
  primary by id, occurred_at
  chunk_time_interval 1d
  partition by tenant
  ttl_field occurred_at
}
`

func TestApplyRefusesExpiryItCannotPerform(t *testing.T) {
	svc := depScopeService(t)

	plan := depScopePlan(t, svc, "unex", "doc.atl", unexpDeleteSweep)
	err := depScopeApply(t, svc, "unex", "doc.atl", unexpDeleteSweep, plan)
	if err == nil {
		t.Fatal("the apply accepted `ttl_field` alongside `partition by` on a " +
			"plain entity. The checkpoint now records a retention rule the " +
			"sweeper cannot carry out, and the rows accumulate forever")
	}

	// Asserted on content, not merely on being non-nil. The change policy also
	// refuses applies, so a test satisfied by any error would pass while this
	// check was never consulted — which is how the neighbouring
	// unreachable-tenant test was nearly written.
	for _, want := range []string{"unex.Doc", "hypertable", "partition by", "caller", "time dimension"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so either it is not this "+
				"check firing or an author hitting it has one fewer way out "+
				"than exists: %v", want, err)
		}
	}

	// A gate, not a message: the DDL must not have run.
	var n int
	if qerr := svc.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = 'unex_doc'`).Scan(&n); qerr != nil {
		t.Fatalf("count tables: %v", qerr)
	}
	if n != 0 {
		t.Errorf("the refused apply created the table anyway, so the refusal is "+
			"advice rather than a gate (%d matching relations)", n)
	}
}

func TestApplyAcceptsChunkDroppingExpiry(t *testing.T) {
	svc := depScopeService(t)

	plan := depScopePlan(t, svc, "unex", "event.atl", unexpChunkDrop)
	if err := depScopeApply(t, svc, "unex", "event.atl", unexpChunkDrop, plan); err != nil {
		t.Fatalf("the apply refused a hypertable whose ttl_field is its time "+
			"dimension, which is the one shape that makes expiry work on a "+
			"tenant-isolated table: %v", err)
	}

	// It is a hypertable in the catalogue, not just an accepted declaration.
	// Without this the test would pass on an apply that quietly emitted a plain
	// table, and drop_chunks would then fail at every sweep.
	var n int
	if err := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM timescaledb_information.hypertables
 WHERE hypertable_schema = 'atlantis' AND hypertable_name = 'unex_event'`).Scan(&n); err != nil {
		t.Fatalf("count hypertables: %v", err)
	}
	if n != 1 {
		t.Errorf("unex_event is not a hypertable (%d found), so the chunk-dropping "+
			"expiry this declaration asked for has nothing to drop", n)
	}
}
