package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PostOnce delivers one event and returns, for a process that exits before a
// queue would drain.
//
// endpoint is a base URL whose /api/t/batch route forwards to PostHog. The
// body carries no api_key: the proxy deletes whatever a client sends and
// substitutes the project's own, so a distributed binary holds no key.
//
// The caller's context bounds the whole exchange. Every failure is returned
// rather than logged, so a command's output is the command's.
func PostOnce(ctx context.Context, endpoint string, e Event) error {
	if e.Name == "" || e.DistinctID == "" {
		return fmt.Errorf("analytics: an event needs a name and an actor")
	}
	payload, err := json.Marshal(struct {
		Batch []wireEvent `json:"batch"`
	}{Batch: []wireEvent{{
		Event:      e.Name,
		DistinctID: e.DistinctID,
		Properties: properties(e),
		Timestamp:  stamp(e.Time),
	}}})
	if err != nil {
		return fmt.Errorf("analytics: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+ProxyPath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("analytics: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("analytics: deliver: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("analytics: refused with %d", resp.StatusCode)
	}
	return nil
}

// ProxyPath is the route on a deployment that forwards to PostHog's batch
// endpoint. internal/cloud/server serves it; the browsers and the CLI post
// here so neither holds a project key.
const ProxyPath = "/api/t/batch"
