package console

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// Config holds all console BFF configuration, sourced from environment
// variables.
//
// One console serves many organisations, each behind its own CA, so an
// atlantis address and the credentials for it live in console.orgs rather than
// in this struct. An unregistered organisation is refused: a process-wide
// default endpoint would route one into another organisation's atlantis with
// every page rendering. tide and tidectl read ATL_ENDPOINT for themselves.
type Config struct {
	Listen string // CONSOLE_LISTEN — default :3000
	PGURL  string // CONSOLE_PG_URL — required
	// DataKeyset seals the per-organisation private keys in console.orgs.
	//
	// Base64-encoded Tink keyset, required, no default. A console that
	// generated its own on first boot would encrypt every organisation's
	// credentials under a key that dies with the container — which presents as
	// every organisation being unreachable after a restart, with the rows
	// intact and permanently unopenable.
	DataKeyset string // CONSOLE_DATA_KEY

	// PostHogKey is the analytics project key, under Cloud's name because the
	// console reports about the same organisations and the same people Cloud
	// does. Empty is analytics.Discard.
	PostHogKey  string // CLOUD_POSTHOG_KEY
	PostHogHost string // CLOUD_POSTHOG_HOST

	SessionSecret string // CONSOLE_SESSION_SECRET — required, ≥32 chars
	CookieSecure  bool   // CONSOLE_COOKIE_SECURE — default false

	// Enrolment: how a machine gets a client certificate.
	//
	// Optional as a group. A console with some of it set refuses to start,
	// because half-configured states fail late and quietly. All empty turns
	// enrolment off, and the token route says so rather than answering 404.
	SignerAddr string // ATL_SIGNER_ADDR — the signer's https:// base URL
	// The console's own client credentials to the signer. CN must be in the
	// signer's SIGNER_ALLOWED_CLIENT_CNS.
	SignerCert string // ATL_SIGNER_CERT
	SignerKey  string // ATL_SIGNER_KEY
	SignerCA   string // ATL_SIGNER_CA — verifies the signer, not the callers

	// The enrolment listener, separate from Listen: Listen sits behind a TLS
	// terminator the console does not control and cannot inspect a client
	// certificate, while this one terminates its own TLS and requests one. It
	// carries two routes and never the console's API or SPA.
	//
	// There is no client-CA setting. Each organisation has its own authority
	// and one pool cannot verify them all, so handleRenew does the check.
	EnrollListen  string // CONSOLE_ENROLL_LISTEN
	EnrollTLSCert string // CONSOLE_ENROLL_TLS_CERT
	EnrollTLSKey  string // CONSOLE_ENROLL_TLS_KEY

	// EnrollPublicURL is the address a machine reaches the listener at, which
	// EnrollListen is not: that is a bind address, often `:3443`.
	//
	// Not derived from the Host header, which is caller-controlled. A wrong
	// value does not fail; it prints an enrolment command carrying a live token
	// and pointing elsewhere.
	//
	// Without it the console still enrols but cannot print a runnable command.
	EnrollPublicURL string // CONSOLE_ENROLL_PUBLIC_URL

	// Who this console accepts identity from. All three required, no defaults.
	//
	// CloudIssuer and CloudAudience are compared for exact equality, and an
	// empty expected value skips the check, so a console missing either would
	// run and accept assertions from any issuer for any console.
	CloudIssuer   string // CLOUD_ISSUER — the iss value to require
	CloudAudience string // CLOUD_AUDIENCE — this console's own name, required in aud
	CloudJWKSURL  string // CLOUD_JWKS_URL — where the issuer publishes its keys

	// AuditRetentionDays controls how long operator-action audit rows
	// are kept before their monthly partition is DROPped by the
	// background worker. 0 disables retention entirely (kept forever);
	// the default of 365 covers the SOC 2 / PCI-DSS minimum.
	AuditRetentionDays int

	// SandboxPerUserLimit caps how many active sandboxes one user can hold at
	// once; the (N+1)th boot returns 429. Default 100, set by
	// SANDBOX_PER_USER_LIMIT, which is generous because a fanned-out agent
	// workflow boots dozens of forks per task.
	SandboxPerUserLimit int

	// SandboxTTL is the idle window after which the TTL janitor evicts
	// a sandbox. Default 30 minutes; tune via SANDBOX_TTL (Go duration
	// string, e.g. "10s", "2h").
	SandboxTTL time.Duration

	// MetricsListen is where /metrics is served, on its own listener.
	// Default 127.0.0.1:9103, set by CONSOLE_METRICS_LISTEN. Loopback
	// because these series carry the customer list and each customer's
	// schema version; see newMetricsServer.
	MetricsListen string

	// FleetPollInterval is the gap between sweeps of every registered
	// organisation. Default 5 minutes, set by CONSOLE_FLEET_POLL_INTERVAL.
	FleetPollInterval time.Duration

	// FleetOrgTimeout bounds one organisation within a sweep, which is what
	// keeps a wedged tenant from consuming it. Default 15 seconds, set by
	// CONSOLE_FLEET_ORG_TIMEOUT.
	FleetOrgTimeout time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Listen:        envOr("CONSOLE_LISTEN", ":3000"),
		PGURL:         os.Getenv("CONSOLE_PG_URL"),
		DataKeyset:    os.Getenv("CONSOLE_DATA_KEY"),
		PostHogKey:    os.Getenv("CLOUD_POSTHOG_KEY"),
		PostHogHost:   envOr("CLOUD_POSTHOG_HOST", analytics.DefaultEndpoint),
		SessionSecret: os.Getenv("CONSOLE_SESSION_SECRET"),
		CookieSecure:  os.Getenv("CONSOLE_COOKIE_SECURE") == "true",
		SignerAddr:    os.Getenv("ATL_SIGNER_ADDR"),
		SignerCert:    os.Getenv("ATL_SIGNER_CERT"),
		SignerKey:     os.Getenv("ATL_SIGNER_KEY"),
		SignerCA:      os.Getenv("ATL_SIGNER_CA"),

		EnrollListen:    os.Getenv("CONSOLE_ENROLL_LISTEN"),
		EnrollTLSCert:   os.Getenv("CONSOLE_ENROLL_TLS_CERT"),
		EnrollTLSKey:    os.Getenv("CONSOLE_ENROLL_TLS_KEY"),
		EnrollPublicURL: os.Getenv("CONSOLE_ENROLL_PUBLIC_URL"),

		CloudIssuer:   os.Getenv("CLOUD_ISSUER"),
		CloudAudience: os.Getenv("CLOUD_AUDIENCE"),
		CloudJWKSURL:  os.Getenv("CLOUD_JWKS_URL"),

		AuditRetentionDays: envInt("CONSOLE_AUDIT_RETENTION_DAYS", 365),

		SandboxPerUserLimit: envInt("SANDBOX_PER_USER_LIMIT", 100),
		SandboxTTL:          envDuration("SANDBOX_TTL", 30*time.Minute),

		MetricsListen:     envOr("CONSOLE_METRICS_LISTEN", "127.0.0.1:9103"),
		FleetPollInterval: envDuration("CONSOLE_FLEET_POLL_INTERVAL", defaultFleetPollInterval),
		FleetOrgTimeout:   envDuration("CONSOLE_FLEET_ORG_TIMEOUT", defaultFleetOrgTimeout),
	}
	if c.PGURL == "" {
		return Config{}, fmt.Errorf("CONSOLE_PG_URL is required")
	}
	// See the field comments: an unset issuer or audience does not weaken the
	// check, it removes it.
	for _, v := range []struct{ name, val string }{
		{"CLOUD_ISSUER", c.CloudIssuer},
		{"CLOUD_AUDIENCE", c.CloudAudience},
		{"CLOUD_JWKS_URL", c.CloudJWKSURL},
	} {
		if v.val == "" {
			return Config{}, fmt.Errorf("%s is required: the console has no local accounts and "+
				"verifies every sign-in against Atlantis Cloud", v.name)
		}
	}
	if err := rejectRetiredEnrolmentEnv(); err != nil {
		return Config{}, err
	}
	if err := c.validateEnrollment(); err != nil {
		return Config{}, err
	}
	// Only when a key is set, so a deployment with analytics off is not held to
	// a rule that cannot affect it.
	if c.PostHogKey != "" {
		if err := analytics.CheckEndpoint("CLOUD_POSTHOG_HOST", c.PostHogHost); err != nil {
			return Config{}, err
		}
	}
	if c.SessionSecret == "" {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET is required")
	}
	if len(c.SessionSecret) < 32 {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET must be at least 32 characters")
	}
	// mTLS is required on every channel to atlantis, but the credentials live
	// in each organisation's console.orgs row, so there is no process-wide
	// certificate to validate here. dialOrg has no insecure branch, and an
	// organisation with no credentials is refused rather than dialled clear.
	//
	// What is validated here is the key those credentials are sealed with.
	if c.DataKeyset == "" {
		return Config{}, fmt.Errorf(
			"CONSOLE_DATA_KEY is required.\n\n" +
				"Each organisation's atlantis is reached with its own client " +
				"certificate, and the private keys are encrypted in console.orgs. " +
				"Without this keyset the console cannot read any of them, so every " +
				"organisation would be unreachable.\n\n" +
				"For local development: `make dev-data-key` prints one to export.")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envDuration parses an optional Go-duration env var. Malformed values
// fall through to the default with a stderr warning (same convention
// as envInt) so a typo doesn't silently change TTL behaviour.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console: %s=%q invalid (using default %s): %v\n", key, v, fallback, err)
		return fallback
	}
	return d
}

