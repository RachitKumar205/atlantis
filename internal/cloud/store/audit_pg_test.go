package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestAuditRecordsWhatHappened(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	db.LogAction(ctx, "audited", ProvisionerActor, "", "org_provisioned",
		map[string]any{"endpoint": "atl-dev.test:31234", "attempts": 2})

	entries, err := db.AuditFor(ctx, "audited", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.Action != "org_provisioned" {
		t.Errorf("action = %q", e.Action)
	}
	// A machine names itself. An empty actor reads as a bug in the logging
	// rather than as a machine acting on its own behalf.
	if e.Actor != ProvisionerActor {
		t.Errorf("actor = %q, want %q", e.Actor, ProvisionerActor)
	}
	if e.Detail["endpoint"] != "atl-dev.test:31234" {
		t.Errorf("detail did not round-trip: %v", e.Detail)
	}
}

// Entries are scoped to the organisation they concern. Without this the log is
// a single stream nobody can answer a question from.
func TestAuditIsScopedToItsOrganisation(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	db.LogAction(ctx, "scoped-a", ProvisionerActor, "", "org_provisioned", nil)
	db.LogAction(ctx, "scoped-b", ProvisionerActor, "", "org_provisioned", nil)

	entries, err := db.AuditFor(ctx, "scoped-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Org != "scoped-a" {
			t.Fatalf("reading scoped-a returned an entry for %q", e.Org)
		}
	}
}

// An action that succeeded is not undone because recording it failed — but the
// failure must not be silent either.
//
// The console learned this the hard way and its comment records it: an audit
// log that silently stops recording looked exactly like months with nothing to
// record. So this asserts both halves — no panic, no error propagated, and a
// warning actually emitted.
func TestAFailedAuditWriteIsLoggedRatherThanDiscarded(t *testing.T) {
	_, dsn := newTestStoreWithDSN(t)

	var buf bytes.Buffer
	db, err := New(context.Background(), dsn, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Close the pool so the write cannot succeed. Crude, and the only failure
	// available without reaching into the driver.
	db.Close()

	db.LogAction(context.Background(), "gone", ProvisionerActor, "", "org_provisioned", nil)

	if !strings.Contains(buf.String(), "audit write failed") {
		t.Fatalf("a failed audit write emitted nothing; log was:\n%s", buf.String())
	}
}

// Detail that will not marshal costs the detail, never the entry. The action,
// the actor and the time are the parts somebody is asking about.
func TestUnencodableDetailStillRecordsTheAction(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// A channel cannot be marshalled to JSON.
	db.LogAction(ctx, "baddetail", ProvisionerActor, "", "org_provisioned",
		map[string]any{"ch": make(chan int)})

	entries, err := db.AuditFor(ctx, "baddetail", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 — the action was lost with its detail", len(entries))
	}
	if entries[0].Action != "org_provisioned" {
		t.Errorf("action = %q", entries[0].Action)
	}
}
