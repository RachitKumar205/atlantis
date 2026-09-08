package analytics

import (
	"context"
	"sync"
)

// Recorder keeps events in memory for tests. Its zero value is ready to use.
type Recorder struct {
	mu     sync.Mutex
	events []Event
	groups []GroupUpdate
	closed bool
}

// GroupUpdate is one GroupIdentify call.
type GroupUpdate struct {
	Org   string
	Props map[string]any
}

func (r *Recorder) Capture(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *Recorder) GroupIdentify(org string, props map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups = append(r.groups, GroupUpdate{Org: org, Props: props})
}

func (r *Recorder) Close(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// Events returns a copy of what has been captured, oldest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Groups returns a copy of the GroupIdentify calls, oldest first.
func (r *Recorder) Groups() []GroupUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]GroupUpdate, len(r.groups))
	copy(out, r.groups)
	return out
}

// Named returns the captured events with this name.
func (r *Recorder) Named(name string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// Closed reports whether Close has been called.
func (r *Recorder) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Reset drops everything recorded.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
	r.groups = nil
	r.closed = false
}
