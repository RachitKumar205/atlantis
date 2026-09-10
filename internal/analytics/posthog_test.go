package analytics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// batchRecorder collects the batches an httptest server receives.
type batchRecorder struct {
	mu      sync.Mutex
	bodies  []batchBody
	arrived chan struct{}
}

type batchBody struct {
	APIKey string      `json:"api_key"`
	Batch  []wireEvent `json:"batch"`
}

func newBatchRecorder() *batchRecorder {
	return &batchRecorder{arrived: make(chan struct{}, 64)}
}

func (b *batchRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/batch/" {
			t.Errorf("posted to %q, want /batch/", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q, want application/json", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		var body batchBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		b.mu.Lock()
		b.bodies = append(b.bodies, body)
		b.mu.Unlock()
		select {
		case b.arrived <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (b *batchRecorder) all() []batchBody {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]batchBody, len(b.bodies))
	copy(out, b.bodies)
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// Three events under the batch size leave as one request when Close flushes
// them, carrying the configured key rather than anything a caller supplied.
func TestCloseFlushesQueuedEventsAsOneBatch(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{
		APIKey:        "phc_test",
		Endpoint:      srv.URL,
		Logger:        quietLogger(),
		FlushInterval: time.Hour,
	})
	for _, name := range []string{"a.one", "a.two", "a.three"} {
		p.Capture(Event{Name: name, DistinctID: "user-1", Org: "acme"})
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	bodies := rec.all()
	if len(bodies) != 1 {
		t.Fatalf("got %d requests, want 1", len(bodies))
	}
	if bodies[0].APIKey != "phc_test" {
		t.Errorf("api_key %q, want phc_test", bodies[0].APIKey)
	}
	if len(bodies[0].Batch) != 3 {
		t.Fatalf("batch of %d, want 3", len(bodies[0].Batch))
	}
	for i, want := range []string{"a.one", "a.two", "a.three"} {
		if got := bodies[0].Batch[i].Event; got != want {
			t.Errorf("event %d is %q, want %q", i, got, want)
		}
	}
	groups, ok := bodies[0].Batch[0].Properties["$groups"].(map[string]any)
	if !ok || groups[GroupType] != "acme" {
		t.Errorf("group not set on the event: %v", bodies[0].Batch[0].Properties)
	}
}

// Reaching the batch size sends without waiting for the interval or for Close.
func TestBatchSizeTriggersDelivery(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{
		APIKey:        "phc_test",
		Endpoint:      srv.URL,
		Logger:        quietLogger(),
		BatchSize:     2,
		FlushInterval: time.Hour,
	})
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	p.Capture(Event{Name: "a.one", DistinctID: "u"})
	p.Capture(Event{Name: "a.two", DistinctID: "u"})

	select {
	case <-rec.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("a full batch was not delivered")
	}
}

// A queue with no room drops rather than waiting, and Capture stays fast while
// the delivery goroutine is stuck on a server that never answers.
func TestCaptureNeverBlocksOnAnUnreachableServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	p := NewPostHog(Config{
		APIKey:        "phc_test",
		Endpoint:      srv.URL,
		Logger:        quietLogger(),
		QueueSize:     4,
		BatchSize:     1,
		FlushInterval: time.Hour,
		Timeout:       time.Hour,
	})

	start := time.Now()
	for i := 0; i < 5000; i++ {
		p.Capture(Event{Name: "a.flood", DistinctID: "u"})
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("5000 captures took %v against a hung server", elapsed)
	}
	if p.Dropped() == 0 {
		t.Fatal("a queue of 4 accepted 5000 events without dropping any")
	}
}

// Close is idempotent, and a second call does not panic on a closed channel.
func TestCloseTwice(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{APIKey: "phc_test", Endpoint: srv.URL, Logger: quietLogger()})
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Capture after Close drops instead of panicking on a send to a closed sink.
func TestCaptureAfterCloseDrops(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{APIKey: "phc_test", Endpoint: srv.URL, Logger: quietLogger()})
	_ = p.Close(context.Background())

	p.Capture(Event{Name: "a.late", DistinctID: "u"})

	if got := len(rec.all()); got != 0 {
		t.Fatalf("%d requests after Close, want 0", got)
	}
	if p.Dropped() != 1 {
		t.Errorf("dropped %d, want 1", p.Dropped())
	}
}

// A refused batch is logged and discarded; nothing is retried.
func TestRefusedBatchIsNotRetried(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := NewPostHog(Config{APIKey: "phc_test", Endpoint: srv.URL, Logger: quietLogger()})
	p.Capture(Event{Name: "a.one", DistinctID: "u"})
	_ = p.Close(context.Background())

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("the server saw %d requests, want 1", calls)
	}
}

