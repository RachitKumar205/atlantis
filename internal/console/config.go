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
	Listen        string // CONSOLE_LISTEN — default :3000
	PGURL         string // CONSOLE_PG_URL — required
	ATLEndpoint   string // ATL_ENDPOINT — default localhost:9090
	ATLTLSCert    string // ATL_TLS_CERT
	ATLTLSKey     string // ATL_TLS_KEY
	ATLTLSCA      string // ATL_TLS_CA
	SessionSecret string // CONSOLE_SESSION_SECRET — required, ≥32 chars
	CookieSecure  bool   // CONSOLE_COOKIE_SECURE — default false
	HealthListen  string // ATL_HEALTH_LISTEN — atlantis health HTTP addr, default :8081
	GitHubToken   string // GITHUB_TOKEN — optional; PR flow requires it
	SignerAddr    string // ATL_SIGNER_ADDR — optional; cert issuance requires it

	// AuditRetentionDays controls how long operator-action audit rows
	// are kept before their monthly partition is DROPped by the
	// background worker. 0 disables retention entirely (kept forever);
	// the default of 365 covers the SOC 2 / PCI-DSS minimum.
	AuditRetentionDays int

	// SandboxPerUserLimit caps how many active sandboxes one user can
	// hold at once. 4th boot returns 429. Default 3; tune via
	// SANDBOX_PER_USER_LIMIT.
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
		ATLEndpoint:   envOr("ATL_ENDPOINT", "localhost:9090"),
		ATLTLSCert:    os.Getenv("ATL_TLS_CERT"),
		ATLTLSKey:     os.Getenv("ATL_TLS_KEY"),
		ATLTLSCA:      os.Getenv("ATL_TLS_CA"),
		SessionSecret: os.Getenv("CONSOLE_SESSION_SECRET"),
		CookieSecure:  os.Getenv("CONSOLE_COOKIE_SECURE") == "true",
		HealthListen:  envOr("ATL_HEALTH_LISTEN", "localhost:8081"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		SignerAddr:    os.Getenv("ATL_SIGNER_ADDR"),

		AuditRetentionDays: envInt("CONSOLE_AUDIT_RETENTION_DAYS", 365),

		SandboxPerUserLimit: envInt("SANDBOX_PER_USER_LIMIT", 3),
		SandboxTTL:          envDuration("SANDBOX_TTL", 30*time.Minute),
	}
	if c.PGURL == "" {
		return Config{}, fmt.Errorf("CONSOLE_PG_URL is required")
	}
	if c.SessionSecret == "" {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET is required")
	}
	if len(c.SessionSecret) < 32 {
		return Config{}, fmt.Errorf("CONSOLE_SESSION_SECRET must be at least 32 characters")
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
