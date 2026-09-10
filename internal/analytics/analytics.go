// Package analytics reports product events to PostHog.
//
// A Sink accepts events and never blocks its caller. Delivery is best effort:
// events are batched, sent once, and dropped on failure. Nothing here can fail
// a request, delay one, or return an error to the code being measured.
//
// Discard is the zero-configuration implementation and is what a deployment
// with no project key gets.
package analytics

import (
	"context"
	"time"
)

// GroupType is the single PostHog group every event is keyed on. One
// organisation is one account: it owns the provisioning, the console session
// and the row-level security boundary.
const GroupType = "organisation"

// Event is one thing that happened.
type Event struct {
	// Name is the event name, dotted, from the catalogue.
	Name string

	// DistinctID identifies the actor. For a person this is the Cloud user id,
	// which is also the audit actor. For an unattended process it is
	// "org:"+org, paired with NoPersonProfile.
	DistinctID string

	// Org names the organisation the event belongs to, and becomes the group
	// key. Empty for an account event raised before any organisation exists.
	Org string

	// Props are the event properties. Every key is named by a Projection or by
	// the handler that raised the event; no map is copied in wholesale.
	Props map[string]any

	// NoPersonProfile suppresses person-profile creation, for events whose
	// DistinctID is a machine. It also drops Person, which describes a person
	// the event does not have.
	NoPersonProfile bool

	// Person updates the properties of the person DistinctID names. PostHog
	// merges these under $set, so a key sent once stays until it is
	// overwritten, and an event may send a subset.
	//
	// The keys are the Person* constants in catalogue.go and no others.
	Person map[string]any

	// Time is when it happened. Zero means the moment Capture was called.
	Time time.Time
}

// Sink accepts events.
//
// Capture returns nothing, and for the same reason logAction and LogAction
// return nothing: an action that succeeded is not undone because recording it
// failed. A Sink must be safe for concurrent use.
type Sink interface {
	// Capture records one event. It does not block.
	Capture(Event)

	// GroupIdentify sets properties on an organisation.
	GroupIdentify(org string, props map[string]any)

	// Close flushes what is queued, bounded by ctx, and stops the sink.
	// Calling it more than once is safe.
	Close(ctx context.Context) error
}
