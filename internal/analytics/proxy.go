package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxyPathPrefix is the route prefix a deployment serves ingestion on. The
// segments after it name the upstream endpoint.
const ProxyPathPrefix = "/api/t/"

const (
	proxyMaxBody  = 64 << 10
	proxyMaxBatch = 50

	// ProxyRateLimit is requests per minute per address. A page sends one
	// event every few seconds.
	ProxyRateLimit = 120

	// ProxyMaxAddresses bounds how many addresses a deployment tracks for this
	// route.
	//
	// A sliding-window limiter holds up to one timestamp per allowed request
	// per address, 24 bytes each, until its sweep. At ProxyRateLimit that is
	// 1024 × 120 × 24 B ≈ 3 MB of timestamps, against ≈ 250 KB for a limiter
	// sized for sign-in. The route takes no session, so the ceiling is one an
	// unauthenticated caller chooses to reach and this deployment chooses to
	// hold.
	//
	// A new address arriving at the cap is refused, which costs it analytics
	// and nothing else.
	ProxyMaxAddresses = 1024

	proxyTimeout = 5 * time.Second
)

// proxyUpstreamPaths are the ingestion paths that forward, with no leading or
// trailing slash.
//
// PostHog serves /static/ and /array/ from the same host, and both return
// JavaScript. Forwarding either would let their CDN satisfy `script-src
// 'self'` on this origin.
var proxyUpstreamPaths = map[string]bool{
	"e":      true,
	"i/v0/e": true,
	"batch":  true,
}

// proxyTokenFields are the top-level keys PostHog reads a project token from,
// in its own precedence order, ahead of api_key.
//
// posthog/api/utils.py resolves $token, then token, then api_key, then
// properties.token. Substituting api_key alone leaves three other ways to name
// a different project.
var proxyTokenFields = []string{"$token", "token", "api_key"}

// proxyEventNames are the reserved PostHog event names the route forwards.
//
// A name beginning with $ is one PostHog acts on rather than counts:
// $groupidentify writes an organisation's group properties, $create_alias
// joins two people into one. The route carries no session, so the two names
// the browsers send are the two it will forward.
//
// A name that does not begin with $ is forwarded whatever it is. An invented
// `console.caller_registered` is a wrong number in a chart; an invented
// $groupidentify is a customer's organisation properties overwritten.
var proxyEventNames = map[string]bool{
	"$pageview": true,
	"$identify": true,
}

// proxyPersonFields are the property keys that write a person's profile.
//
// $set on an event describes the person its distinct_id names, so a caller
// that guesses a Cloud user id writes that person's address or display name.
// Nothing sent through this route uses them: the servers set person properties
// through their own sink, which holds the project key and takes no input from
// outside.
var proxyPersonFields = []string{"$set", "$set_once", "$unset"}

// ProxyConfig is what a deployment gives Proxy.
type ProxyConfig struct {
	// Key is the project write key. Empty makes every route 404, which is
	// every development and CI build.
	Key string

	// Host is the ingestion host. Empty means DefaultEndpoint.
	Host string

	// PublicURL is the address browsers reach this deployment at, compared
	// against Origin. Empty compares against the request's own Host, which is
	// the same comparison for a browser: a cross-origin page carries the
	// target's Host and its own Origin.
	PublicURL string

	// ClientIP is the key the rate limiter counts against. Required.
	ClientIP func(*http.Request) string

	// Allow reports whether this address may send another event, and how many
	// seconds until it may. Required.
	Allow func(ip string) (bool, int)

	// Logger records refused bodies. Nil uses the default.
	Logger *slog.Logger
}

// Proxy forwards one browser ingestion request to PostHog.
//
// The project key never leaves the server: rewriteProxyKey deletes every field
// PostHog reads a token from and substitutes this deployment's, so a page holds
// no key and a request writes into this project or none.
//
// No session is required. The sign-up funnel is measured before one exists, so
// what bounds the route is the path allowlist, the size caps, the per-address
// rate limit and the substitution.
type Proxy struct {
	cfg ProxyConfig
}

