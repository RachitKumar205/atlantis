package analytics

import "context"

// Discard accepts events and does nothing with them. Its zero value is ready
// to use, and it is what a deployment with no project key gets, so an
// unconfigured binary runs every instrumented path.
type Discard struct{}

func (Discard) Capture(Event) {}

func (Discard) GroupIdentify(string, map[string]any) {}

func (Discard) Close(context.Context) error { return nil }