// envInt parses an optional non-negative integer env var. A malformed
// value falls through to the default with a stderr warning so a typo
// doesn't silently disable a feature (e.g. retention=0 by accident).
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "console: ignoring invalid %s=%q (using default %d)\n", key, v, fallback)
		return fallback
	}
	return n
}

// EnrollmentEnabled reports whether this console can issue caller certificates.
//
// validateEnrollment refuses the half-configured states, so one field answers
// for the whole feature. That field is the LISTENER, not a signer address.
//
// The listener is the part that cannot be resolved per organisation: without it
// a machine has nowhere to redeem a token, whatever signer the organisation
// has. signerFor answers which signer, per organisation, later.
func (c Config) EnrollmentEnabled() bool { return c.EnrollListen != "" }

// FallbackSignerConfigured reports whether this console has a process-wide
// signer to fall back on for organisations that carry none of their own.
//
// Expected to be false in a hosted deployment, where every organisation has its
// own. `make dev-signer` is what it exists for.
func (c Config) FallbackSignerConfigured() bool { return c.SignerAddr != "" }

// rejectRetiredEnrolmentEnv refuses to start when a removed setting is still
// set, rather than booting with it inert.
//
// Same shape as cmd/server's rejectRetiredAuthzEnv. CONSOLE_ENROLL_CLIENT_CA
// named the authority permitted to renew; starting with it present and unread
// would leave a restriction believed to be in force that nothing applies.
func rejectRetiredEnrolmentEnv() error {
	const name = "CONSOLE_ENROLL_CLIENT_CA"
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil
	}
	return fmt.Errorf(
		"%s is set to %q but is no longer read. Renewal now verifies a presented "+
			"certificate against the organisation's own CA, taken from console.orgs — "+
			"one pool could not verify callers from many authorities once each "+
			"organisation had its own. Unset the variable; nothing replaces it.",
		name, v)
}

