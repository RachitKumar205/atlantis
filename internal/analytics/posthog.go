package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// DefaultEndpoint is PostHog Cloud's US ingestion host.
const DefaultEndpoint = "https://us.i.posthog.com"

// CheckEndpoint reports why endpoint cannot be posted to, naming setting in the
// error. It is the caller's job to decide whether the rule applies.
//
// A relative URL, or one carrying userinfo, fails at every delivery rather than
// at boot.
func CheckEndpoint(setting, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute http or https URL (got %q)", setting, endpoint)
	}
	if u.User != nil {
		return fmt.Errorf("%s must carry no userinfo (got %q)", setting, endpoint)
	}
	return nil
}

const (
	defaultQueueSize     = 1024
	defaultBatchSize     = 50
	defaultFlushInterval = 5 * time.Second
	defaultTimeout       = 5 * time.Second

	// dropLogInterval bounds how often a full queue is reported. A sink that
	// is dropping is dropping steadily, and one line per event would be the
	// larger problem.
	dropLogInterval = time.Minute
)

// Config configures a PostHog sink. Only APIKey is required.
type Config struct {
	// APIKey is the project write key. It appears in the request body, not a
	// header.
	APIKey string

	// Endpoint is the ingestion host. Empty means DefaultEndpoint.
	Endpoint string

	// Client overrides the HTTP client. Nil builds one bounded by Timeout.
	Client *http.Client

	// Logger records drops and delivery failures. Nil uses the default.
	Logger *slog.Logger

	// QueueSize bounds events held between flushes. Zero means 1024.
	QueueSize int

	// BatchSize is the event count that triggers a flush. Zero means 50.
	BatchSize int

	// FlushInterval is the longest an event waits. Zero means 5s.
	FlushInterval time.Duration

	// Timeout bounds one delivery. Zero means 5s.
	Timeout time.Duration
}

// PostHog batches events and posts them.
//
// Capture hands the event to a buffered channel and returns; one goroutine
// owns delivery. A full queue drops rather than waiting, so a slow or
// unreachable PostHog cannot reach back into a request.
//
// Delivery is attempted once. A retry would queue behind the failure that
// caused it, which is the outage this sink exists to survive.
type PostHog struct {
	cfg      Config
	ch       chan wireEvent
	done     chan struct{}
	finished chan struct{}
	closeOne sync.Once

	dropped     atomic.Int64
	lastDropLog atomic.Int64
}

// NewPostHog starts a sink. Close stops it.
func NewPostHog(cfg Config) *PostHog {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	p := &PostHog{
		cfg:      cfg,
		ch:       make(chan wireEvent, cfg.QueueSize),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *PostHog) Capture(e Event) {
	if e.Name == "" || e.DistinctID == "" {
		return
	}
	p.enqueue(wireEvent{
		Event:      e.Name,
		DistinctID: e.DistinctID,
		Properties: properties(e),
		Timestamp:  stamp(e.Time),
	})
}

func (p *PostHog) GroupIdentify(org string, props map[string]any) {
	if org == "" {
		return
	}
	set := props
	if set == nil {
		set = map[string]any{}
	}
	p.enqueue(wireEvent{
		Event: "$groupidentify",
		// $groupidentify carries no person. The id keeps the request
		// well-formed and is the same machine id an unattended event uses.
		DistinctID: MachineID(org),
		Properties: map[string]any{
			"$group_type":             GroupType,
			"$group_key":              org,
			"$group_set":              set,
			"$process_person_profile": false,
		},
		Timestamp: stamp(time.Time{}),
	})
}

// enqueue offers one event to the delivery goroutine, counting a drop when the
// queue is full or the sink has been closed.
//
// The channel is never closed, so a Capture racing Close cannot panic; the
// goroutine stops on done and the buffered events go with it.
func (p *PostHog) enqueue(w wireEvent) {
	select {
	case <-p.done:
		p.recordDrop()
		return
	default:
	}
	select {
	case p.ch <- w:
	default:
		p.recordDrop()
	}
}

func (p *PostHog) recordDrop() {
	n := p.dropped.Add(1)
	now := time.Now().UnixNano()
	last := p.lastDropLog.Load()
	if now-last < int64(dropLogInterval) {
		return
	}
	if !p.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	p.logger().Warn("analytics events dropped", "total", n)
}

func (p *PostHog) Close(ctx context.Context) error {
	p.closeOne.Do(func() { close(p.done) })
	select {
	case <-p.finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped returns the number of events the sink discarded.
func (p *PostHog) Dropped() int64 { return p.dropped.Load() }

func (p *PostHog) loop() {
	defer close(p.finished)

	t := time.NewTicker(p.cfg.FlushInterval)
	defer t.Stop()

	batch := make([]wireEvent, 0, p.cfg.BatchSize)
	for {
		select {
		case w := <-p.ch:
			batch = append(batch, w)
			if len(batch) >= p.cfg.BatchSize {
				p.flush(batch)
				batch = batch[:0]
			}
		case <-t.C:
			if len(batch) > 0 {
				p.flush(batch)
				batch = batch[:0]
			}
		case <-p.done:
			// Take what is already buffered, then send once and stop.
			for {
				select {
				case w := <-p.ch:
					batch = append(batch, w)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				p.flush(batch)
			}
			return
		}
	}
}

// flush posts one batch. A failure is logged and the events are gone.
func (p *PostHog) flush(batch []wireEvent) {
	if len(batch) == 0 {
		return
	}
	payload, err := json.Marshal(struct {
		APIKey string      `json:"api_key"`
		Batch  []wireEvent `json:"batch"`
	}{APIKey: p.cfg.APIKey, Batch: batch})
	if err != nil {
		p.logger().Warn("analytics batch could not be encoded", "events", len(batch), "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint+"/batch/", bytes.NewReader(payload))
	if err != nil {
		p.logger().Warn("analytics request could not be built", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		p.logger().Warn("analytics batch not delivered", "events", len(batch), "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded so the remote end cannot choose this process's memory use.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		p.logger().Warn("analytics batch refused",
			"events", len(batch), "status", resp.StatusCode, "body", truncate(string(raw), 200))
	}
}

func (p *PostHog) logger() *slog.Logger {
	if p.cfg.Logger != nil {
		return p.cfg.Logger
	}
	return slog.Default()
}

// wireEvent is one entry of PostHog's batch array.
type wireEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties,omitempty"`
	Timestamp  string         `json:"timestamp,omitempty"`
}

// MachineID is the distinct id for an event with no person behind it.
func MachineID(org string) string { return "org:" + org }

// properties copies an event's properties and adds the group and person keys.
// The caller's map is not modified.
func properties(e Event) map[string]any {
	out := make(map[string]any, len(e.Props)+2)
	for k, v := range e.Props {
		out[k] = v
	}
	if e.Org != "" {
		out["$groups"] = map[string]string{GroupType: e.Org}
	}
	if e.NoPersonProfile {
		out["$process_person_profile"] = false
	}
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "… (truncated)"
}

var _ Sink = (*PostHog)(nil)
var _ Sink = Discard{}
var _ Sink = (*Recorder)(nil)
