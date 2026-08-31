package admin

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The control database and the managed one.
//
// A server with no managed DSN manages itself, which is what every deployment
// before the split did. A server with one keeps its own tables in the control
// database and reads, plans and applies against the other — so an organisation
// that adopted an existing database runs no DDL to describe tables it already
// has.
//
// The apply path decides whether to commit once or twice by comparing the two
// pointers, so these assert the identity New establishes rather than dialling.

// poolSentinel returns a distinct pool pointer. Never connected: New records
// it and the apply path compares it.
func poolSentinel() *pgxpool.Pool { return &pgxpool.Pool{} }

func TestNewWithoutAManagedPoolManagesItself(t *testing.T) {
	ctl := poolSentinel()
	s := New(ctl, Config{})
	if s.managed != s.pool {
		t.Error("a Service with no managed pool does not manage its own database; " +
			"every deployment predating the split configures nothing and must " +
			"behave exactly as it did — one transaction, one commit")
	}
	if s.managed != ctl {
		t.Error("the managed pool is not the control pool")
	}
}

func TestNewWithAManagedPoolKeepsThemApart(t *testing.T) {
	ctl, mgd := poolSentinel(), poolSentinel()
	if ctl == mgd {
		t.Fatal("setup: the sentinels are the same pointer")
	}

	s := New(ctl, Config{Managed: mgd})
	if s.pool != ctl {
		t.Error("the control pool is not the one passed to New")
	}
	if s.managed != mgd {
		t.Error("the managed pool is not the one configured; introspection and " +
			"DDL would target the control database")
	}
	if s.managed == s.pool {
		t.Error("the two pools compare equal, so apply would commit once and " +
			"the DDL would land in the control database")
	}
}

// GetCanonicalIR reads the checkpoint, which is control-database state. A
// managed pool must not move it: the record of what atlantis holds lives with
// atlantis, not with the schema it manages.
func TestCanonicalIRStaysOnTheControlDatabase(t *testing.T) {
	s := New(nil, Config{Managed: poolSentinel()})
	resp, err := s.GetCanonicalIR(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetCanonicalIR: %v", err)
	}
	// A nil control pool is the no-PG path, which answers the empty
	// checkpoint. Reading the managed pool instead would answer from the
	// wrong database.
	if string(resp.GetIr()) != "null" {
		t.Errorf("ir = %q, want null — the read did not use the control pool",
			resp.GetIr())
	}
}
