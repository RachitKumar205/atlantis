// Package server is Atlantis Cloud's HTTP surface: the JWKS document every
// console verifies against, the account flows (sign up, verify an address,
// request and complete a password reset), sign-in, OAuth identity linking, and
// the organisation routes behind them.
//
// Sign-in is two-legged — a password, then a TOTP second factor — with a
// pending cookie held between the legs. A correct password alone mints no
// session: it produces a pending login that must be completed by verifying a
// factor or enrolling one.
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

	// PublicURL is the base every emailed link is built from. Required, with no
	// default: a wrong value still produces working links, pointing at the
	// wrong host. Not derived from the Host header, which is caller-controlled.
	PublicURL string // CLOUD_PUBLIC_URL — required

	// OAuthRedirectBase is the base the provider callback is built from,
	// defaulting to PublicURL. Set only where the two must differ.
	//
	// Google refuses a redirect URI that is not HTTPS unless its host is a
	// loopback address, and refuses any host whose TLD is absent from the
	// public suffix list. The development host, atl-dev.test, fails both.
	//
	// Read from configuration and never from the request, for the reason on
	// redirectURI. Validated only when a provider is configured, so a
	// deployment with no OAuth is not held to a rule that cannot affect it.
	OAuthRedirectBase string // CLOUD_OAUTH_REDIRECT_BASE — defaults to CLOUD_PUBLIC_URL

	// ExtraOrigins are additional origins accepted on state-changing /api/*
	// routes, beyond PublicURL and the request's own Host.
	//
	// `vite dev` serves the page from localhost:5173 and proxies /api with
	// changeOrigin, which rewrites Host to Cloud's address while the browser
	// still sends the page's origin. Neither PublicURL nor Host then matches
	// and every write is refused.
	//
	// Not inferred from the request: allowing any loopback origin would accept
	// cross-site writes from anything else on the same host.
	ExtraOrigins []string // CLOUD_EXTRA_ORIGINS — development only

	// Cloud refuses to start unless exactly one mail transport is completely
	// configured, or MailDev is set. See validateMail.
	ResendAPIKey string // CLOUD_RESEND_API_KEY

	// MailFrom is the sender for whichever transport is in use. Accepts
	// `Name <addr>` as well as a bare address. Falls back to CLOUD_SMTP_FROM.
	MailFrom string // CLOUD_MAIL_FROM

	// MailDev selects the logging mailer, which prints messages instead of
	// sending them. It is never the default: mail.Log writes password-reset
	// links to the log.
	MailDev bool // CLOUD_MAIL_DEV

	// SMTP, for a deployment with its own relay. There is no SMTPFrom field;
	// CLOUD_SMTP_FROM feeds MailFrom above.
	SMTPAddr     string // CLOUD_SMTP_ADDR
	SMTPUser     string // CLOUD_SMTP_USER
	SMTPPassword string // CLOUD_SMTP_PASSWORD

	// CheckBreaches asks Have I Been Pwned whether a password has leaked.
	// Default true. The caller fails open when the API is unreachable and
	// counts how often that happens.
	CheckBreaches bool // CLOUD_HIBP_CHECK — default true

	// TrustProxy makes the rate limiter read X-Forwarded-For.
	//
	// Off by default: the header is spoofable, so the limiter can be reset per
	// request. Set it only behind a terminator that overwrites the header.
	TrustProxy bool // CLOUD_TRUST_PROXY

	// DataKeyset seals each account's TOTP secret in cloud.totp_secrets.
	//
	// Base64 Tink keyset, required, no default. A TOTP secret is recomputed to
	// be checked, so it is encrypted rather than hashed.
	//
	// No per-boot default: a key that dies with the process leaves every
	// enrolled factor unopenable after a restart, with the rows intact.
	DataKeyset string // CLOUD_DATA_KEY

	// CookieSecure sets the Secure flag on the session cookie. Default false so
	// http://localhost works; true once a TLS terminator sits in front.
	CookieSecure bool // CLOUD_COOKIE_SECURE

	// OAuth provider credentials, from each provider's developer console.
	//
	// A provider with neither value set is not registered, and its routes
	// answer 404. One of a pair without the other is refused at boot.
	GitHubClientID     string // CLOUD_GITHUB_CLIENT_ID
	GitHubClientSecret string // CLOUD_GITHUB_CLIENT_SECRET
	GoogleClientID     string // CLOUD_GOOGLE_CLIENT_ID
	GoogleClientSecret string // CLOUD_GOOGLE_CLIENT_SECRET

	// SendTimeout bounds a single mail send.
	SendTimeout time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Listen:     envOr("CLOUD_LISTEN", ":9500"),
		PGURL:      os.Getenv("CLOUD_PG_URL"),
		Issuer:     os.Getenv("CLOUD_ISSUER"),
		SigningKey: envOr("CLOUD_SIGNING_KEY", "./certs/cloud-signing-key.pem"),
		PublicURL:  strings.TrimRight(os.Getenv("CLOUD_PUBLIC_URL"), "/"),

		OAuthRedirectBase: strings.TrimRight(os.Getenv("CLOUD_OAUTH_REDIRECT_BASE"), "/"),
		ExtraOrigins:      splitOrigins(os.Getenv("CLOUD_EXTRA_ORIGINS")),
		ResendAPIKey:      os.Getenv("CLOUD_RESEND_API_KEY"),
		// CLOUD_SMTP_FROM is the fallback.
		MailFrom:      envOr("CLOUD_MAIL_FROM", os.Getenv("CLOUD_SMTP_FROM")),
		MailDev:       os.Getenv("CLOUD_MAIL_DEV") == "true",
		SMTPAddr:      os.Getenv("CLOUD_SMTP_ADDR"),
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

	// Parsed rather than trusted: a PublicURL that is not absolute produces
	// unfollowable links in every message this server sends.
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Config{}, fmt.Errorf("CLOUD_PUBLIC_URL must be an absolute URL "+
			"like https://cloud.atlantis.dev, got %q", c.PublicURL)
	}

	if err := c.validateMail(); err != nil {
		return Config{}, err
	}

	// Neither value set means the provider is off. Exactly one set fails at the
	// callback, after consent has already been granted at the provider.
	for _, p := range []struct{ idName, id, secretName, secret string }{
		{"CLOUD_GITHUB_CLIENT_ID", c.GitHubClientID, "CLOUD_GITHUB_CLIENT_SECRET", c.GitHubClientSecret},
		{"CLOUD_GOOGLE_CLIENT_ID", c.GoogleClientID, "CLOUD_GOOGLE_CLIENT_SECRET", c.GoogleClientSecret},
	} {
		switch {
		case p.id != "" && p.secret == "":
			return Config{}, fmt.Errorf("%s is set but %s is not: "+
				"a client id without its secret cannot complete a sign-in, and "+
				"the failure lands after consent has been granted",
				p.idName, p.secretName)
		case p.secret != "" && p.id == "":
			return Config{}, fmt.Errorf("%s is set but %s is not: "+
				"there is nothing to identify this deployment to the provider",
				p.secretName, p.idName)
		}
	}

	if err := c.validateOAuthRedirectBase(); err != nil {
		return Config{}, err
	}

	return c, nil
}

