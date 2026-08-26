package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Resend sends through Resend's HTTP API. APIKey and From are required.
// Preferred over Resend's SMTP endpoint, which has no server-side delivery log.
type Resend struct {
	// APIKey authenticates as a Bearer token.
	APIKey string

	// From is the sender. Resend accepts a bare address or `Name <addr>`.
	From string

	// Endpoint is the API base. Empty means resendEndpoint.
	Endpoint string

	// Timeout bounds the request. Zero means 10s. Send runs inside a sign-up
	// request, so this bounds that request too.
	Timeout time.Duration

	// Client overrides the HTTP client. Nil builds one from Timeout.
	Client *http.Client

	// Logger records the message id of a successful send. Nil uses the default.
	Logger *slog.Logger
}

const resendEndpoint = "https://api.resend.com/emails"

// Send delivers one message.
func (r *Resend) Send(ctx context.Context, to, subject, body string) error {
	// This path does not call compose, which holds the same check for SMTP.
	// Resend builds the message headers from these three fields.
	if err := checkHeaderFields(r.From, to, subject); err != nil {
		return err
	}
	if r.APIKey == "" {
		return errors.New("mail: no Resend API key")
	}
	if r.From == "" {
		// Checked here so the error names the setting rather than the field.
		return errors.New("mail: no sender address configured")
	}

	payload, err := json.Marshal(map[string]string{
		"from": r.From, "to": to, "subject": subject,
		// Resend derives a text part from html and not the reverse, so sending
		// text leaves nothing to be generated.
		"text": body,
	})
	if err != nil {
		return fmt.Errorf("mail: encode request: %w", err)
	}

	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = resendEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// net/http retries a request whose connection died before any bytes were
	// written, and bytes.Reader supplies GetBody, so this POST is eligible.
	req.Header.Set("Idempotency-Key", idempotencyKey(r.From, to, subject, body))

	client := r.Client
	if client == nil {
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded so the remote end cannot choose this process's memory use.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("mail: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return resendError(resp.StatusCode, raw)
	}

	var ok struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil || ok.ID == "" {
		// A 200 without an id is not a send failure, but nothing can be looked
		// up in Resend afterwards.
		r.logger().Warn("Resend accepted the message but returned no id",
			"to", to, "subject", subject)
		return nil
	}
	r.logger().Info("sent mail", "id", ok.ID, "to", to, "subject", subject)
	return nil
}

// idempotencyKey returns the Idempotency-Key for one message. Transmitting the
// same message twice delivers once; two messages differing only in body get
// different keys.
//
// SHA-256 hex is 64 characters, within Resend's 256-character limit. Resend
// expires keys after 24 hours. The NUL separator keeps field boundaries
// unambiguous.
func idempotencyKey(from, to, subject, body string) string {
	sum := sha256.Sum256([]byte(from + "\x00" + to + "\x00" + subject + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func (r *Resend) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// resendError maps a non-200 response to an error naming its cause. Resend
// returns 403 with name "validation_error" for an unverified sending domain,
// which is a DNS problem and is called out as one.
func resendError(status int, body []byte) error {
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)

	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		// Not JSON: an intermediary's error page. The read above allows 64KB,
		// and this ends up in a log line.
		detail = truncate(strings.TrimSpace(string(body)), 200)
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("mail: Resend refused the API key (%s): %s", e.Name, detail)
	case http.StatusForbidden:
		return fmt.Errorf("mail: Resend refused the sender (%s): %s\n\n"+
			"A new sending domain has to be verified in Resend before anything "+
			"will send from it — check its DNS records", e.Name, detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("mail: Resend rate limit (%s): %s", e.Name, detail)
	default:
		return fmt.Errorf("mail: Resend returned %d (%s): %s", status, e.Name, detail)
	}
}

// truncate shortens s to at most n bytes and marks that it was cut. The cut
// walks back to a rune boundary: s is arbitrary bytes from a remote server, and
// slicing UTF-8 at a byte offset leaves a broken rune.
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
