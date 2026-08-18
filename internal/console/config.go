package console

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all console BFF configuration, sourced from environment
// variables.
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

	// There is deliberately no ATL_ENDPOINT, ATL_TLS_CERT/KEY/CA or
	// ATL_HEALTH_LISTEN here any more.
	//
	// One console serves many organisations, each with its own atlantis behind
	// its own CA, so an address and a certificate are properties of an
	// organisation rather than of the process. They live in console.orgs and
	// are registered by `cloud org register`.
	//
	// Removed rather than kept as a fallback, which is the whole point: an
	// organisation nobody registered is refused. A default endpoint would mean
	// one missing row silently routes an unprovisioned organisation into
	// somebody else's atlantis, and every page would render.
	//
	// The env var names stay meaningful elsewhere — tide and tidectl still read
	// ATL_ENDPOINT for their own connections. Only the console stopped.
	SessionSecret string // CONSOLE_SESSION_SECRET — required, ≥32 chars
	CookieSecure  bool   // CONSOLE_COOKIE_SECURE — default false
	SignerAddr    string // ATL_SIGNER_ADDR — optional; cert issuance requires it

	// Who this console accepts identity from. All three are required and
	// none has a default.
	//
	// A default would be the wrong kind of convenience here. CloudIssuer and
	// CloudAudience are compared for exact equality, and an empty expected
	// value means the corresponding check does not run — so a console started
	// with either one missing would not fail, it would run and accept
	// assertions from any issuer, for any console. The failure has to happen
	// at startup because it cannot be noticed afterwards.
	CloudIssuer   string // CLOUD_ISSUER — the iss value to require
	CloudAudience string // CLOUD_AUDIENCE — this console's own name, required in aud
	CloudJWKSURL  string // CLOUD_JWKS_URL — where the issuer publishes its keys

	// AuditRetentionDays controls how long operator-action audit rows
	// are kept before their monthly partition is DROPped by the
	// background worker. 0 disables retention entirely (kept forever);
	// the default of 365 covers the SOC 2 / PCI-DSS minimum.
	AuditRetentionDays int

	// SandboxPerUserLimit caps how many active sandboxes one user can
	// hold at once. (N+1)th boot returns 429. Default 100; tune via
	// SANDBOX_PER_USER_LIMIT. The default is intentionally generous —
	// fanned-out agent workflows boot dozens of forks per task.
	SandboxPerUserLimit int

	// SandboxTTL is the idle window after which the TTL janitor evicts
	// a sandbox. Default 30 minutes; tune via SANDBOX_TTL (Go duration
	// string, e.g. "10s", "2h").
	SandboxTTL time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Listen:        envOr("CONSOLE_LISTEN", ":3000"),
		PGURL:         os.Getenv("CONSOLE_PG_URL"),
		DataKeyset:    os.Getenv("CONSOLE_DATA_KEY"),
		SessionSecret: os.Getenv("CONSOLE_SESSION_SECRET"),
		CookieSecure:  os.Getenv("CONSOLE_COOKIE_SECURE") == "true",
		SignerAddr:    os.Getenv("ATL_SIGNER_ADDR"),

		CloudIssuer:   os.Getenv("CLOUD_ISSUER"),
		CloudAudience: os.Getenv("CLOUD_AUDIENCE"),
		CloudJWKSURL:  os.Getenv("CLOUD_JWKS_URL"),

		AuditRetentionDays: envInt("CONSOLE_AUDIT_RETENTION_DAYS", 365),

		SandboxPerUserLimit: envInt("SANDBOX_PER_USER_LIMIT", 100),
		SandboxTTL:          envDuration("SANDBOX_TTL", 30*time.Minute),
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
	if c.SessionSecret == "" {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET is required")
	}
	if len(c.SessionSecret) < 32 {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET must be at least 32 characters")
	}
	// mTLS is still required on every channel to atlantis — what changed is
	// where the credentials come from.
	//
	// They used to be three process-wide file paths, which is why this function
	// used to demand them. One console now serves many organisations, each with
	// its own atlantis behind its own CA, so a certificate belongs to an
	// organisation and lives in its console.orgs row. There is no process-wide
	// certificate left to validate here, and no mode that skips mTLS: dialOrg
	// has no insecure branch, and an organisation with no credentials is
	// refused rather than dialled in the clear.
	//
	// What this function validates instead is the key those credentials are
	// sealed with, without which none of them can be read.
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
