package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	analyticsPathPrefix = "/api/t/"

	analyticsMaxBody  = 64 << 10
	analyticsMaxBatch = 50

	// analyticsRateLimit is requests per minute per address. A page sends one
	// batch every few seconds.
	analyticsRateLimit = 120

	analyticsTimeout = 5 * time.Second

	// analyticsFlushTimeout bounds the last delivery at shutdown.
	analyticsFlushTimeout = 2 * time.Second
)

// analyticsUpstreamPaths are the ingestion paths that forward, with no leading
// or trailing slash.
//
// PostHog serves /static/ and /array/ from the same host, and both return
// JavaScript. Forwarding either would let their CDN satisfy `script-src
// 'self'` on this origin.
var analyticsUpstreamPaths = map[string]bool{
	"e":      true,
	"i/v0/e": true,
	"batch":  true,
}

// analyticsTokenFields are the top-level keys PostHog reads a project token
// from, in its own precedence order, ahead of api_key.
//
// posthog/api/utils.py resolves $token, then token, then api_key, then
// properties.token. Substituting api_key alone leaves three other ways to name
// a different project.
var analyticsTokenFields = []string{"$token", "token", "api_key"}

// handleAnalytics forwards one ingestion request to PostHog.
//
// No session is required: the sign-up funnel is measured before one exists.
// What bounds it is the path allowlist, the size caps, the per-address rate
// limit, and rewriteAnalyticsKey.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PostHogKey == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	// A cross-origin POST carrying this Content-Type is preflighted, and no
	// CORS header comes back, so a page on another origin cannot reach this.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "", http.StatusUnsupportedMediaType)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOriginAnalytics(origin) {
		http.Error(w, "", http.StatusForbidden)
		return
	}
	if !s.analyticsAllowed(w, r) {
		return
	}

	upstream := strings.Trim(strings.TrimPrefix(r.URL.Path, analyticsPathPrefix), "/")
	if !analyticsUpstreamPaths[upstream] {
		http.NotFound(w, r)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, analyticsMaxBody+1))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	if len(raw) > analyticsMaxBody {
		http.Error(w, "", http.StatusRequestEntityTooLarge)
		return
	}

	body, err := rewriteAnalyticsKey(raw, s.cfg.PostHogKey)
	switch {
	case err == errTooManyEvents:
		http.Error(w, "", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		s.log.Warn("analytics body refused", "err", err)
		http.Error(w, "", http.StatusUnsupportedMediaType)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), analyticsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(s.cfg.PostHogHost, "/")+"/"+upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// PostHog routes on Host and answers 401 when it receives this origin's.
	req.Host = hostOf(s.cfg.PostHogHost)
	req.Header.Set("X-Forwarded-For", s.clientIP(r))

	resp, err := analyticsClient.Do(req)
	if err != nil {
		http.Error(w, "", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// sameOriginAnalytics reports whether an Origin header names this deployment.
func (s *Server) sameOriginAnalytics(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	pub, err := url.Parse(s.cfg.PublicURL)
	if err != nil || pub.Host == "" {
		return false
	}
	return u.Host == pub.Host
}

// analyticsAllowed reports whether this address may send another batch,
// answering it if not.
func (s *Server) analyticsAllowed(w http.ResponseWriter, r *http.Request) bool {
	ok, retry := s.analyticsLim.allow(s.clientIP(r))
	if ok {
		return true
	}
	w.Header().Set("Retry-After", itoa(retry))
	http.Error(w, "", http.StatusTooManyRequests)
	return false
}

// rewriteAnalyticsKey removes every field PostHog reads a token from and sets
// api_key to this deployment's, so a request writes into this project or none.
//
// Values other than those fields are re-encoded from their original bytes, so
// number precision and key order inside them survive.
func rewriteAnalyticsKey(raw []byte, key string) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	// A JSON null decodes into a nil map and returns no error; assigning to
	// one panics.
	if body == nil {
		return nil, errNotAnObject
	}
	if b, ok := body["batch"]; ok {
		var events []json.RawMessage
		if err := json.Unmarshal(b, &events); err != nil {
			return nil, err
		}
		if len(events) > analyticsMaxBatch {
			return nil, errTooManyEvents
		}
	}
	if props, ok := body["properties"]; ok {
		body["properties"] = stripToken(props)
	}
	for _, f := range analyticsTokenFields {
		delete(body, f)
	}
	quoted, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	body["api_key"] = quoted
	return json.Marshal(body)
}

// stripToken removes token from a properties object, returning a value that is
// not an object unchanged.
func stripToken(raw json.RawMessage) json.RawMessage {
	var props map[string]json.RawMessage
	if err := json.Unmarshal(raw, &props); err != nil || props == nil {
		return raw
	}
	if _, ok := props["token"]; !ok {
		return raw
	}
	delete(props, "token")
	out, err := json.Marshal(props)
	if err != nil {
		return raw
	}
	return out
}

type errAnalytics string

func (e errAnalytics) Error() string { return string(e) }

var (
	errTooManyEvents = errAnalytics("batch holds more events than the cap")
	errNotAnObject   = errAnalytics("body is not a JSON object")
)

// hostOf returns the authority of an absolute URL, port included.
func hostOf(rawURL string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// analyticsClient bounds concurrency to PostHog. The default transport caps
// neither connections nor idle connections per host, so an inbound burst opens
// one each.
var analyticsClient = &http.Client{
	Timeout: analyticsTimeout,
	Transport: &http.Transport{
		MaxConnsPerHost:     32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}