// NewProxy returns a handler for ProxyPathPrefix + "{rest...}".
//
// It panics where ClientIP or Allow is nil. Both are read on the first
// request, so a deployment missing either serves until somebody posts an event
// and then panics inside a handler.
func NewProxy(cfg ProxyConfig) *Proxy {
	if cfg.ClientIP == nil {
		panic("analytics: Proxy needs a ClientIP")
	}
	if cfg.Allow == nil {
		panic("analytics: Proxy needs an Allow")
	}
	if cfg.Host == "" {
		cfg.Host = DefaultEndpoint
	}
	return &Proxy{cfg: cfg}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Key == "" {
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
	if origin := r.Header.Get("Origin"); origin != "" && !p.sameOrigin(origin, r) {
		http.Error(w, "", http.StatusForbidden)
		return
	}
	// Resolved once. The address the limiter counted and the address forwarded
	// are the same value by construction.
	client := p.cfg.ClientIP(r)
	if ok, retry := p.cfg.Allow(client); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	upstream := strings.Trim(strings.TrimPrefix(r.URL.Path, ProxyPathPrefix), "/")
	if !proxyUpstreamPaths[upstream] {
		http.NotFound(w, r)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, proxyMaxBody+1))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	if len(raw) > proxyMaxBody {
		http.Error(w, "", http.StatusRequestEntityTooLarge)
		return
	}

	body, err := rewriteProxyKey(raw, p.cfg.Key)
	switch {
	case err == errTooManyEvents:
		http.Error(w, "", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		p.logger().Warn("analytics body refused", "err", err)
		http.Error(w, "", http.StatusUnsupportedMediaType)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(p.cfg.Host, "/")+"/"+upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// PostHog routes on Host and answers 401 when it receives this origin's.
	req.Host = HostOf(p.cfg.Host)
	req.Header.Set("X-Forwarded-For", client)

	resp, err := proxyClient.Do(req)
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

// sameOrigin reports whether an Origin header names this deployment.
func (p *Proxy) sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if p.cfg.PublicURL == "" {
		return u.Host == r.Host
	}
	pub, err := url.Parse(p.cfg.PublicURL)
	if err != nil || pub.Host == "" {
		return false
	}
	return u.Host == pub.Host
}

func (p *Proxy) logger() *slog.Logger {
	if p.cfg.Logger != nil {
		return p.cfg.Logger
	}
	return slog.Default()
}

// rewriteProxyKey removes every field PostHog reads a token from and sets
// api_key to this deployment's, so a request writes into this project or none.
//
// Values other than those fields are re-encoded from their original bytes, so
// number precision and key order inside them survive.
func rewriteProxyKey(raw []byte, key string) ([]byte, error) {
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
		if len(events) > proxyMaxBatch {
			return nil, errTooManyEvents
		}
		for _, e := range events {
			var one map[string]json.RawMessage
			if err := json.Unmarshal(e, &one); err != nil {
				return nil, err
			}
			if err := checkProxyEvent(one); err != nil {
				return nil, err
			}
		}
	}
	// The single-event shape carries the same keys at the top level, and a
	// batch body may carry them beside the array.
	if err := checkProxyEvent(body); err != nil {
		return nil, err
	}
	if props, ok := body["properties"]; ok {
		body["properties"] = stripToken(props)
	}
	for _, f := range proxyTokenFields {
		delete(body, f)
	}
	quoted, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	body["api_key"] = quoted
	return json.Marshal(body)
}

// checkProxyEvent reports why one event may not be forwarded.
//
// It reads and does not rewrite: an event that carries a refused key is
// refused whole, because dropping the key would forward the rest of what the
// caller wanted and record it as delivered.
func checkProxyEvent(e map[string]json.RawMessage) error {
	if raw, ok := e["event"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return err
		}
		if strings.HasPrefix(name, "$") && !proxyEventNames[name] {
			return errEventRefused
		}
	}
	for _, f := range proxyPersonFields {
		if _, ok := e[f]; ok {
			return errPersonRefused
		}
	}

	props, ok := e["properties"]
	if !ok {
		return nil
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(props, &p); err != nil || p == nil {
		return nil
	}
	for _, f := range proxyPersonFields {
		if _, ok := p[f]; ok {
			return errPersonRefused
		}
	}
	return nil
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

type errProxy string

func (e errProxy) Error() string { return string(e) }

var (
	errTooManyEvents = errProxy("batch holds more events than the cap")
	errNotAnObject   = errProxy("body is not a JSON object")
	errEventRefused  = errProxy("event name is reserved and not one this route forwards")
	errPersonRefused = errProxy("event writes person properties, which this route does not forward")
)

// HostOf returns the authority of an absolute URL, port included.
func HostOf(rawURL string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// proxyClient bounds concurrency to PostHog. The default transport caps
// neither connections nor idle connections per host, so an inbound burst opens
// one each.
var proxyClient = &http.Client{
	Timeout: proxyTimeout,
	Transport: &http.Transport{
		MaxConnsPerHost:     32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}
