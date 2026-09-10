package console

import (
	"io"
	"log/slog"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// report is the half of logAction that runs after the insert. Tested on its
// own so the projection dispatch does not need a database.
func reportOnly(sink analytics.Sink) *store {
	return &store{
		log:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		sink: sink,
	}
}

func TestAnAuditedConsoleActionBecomesAnEvent(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", "user-1", "ada@example.com", "register_caller", map[string]any{
		"caller": "billing-api", "can_mutate": true,
	})

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Name != "console.caller_registered" {
		t.Errorf("event %q, want console.caller_registered", e.Name)
	}
	if e.DistinctID != "user-1" {
		t.Errorf("distinct id %q, want the Cloud user id", e.DistinctID)
	}
	if e.Org != "acme" {
		t.Errorf("org %q, want acme", e.Org)
	}
	if e.Props["can_mutate"] != true {
		t.Errorf("can_mutate is %v, want true", e.Props["can_mutate"])
	}
	if e.Person[analytics.PersonEmail] != "ada@example.com" {
		t.Errorf("person properties are %v, want the actor's address", e.Person)
	}
	// The caller name is a customer-chosen identifier.
	for _, v := range e.Props {
		if v == "billing-api" {
			t.Errorf("the caller name crossed: %v", e.Props)
		}
	}
}

// An action the catalogue does not know is reported nowhere rather than
// guessed at.
func TestAnUnknownConsoleActionEmitsNothing(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", "user-1", "ada@example.com", "something.nobody.catalogued",
		map[string]any{"secret": "x"})

	if got := len(rec.Events()); got != 0 {
		t.Fatalf("got %d events, want 0", got)
	}
}

// A console with no project key runs every path.
func TestConsoleNoSinkIsSafe(t *testing.T) {
	s := reportOnly(nil)
	s.report("acme", "user-1", "ada@example.com", "register_caller", map[string]any{"can_mutate": true})
}

// An action with no address still reports; the person keeps whatever a
// previous event set.
func TestAConsoleActionWithNoAddressStillReports(t *testing.T) {
	rec := &analytics.Recorder{}
	s := reportOnly(rec)

	s.report("acme", "user-1", "", "sandbox_destroyed", nil)

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if len(events[0].Person) != 0 {
		t.Errorf("person properties are %v, want none", events[0].Person)
	}
}
