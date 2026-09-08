package store

import (
	"io"
	"log/slog"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// report is the half of LogAction that runs after the insert. Tested on its
// own so the projection dispatch does not need a database.
func reportOnly(sink analytics.Sink) *Store {
	return &Store{
		log:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		sink: sink,
	}
}

func TestAnAuditedActionBecomesAnEvent(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", "user-1", "org.created", map[string]any{"display_name": "Acme Ltd"})

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Name != analytics.EventOrgCreated {
		t.Errorf("event %q, want %q", e.Name, analytics.EventOrgCreated)
	}
	if e.DistinctID != "user-1" {
		t.Errorf("distinct id %q, want the actor", e.DistinctID)
	}
	if e.Org != "acme" {
		t.Errorf("org %q, want acme", e.Org)
	}
	if e.Props["display_name_set"] != true {
		t.Errorf("display_name_set is %v, want true", e.Props["display_name_set"])
	}
	// The name itself is customer text.
	for _, v := range e.Props {
		if v == "Acme Ltd" {
			t.Errorf("the display name crossed: %v", e.Props)
		}
	}
}

// Work the provisioner did on its own has no person behind it.
func TestProvisionerActionsCreateNoPerson(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", ProvisionerActor, "org.provisioned", map[string]any{
		"attempts": 2, "took_ms": 48000, "endpoint": "org-acme.example.dev:31234",
	})

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.DistinctID != analytics.MachineID("acme") {
		t.Errorf("distinct id %q, want the machine id", e.DistinctID)
	}
	if !e.NoPersonProfile {
		t.Error("a provisioner event would create a person profile")
	}
	if _, ok := e.Props["endpoint"]; ok {
		t.Errorf("the endpoint crossed: %v", e.Props)
	}
	if e.Props["attempts"] != float64(2) {
		t.Errorf("attempts is %v, want 2", e.Props["attempts"])
	}
}

// An action the catalogue marks as not reported emits nothing.
func TestAnUnreportedActionEmitsNothing(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", ProvisionerActor, "org.console_credentials_rotated", nil)

	if got := len(rec.Events()); got != 0 {
		t.Fatalf("got %d events, want 0", got)
	}
}

// An action with no projection is reported nowhere rather than guessed at.
func TestAnUnknownActionEmitsNothing(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", "user-1", "something.nobody.catalogued", map[string]any{"secret": "x"})

	if got := len(rec.Events()); got != 0 {
		t.Fatalf("got %d events, want 0", got)
	}
}

// A store with no sink runs every path, which is what a deployment with no
// project key does.
func TestNoSinkIsSafe(t *testing.T) {
	s := reportOnly(nil)
	s.report("acme", "user-1", "org.created", map[string]any{"display_name": "Acme"})
}