// OAuthRedirect is the base the provider callback is built from: the override
// when set, and PublicURL otherwise.
//
// A method rather than a value fixed during parsing, so a Config built
// directly resolves the same way as one read from the environment.
func (c Config) OAuthRedirect() string {
	if c.OAuthRedirectBase != "" {
		return c.OAuthRedirectBase
	}
	return c.PublicURL
}

// validateOAuthRedirectBase refuses a base a provider will not accept.
//
// Checked at boot rather than left to the provider, which reports it as
// redirect_uri_mismatch after consent has been granted, against a URI that
// does not appear in the request the operator can see.
//
// Only when a provider is configured: the value is inherited from PublicURL,
// and a deployment with no OAuth has no reason to satisfy a rule about it.
func (c Config) validateOAuthRedirectBase() error {
	if c.GitHubClientID == "" && c.GoogleClientID == "" {
		return nil
	}
	base := c.OAuthRedirect()
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("CLOUD_OAUTH_REDIRECT_BASE must be an absolute URL "+
			"like https://cloud.atlantis.dev, got %q", base)
	}
	if u.Scheme == "https" {
		return nil
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	return fmt.Errorf("the OAuth callback base is %q, which no provider will "+
		"accept: a redirect URI must be https unless its host is a loopback "+
		"address. It defaults to CLOUD_PUBLIC_URL; set CLOUD_OAUTH_REDIRECT_BASE "+
		"to override it for local development, e.g. http://localhost:30500",
		base)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitOrigins parses CLOUD_EXTRA_ORIGINS.
//
// Comma-separated, trailing slashes trimmed. Empty entries are dropped: an
// origin matching the empty string would undo sameOrigin's refusal of a
// missing header.
func splitOrigins(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimRight(strings.TrimSpace(part), "/")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// validateMail reports an error unless exactly one mail transport is fully
// configured, or MailDev is set.
//
// Address verification and password reset both depend on mail, and neither
// reports a send failure in its response, so the failure surfaces at start-up.
func (c Config) validateMail() error {
	resend := c.ResendAPIKey != ""
	smtp := c.SMTPAddr != ""

	if c.MailDev {
		// With both set, the logging mailer wins and nothing is delivered.
		if resend || smtp {
			return fmt.Errorf("CLOUD_MAIL_DEV is set alongside a real mail transport: " +
				"the logging mailer would win and nothing would be delivered, so " +
				"unset one of them")
		}
		return nil
	}

	switch {
	case resend && smtp:
		// Ambiguous rather than redundant: the error names the cost.
		return fmt.Errorf("CLOUD_RESEND_API_KEY and CLOUD_SMTP_ADDR are both set: " +
			"choose one, or a failed delivery gets investigated against the " +
			"transport that was not used")
	case !resend && !smtp:
		return fmt.Errorf("no mail transport is configured, so account verification " +
			"and password reset cannot be delivered.\n\n" +
			"Set CLOUD_RESEND_API_KEY with CLOUD_MAIL_FROM, or CLOUD_SMTP_ADDR with " +
			"CLOUD_SMTP_FROM.\n" +
			"For local development, CLOUD_MAIL_DEV=true prints messages to the log " +
			"instead of sending them")
	}

	// A transport with no sender produces a message every receiver rejects,
	// which presents as mail not arriving.
	if c.MailFrom == "" {
		which, with := "CLOUD_RESEND_API_KEY", "CLOUD_MAIL_FROM"
		if smtp {
			which, with = "CLOUD_SMTP_ADDR", "CLOUD_MAIL_FROM (or CLOUD_SMTP_FROM)"
		}
		return fmt.Errorf("%s is set but %s is not: "+
			"a message with no sender is refused by every receiver", which, with)
	}
	return nil
}
