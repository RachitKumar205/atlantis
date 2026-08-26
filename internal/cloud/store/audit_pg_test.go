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
	// The provisioner names itself, so its rows are not read as rows that lost
	// their actor.
	if e.Actor != ProvisionerActor {
		t.Errorf("actor = %q, want %q", e.Actor, ProvisionerActor)
	}
	if e.Detail["endpoint"] != "atl-dev.test:31234" {
		t.Errorf("detail did not round-trip: %v", e.Detail)
	}
}

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

// LogAction returns nothing, so the log line is the only trace of a failed
// write. An audit log that silently stopped recording looks like a period with
// nothing to record.
func TestAFailedAuditWriteIsLoggedRatherThanDiscarded(t *testing.T) {
	_, dsn := newTestStoreWithDSN(t)

	var buf bytes.Buffer
	db, err := New(context.Background(), dsn, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Close the pool so the write cannot succeed.
	db.Close()

	db.LogAction(context.Background(), "gone", ProvisionerActor, "", "org_provisioned", nil)

	if !strings.Contains(buf.String(), "audit write failed") {
		t.Fatalf("a failed audit write emitted nothing; log was:\n%s", buf.String())
	}
}

// Detail that will not marshal costs the detail, not the entry.
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