// GroupIdentify names the group and creates no person.
func TestGroupIdentifyShape(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{APIKey: "phc_test", Endpoint: srv.URL, Logger: quietLogger(), FlushInterval: time.Hour})
	p.GroupIdentify("acme", map[string]any{"provisioning_state": "ready"})
	_ = p.Close(context.Background())

	bodies := rec.all()
	if len(bodies) != 1 || len(bodies[0].Batch) != 1 {
		t.Fatalf("got %d bodies, want one batch of one", len(bodies))
	}
	ev := bodies[0].Batch[0]
	if ev.Event != "$groupidentify" {
		t.Errorf("event %q, want $groupidentify", ev.Event)
	}
	if ev.Properties["$group_type"] != GroupType {
		t.Errorf("group type %v, want %q", ev.Properties["$group_type"], GroupType)
	}
	if ev.Properties["$group_key"] != "acme" {
		t.Errorf("group key %v, want acme", ev.Properties["$group_key"])
	}
	if ev.Properties["$process_person_profile"] != false {
		t.Errorf("a group update created a person profile: %v", ev.Properties)
	}
}

// An event with no name or no actor is not sent; the id would be meaningless
// and PostHog would reject the batch.
func TestIncompleteEventsAreNotSent(t *testing.T) {
	rec := newBatchRecorder()
	srv := rec.server(t)

	p := NewPostHog(Config{APIKey: "phc_test", Endpoint: srv.URL, Logger: quietLogger(), FlushInterval: time.Hour})
	p.Capture(Event{DistinctID: "u"})
	p.Capture(Event{Name: "a.one"})
	p.GroupIdentify("", nil)
	_ = p.Close(context.Background())

	if got := len(rec.all()); got != 0 {
		t.Fatalf("%d requests, want 0", got)
	}
}

// The caller's property map is not modified by the group and person keys.
func TestPropertiesDoesNotMutateTheCallersMap(t *testing.T) {
	props := map[string]any{"method": "password"}
	got := properties(Event{Props: props, Org: "acme", NoPersonProfile: true})

	if _, ok := props["$groups"]; ok {
		t.Error("the caller's map gained $groups")
	}
	if len(props) != 1 {
		t.Errorf("the caller's map has %d keys, want 1", len(props))
	}
	if got["method"] != "password" {
		t.Error("the property did not carry through")
	}
}

// Discard runs every instrumented path without a project key.
func TestDiscardAcceptsEverything(t *testing.T) {
	var s Sink = Discard{}
	s.Capture(Event{Name: "a.one", DistinctID: "u"})
	s.GroupIdentify("acme", nil)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCheckEndpointNamesTheSetting covers the hosts a deployment can be
// configured with. The setting name is in the message because the same check
// serves CLOUD_POSTHOG_HOST from two processes.
func TestCheckEndpointNamesTheSetting(t *testing.T) {
	ok := []string{
		DefaultEndpoint,
		"http://posthog.internal:8000",
		"https://eu.i.posthog.com/",
	}
	for _, endpoint := range ok {
		if err := CheckEndpoint("SETTING", endpoint); err != nil {
			t.Errorf("CheckEndpoint(%q) = %v, want nil", endpoint, err)
		}
	}

	bad := map[string]string{
		"":                              "absolute",
		"us.i.posthog.com":              "absolute",
		"/relative":                     "absolute",
		"ftp://us.i.posthog.com":        "absolute",
		"https://user:pw@posthog.local": "userinfo",
	}
	for endpoint, want := range bad {
		err := CheckEndpoint("SETTING", endpoint)
		if err == nil {
			t.Errorf("CheckEndpoint(%q) = nil, want an error", endpoint)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckEndpoint(%q) = %v, want it to mention %q", endpoint, err, want)
		}
		if !strings.Contains(err.Error(), "SETTING") {
			t.Errorf("CheckEndpoint(%q) = %v, want it to name the setting", endpoint, err)
		}
	}
}

// TestPersonPropertiesRideUnderSet covers the $set channel the email uses.
func TestPersonPropertiesRideUnderSet(t *testing.T) {
	props := properties(Event{
		Name:       "account.signed_in",
		DistinctID: "user-1",
		Person:     map[string]any{PersonEmail: "ada@example.com"},
	})
	set, ok := props["$set"].(map[string]any)
	if !ok {
		t.Fatalf("$set is %T, want a map", props["$set"])
	}
	if set[PersonEmail] != "ada@example.com" {
		t.Errorf("$set carries %v, want the address", set)
	}
}

// A machine event drops person properties rather than creating the profile
// NoPersonProfile exists to suppress.
func TestAMachineEventCarriesNoPerson(t *testing.T) {
	props := properties(Event{
		Name:            EventOrgProvisioned,
		DistinctID:      MachineID("acme"),
		NoPersonProfile: true,
		Person:          map[string]any{PersonEmail: "ada@example.com"},
	})
	if _, ok := props["$set"]; ok {
		t.Errorf("a machine event carried $set: %v", props)
	}
	if props["$process_person_profile"] != false {
		t.Errorf("$process_person_profile is %v, want false", props["$process_person_profile"])
	}
}

// An event with no person properties sends no $set at all.
func TestNoPersonPropertiesSendsNoSet(t *testing.T) {
	props := properties(Event{Name: "x", DistinctID: "user-1"})
	if _, ok := props["$set"]; ok {
		t.Errorf("an event with no person properties carried $set: %v", props)
	}
}
