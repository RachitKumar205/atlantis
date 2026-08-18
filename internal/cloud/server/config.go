// Package server is Atlantis Cloud's HTTP surface.
//
// # What it serves today
//
// The JWKS document every console verifies against, and the account flows that
// do not authenticate anybody: sign up, verify an address, request a password
// reset, complete one.
//
// # What it deliberately does not serve
//
// Sign-in. Cloud's sign-in is two-legged — a password, then a second factor —
// and the second factor does not exist yet. A route that issued a session on a
// password alone would be the exact posture this product refuses, sitting in
// the tree looking finished; one that could not complete would be untestable.
// So it lands with the factor that gates it.
//
// Nothing here creates a session. Completing a password reset sets the password
// and sends the user to sign in, which is why this package has no cookie
// handling at all.
package server

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is Cloud's configuration, from the environment.
type Config struct {
	Listen string // CLOUD_LISTEN — default :9500

	// PGURL is Cloud's own database. Separate from the console's; see
	// migrations/cloud/0001.
	PGURL string // CLOUD_PG_URL — required

	// Issuer is the iss claim Cloud mints and every console compares for exact
	// equality. SigningKey is where the private half lives.
	Issuer     string // CLOUD_ISSUER — required
	SigningKey string // CLOUD_SIGNING_KEY

	// PublicURL is the base every emailed link is built from.
	//
	// Required, with no default, and the reason is worth stating: this string
	// becomes a URL in an email that asks somebody to prove who they are. A
	// wrong value does not fail — it sends every user a working link to the
	// wrong host, which is a credible phishing primitive if that host is not
	// ours and a broken flow if it is. Guessing it from the Host header would
	// be worse, because the Host header is attacker-controlled.
	PublicURL string // CLOUD_PUBLIC_URL — required

	// SMTP. All optional: with no address, Cloud logs messages instead of
	// sending them and warns on every one. See internal/cloud/mail.
	SMTPAddr     string // CLOUD_SMTP_ADDR
	SMTPFrom     string // CLOUD_SMTP_FROM
	SMTPUser     string // CLOUD_SMTP_USER
	SMTPPassword string // CLOUD_SMTP_PASSWORD

	// CheckBreaches asks Have I Been Pwned whether a password has leaked.
	//
	// Default on. It is a call to a third party during sign-up, so the caller
	// fails open when it cannot be reached — a password is accepted rather than
	// sign-up depending on somebody else's uptime — and counts how often that
	// happens.
	CheckBreaches bool // CLOUD_HIBP_CHECK — default true

	// TrustProxy makes the rate limiter read X-Forwarded-For.
	//
	// Off by default because the header is trivially spoofable, and a limiter
	// keyed on a spoofable value is a limiter an attacker resets per request.
	// Turn it on only when something you control terminates in front.
	TrustProxy bool // CLOUD_TRUST_PROXY

	// SendTimeout bounds a single mail send.
	SendTimeout time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Listen:        envOr("CLOUD_LISTEN", ":9500"),
		PGURL:         os.Getenv("CLOUD_PG_URL"),
		Issuer:        os.Getenv("CLOUD_ISSUER"),
		SigningKey:    envOr("CLOUD_SIGNING_KEY", "./certs/cloud-signing-key.pem"),
		PublicURL:     strings.TrimRight(os.Getenv("CLOUD_PUBLIC_URL"), "/"),
		SMTPAddr:      os.Getenv("CLOUD_SMTP_ADDR"),
		SMTPFrom:      os.Getenv("CLOUD_SMTP_FROM"),
		SMTPUser:      os.Getenv("CLOUD_SMTP_USER"),
		SMTPPassword:  os.Getenv("CLOUD_SMTP_PASSWORD"),
		CheckBreaches: os.Getenv("CLOUD_HIBP_CHECK") != "false",
		TrustProxy:    os.Getenv("CLOUD_TRUST_PROXY") == "true",
		SendTimeout:   10 * time.Second,
	}

	for _, v := range []struct{ name, val string }{
		{"CLOUD_PG_URL", c.PGURL},
		{"CLOUD_ISSUER", c.Issuer},
		{"CLOUD_PUBLIC_URL", c.PublicURL},
	} {
		if v.val == "" {
			return Config{}, fmt.Errorf("%s is required", v.name)
		}
	}

	// Parsed rather than trusted. A PublicURL that is not an absolute URL
	// produces links nobody can follow, and the first person to find out is a
	// user who cannot verify their address.
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Config{}, fmt.Errorf("CLOUD_PUBLIC_URL must be an absolute URL "+
			"like https://cloud.atlantis.dev, got %q", c.PublicURL)
	}

	// A configured mail server with no sender address produces a message every
	// receiver rejects, which presents as "email is not arriving" rather than
	// as a missing setting.
	if c.SMTPAddr != "" && c.SMTPFrom == "" {
		return Config{}, fmt.Errorf("CLOUD_SMTP_FROM is required when CLOUD_SMTP_ADDR is set: " +
			"a message with no sender is refused by every receiver")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
