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

	// DataKeyset seals each account's TOTP secret in cloud.totp_secrets.
	//
	// Base64 Tink keyset, required, no default — the same value shape and the
	// same package the console uses for organisation private keys. A second
	// factor has to be recomputed to be checked, so unlike the argon2id
	// password hash beside it, it cannot be hashed and must be encrypted.
	//
	// Generating one per boot would encrypt every enrolled factor under a key
	// that dies with the process, which presents as every account being locked
	// out after a restart with the rows intact and unopenable.
	DataKeyset string // CLOUD_DATA_KEY

	// CookieSecure sets the Secure flag on the session cookie. Default false so
	// http://localhost works; true once a TLS terminator sits in front.
	CookieSecure bool // CLOUD_COOKIE_SECURE

	// OAuth provider credentials, from each provider's developer console.
	//
	// A provider with neither value set is not registered at all, and its
	// routes answer 404. That is deliberate: the alternative is a route that
	// exists, accepts the request, and fails at the redirect with an error
	// about a missing client id — which reads to whoever hits it as a broken
	// deployment rather than an unconfigured feature.
	//
	// One of a pair without the other IS an error, at boot. It means somebody
	// intended to configure the provider and did half of it, and finding that
	// out at the first sign-in attempt is worse than finding it out at start-up.
	GitHubClientID     string // CLOUD_GITHUB_CLIENT_ID
	GitHubClientSecret string // CLOUD_GITHUB_CLIENT_SECRET
	GoogleClientID     string // CLOUD_GOOGLE_CLIENT_ID
	GoogleClientSecret string // CLOUD_GOOGLE_CLIENT_SECRET

	// SignInAppURL is where a finished OAuth callback sends the browser.
	//
	// Optional, and empty until a Cloud sign-in app exists. With no value the
	// callback answers with a plain page naming the next step, which is what
	// makes these routes usable — and testable — before there is a frontend.
	// Setting it turns the same handler into a redirect without a code change.
	SignInAppURL string // CLOUD_SIGNIN_APP_URL

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
		DataKeyset:    os.Getenv("CLOUD_DATA_KEY"),
		CookieSecure:  os.Getenv("CLOUD_COOKIE_SECURE") == "true",
		SendTimeout:   10 * time.Second,

		GitHubClientID:     os.Getenv("CLOUD_GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("CLOUD_GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("CLOUD_GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("CLOUD_GOOGLE_CLIENT_SECRET"),
		SignInAppURL:       strings.TrimRight(os.Getenv("CLOUD_SIGNIN_APP_URL"), "/"),
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

	if c.DataKeyset == "" {
		return Config{}, fmt.Errorf(
			"CLOUD_DATA_KEY is required.\n\n" +
				"Each account's second-factor secret is encrypted with it. A second " +
				"factor has to be recomputed to be checked, so it cannot be hashed the " +
				"way a password is — without this keyset Cloud can neither enrol a " +
				"factor nor verify one.\n\n" +
				"For local development: `make dev-cloud-data-key` prints one to export.")
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

	// Half a provider is a mistake, not a choice. Neither value set means the
	// provider is off, which is a supported state; exactly one set means
	// somebody meant to turn it on, and the first sign of the missing half
	// would otherwise be a user who has already consented at GitHub.
	for _, p := range []struct{ idName, id, secretName, secret string }{
		{"CLOUD_GITHUB_CLIENT_ID", c.GitHubClientID, "CLOUD_GITHUB_CLIENT_SECRET", c.GitHubClientSecret},
		{"CLOUD_GOOGLE_CLIENT_ID", c.GoogleClientID, "CLOUD_GOOGLE_CLIENT_SECRET", c.GoogleClientSecret},
	} {
		switch {
		case p.id != "" && p.secret == "":
			return Config{}, fmt.Errorf("%s is set but %s is not: "+
				"a client id without its secret cannot complete a sign-in, and the "+
				"failure would land on somebody who had already granted access",
				p.idName, p.secretName)
		case p.secret != "" && p.id == "":
			return Config{}, fmt.Errorf("%s is set but %s is not: "+
				"there is nothing to identify this deployment to the provider",
				p.secretName, p.idName)
		}
	}

	// Same reasoning as CLOUD_PUBLIC_URL, and it matters more here: this one is
	// a redirect target. A value that does not parse would send every finished
	// sign-in to a Location header nothing can follow.
	if c.SignInAppURL != "" {
		u, err := url.Parse(c.SignInAppURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("CLOUD_SIGNIN_APP_URL must be an absolute URL "+
				"like https://cloud.atlantis.dev/signin, got %q", c.SignInAppURL)
		}
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