// validateEnrollment refuses a half-configured enrolment at startup, because
// partial configuration is silent until used. See validateGroup.
//
// Two independent groups: the listener, and the fallback signer. Neither
// requires the other. One group would mean an enrolment listener could not run
// without a process-wide signer address, which a hosted deployment has none of,
// so the fallback would route an unregistered organisation into another
// tenant's signer.
func (c Config) validateEnrollment() error {
	// Both groups are reported together, so a wrong configuration costs one
	// restart rather than one per group.
	var problems []string
	for _, g := range []struct {
		what string
		set  map[string]string
	}{
		{"the enrolment listener", map[string]string{
			"CONSOLE_ENROLL_LISTEN":     c.EnrollListen,
			"CONSOLE_ENROLL_TLS_CERT":   c.EnrollTLSCert,
			"CONSOLE_ENROLL_TLS_KEY":    c.EnrollTLSKey,
			"CONSOLE_ENROLL_PUBLIC_URL": c.EnrollPublicURL,
		}},
		{"the fallback signer", map[string]string{
			"ATL_SIGNER_ADDR": c.SignerAddr,
			"ATL_SIGNER_CERT": c.SignerCert,
			"ATL_SIGNER_KEY":  c.SignerKey,
			"ATL_SIGNER_CA":   c.SignerCA,
		}},
	} {
		if err := c.validateGroup(g.what, g.set); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; and "))
}

// validateGroup refuses a group that is partly filled in.
//
// The states between fail late and quietly: a signer address with no client
// certificate produces a handshake failure reported as "signer unreachable",
// and an enrolment listener with no key does not listen at all.
//
// CONSOLE_ENROLL_PUBLIC_URL is in the listener group.
func (c Config) validateGroup(what string, set map[string]string) error {
	var missing []string
	filled := 0
	for name, v := range set {
		if v == "" {
			missing = append(missing, name)
			continue
		}
		filled++
	}
	if filled == 0 || len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%s is half-configured: %s %s unset. "+
		"Set all of them, or none of them to leave it off",
		what, strings.Join(missing, ", "), plural(len(missing), "is", "are"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
