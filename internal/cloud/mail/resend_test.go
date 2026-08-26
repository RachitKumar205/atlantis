package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// These pin the request the transport builds and what it makes of a response.
// A stub cannot prove that a message arrives; that needs a live send against a
// verified domain.

// resendStub captures the request and answers with what the test chooses.
type resendStub struct {
	srv *httptest.Server

	gotAuth   string
	gotIdem   string
	gotBody   map[string]string
	callCount int
}

func newResendStub(t *testing.T, status int, response string) *resendStub {
	t.Helper()
	s := &resendStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.callCount++
		s.gotAuth = r.Header.Get("Authorization")
		s.gotIdem = r.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(raw, &s.gotBody)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *resendStub) mailer() *Resend {
	return &Resend{
		APIKey:   "re_test_key",
		From:     "Atlantis <no-reply@tryatlantis.dev>",
		Endpoint: s.srv.URL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The request carries what the API requires.
func TestResendSendsTheDocumentedRequest(t *testing.T) {
	stub := newResendStub(t, http.StatusOK, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)

	if err := stub.mailer().Send(context.Background(),
		"ada@example.com", "Verify your email address", "Open this link"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if stub.gotAuth != "Bearer re_test_key" {
		t.Errorf("Authorization is %q, want a Bearer token", stub.gotAuth)
	}
	for field, want := range map[string]string{
		"from":    "Atlantis <no-reply@tryatlantis.dev>",
		"to":      "ada@example.com",
		"subject": "Verify your email address",
		"text":    "Open this link",
	} {
		if got := stub.gotBody[field]; got != want {
			t.Errorf("%s is %q, want %q", field, got, want)
		}
	}
	// Resend derives a text part from html and not the reverse, so sending html
	// would leave the plain-text version generated rather than written.
	if _, ok := stub.gotBody["html"]; ok {
		t.Error("the request carries html; these messages are plain text")
	}
}

// compose() holds this guard for SMTP, and this path never calls compose.
func TestResendRefusesHeaderInjection(t *testing.T) {
	cases := map[string]struct{ to, subject string }{
		"CRLF in recipient":  {"victim@example.com\r\nBcc: everyone@example.net", "Hello"},
		"LF in recipient":    {"victim@example.com\nBcc: everyone@example.net", "Hello"},
		"CR in recipient":    {"victim@example.com\rBcc: everyone@example.net", "Hello"},
		"CRLF in subject":    {"ada@example.com", "Hello\r\nBcc: everyone@example.net"},
		"newline in subject": {"ada@example.com", "Hello\nX-Injected: yes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newResendStub(t, http.StatusOK, `{"id":"x"}`)
			err := stub.mailer().Send(context.Background(), tc.to, tc.subject, "body")
			if !errors.Is(err, ErrHeaderInjection) {
				t.Errorf("got %v, want ErrHeaderInjection", err)
			}
			// Reaching the API at all means the field was transmitted,
			// whatever the response said.
			if stub.callCount != 0 {
				t.Error("the message was sent before the field was checked")
			}
		})
	}

	// Without this the table above passes against a Send that refuses
	// everything.
	stub := newResendStub(t, http.StatusOK, `{"id":"x"}`)
	if err := stub.mailer().Send(context.Background(),
		"ada@example.com", "Perfectly ordinary", "body"); err != nil {
		t.Errorf("an ordinary message was refused: %v", err)
	}
}

// A newline in the BODY is fine — it is content, not a header.
func TestResendAllowsNewlinesInTheBody(t *testing.T) {
	stub := newResendStub(t, http.StatusOK, `{"id":"x"}`)
	body := "Open this link:\n\nhttps://example.test/verify?token=abc\n\nIt expires in an hour."
	if err := stub.mailer().Send(context.Background(), "ada@example.com", "Verify", body); err != nil {
		t.Fatalf("a multi-line body was refused: %v", err)
	}
	if stub.gotBody["text"] != body {
		t.Errorf("the body was altered in transit:\n got %q\nwant %q", stub.gotBody["text"], body)
	}
}

// The same message twice carries the same key; a different message does not.
// Two reset requests for one person are different messages, each with its own
// single-use token, so a key that deduplicated them would deliver the first
// token twice and drop the second.
func TestResendKeysIdempotencyOnTheWholeMessage(t *testing.T) {
	send := func(body string) string {
		stub := newResendStub(t, http.StatusOK, `{"id":"x"}`)
		if err := stub.mailer().Send(context.Background(),
			"ada@example.com", "Reset your password", body); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if stub.gotIdem == "" {
			t.Fatal("no Idempotency-Key was sent; a transport retry would deliver twice")
		}
		return stub.gotIdem
	}

	first := send("https://example.test/reset?token=AAA")
	again := send("https://example.test/reset?token=AAA")
	other := send("https://example.test/reset?token=BBB")

	if first != again {
		t.Error("the same message produced two keys, so a retry would send it twice")
	}
	if first == other {
		t.Error("two different reset tokens share one key; the second would be " +
			"suppressed and its recipient would be sent a token already spent")
	}
}

// An unverified sending domain is the first thing that goes wrong on a new
// deployment.
func TestResendExplainsAnUnverifiedDomain(t *testing.T) {
	stub := newResendStub(t, http.StatusForbidden,
		`{"name":"validation_error","message":"The tryatlantis.dev domain is not verified"}`)

	err := stub.mailer().Send(context.Background(), "ada@example.com", "Verify", "body")
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if !strings.Contains(err.Error(), "not verified") {
		t.Errorf("the error does not carry what Resend said: %v", err)
	}
	// "403" alone points at the API reference when the answer is in DNS.
	if !strings.Contains(err.Error(), "DNS") {
		t.Errorf("the error does not point at domain verification: %v", err)
	}
}

func TestResendReportsOtherFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"bad key":    {http.StatusUnauthorized, `{"name":"missing_api_key","message":"Missing API key"}`, "API key"},
		"rate limit": {http.StatusTooManyRequests, `{"name":"rate_limit_exceeded","message":"Too many requests"}`, "rate limit"},
		"validation": {http.StatusBadRequest, `{"name":"validation_error","message":"Invalid ` + "`to`" + ` field"}`, "validation_error"},
		"not json":   {http.StatusInternalServerError, `<html>gateway</html>`, "500"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newResendStub(t, tc.status, tc.body)
			err := stub.mailer().Send(context.Background(), "ada@example.com", "S", "b")
			if err == nil {
				t.Fatalf("status %d was reported as success", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not mention %q: %v", tc.want, err)
			}
		})
	}
}

// Configuration faults are named as configuration, before any request.
func TestResendRefusesIncompleteConfiguration(t *testing.T) {
	for name, m := range map[string]*Resend{
		"no api key": {From: "no-reply@tryatlantis.dev"},
		"no sender":  {APIKey: "re_test_key"},
	} {
		t.Run(name, func(t *testing.T) {
			err := m.Send(context.Background(), "ada@example.com", "S", "b")
			if err == nil {
				t.Fatal("an incomplete mailer accepted a message")
			}
			// Not a network error: nothing should have been dialled.
			if strings.Contains(err.Error(), "dial") || strings.Contains(err.Error(), "connection") {
				t.Errorf("the mailer tried to send before checking its configuration: %v", err)
			}
		})
	}
}

// A 200 carrying no id is not a failure, but losing it silently would leave a
// delivery question with nothing to look up while everything reported success.
func TestResendWarnsWhenNoIDComesBack(t *testing.T) {
	var logged strings.Builder
	stub := newResendStub(t, http.StatusOK, `{}`)
	m := stub.mailer()
	m.Logger = slog.New(slog.NewTextHandler(&logged, nil))

	if err := m.Send(context.Background(), "ada@example.com", "S", "b"); err != nil {
		t.Fatalf("a 200 with no id was treated as a failure: %v", err)
	}
	if !strings.Contains(logged.String(), "no id") {
		t.Errorf("nothing was logged about the missing id: %q", logged.String())
	}
}

// The 64KB read bounds memory, not the error message. A proxy returning an HTML
// page would otherwise put the whole page where the failure reason belongs.
func TestResendTruncatesAHugeErrorBody(t *testing.T) {
	huge := "<html>" + strings.Repeat("x", 40_000) + "</html>"
	stub := newResendStub(t, http.StatusBadGateway, huge)

	err := stub.mailer().Send(context.Background(), "ada@example.com", "S", "b")
	if err == nil {
		t.Fatal("a 502 was reported as success")
	}
	if len(err.Error()) > 600 {
		t.Errorf("the error is %d bytes; an upstream error page is being copied "+
			"into the log verbatim", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("the error no longer names the status: %v", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the error does not say it was cut, so it reads as the whole "+
			"response: %v", err)
	}
}

// The bytes come from a remote server and are not necessarily ASCII, so a
// byte-offset cut can land inside a rune.
func TestResendTruncationLeavesValidUTF8(t *testing.T) {
	// Multi-byte runes packed so that a 200-byte cut lands inside one.
	stub := newResendStub(t, http.StatusBadGateway, strings.Repeat("é", 500))

	err := stub.mailer().Send(context.Background(), "ada@example.com", "S", "b")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !utf8.ValidString(err.Error()) {
		t.Error("the truncated error is not valid UTF-8; the cut landed inside a rune")
	}
}
