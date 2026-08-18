package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// A composed message carries the headers a receiver expects, and the body.
func TestCompose(t *testing.T) {
	msg, err := compose("cloud@atlantis.test", "ada@example.com", "Verify your email", "Open this link: https://x/y")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	got := string(msg)

	for _, want := range []string{
		"From: cloud@atlantis.test\r\n",
		"To: ada@example.com\r\n",
		"Subject: Verify your email\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Date: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the message has no %q:\n%s", want, got)
		}
	}

	// Headers and body are separated by a blank line; without it the body is
	// parsed as more headers and the message arrives empty.
	head, body, ok := strings.Cut(got, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body separator:\n%s", got)
	}
	if !strings.Contains(body, "https://x/y") {
		t.Errorf("the body did not survive: %q", body)
	}
	if strings.Contains(head, "https://x/y") {
		t.Errorf("the body leaked into the headers: %q", head)
	}
}

// A line break in a user-supplied field is refused.
//
// This is the attack, not a formatting nicety: the recipient arrives from a
// sign-up form, and `victim@x.com\r\nBcc: everyone@y.com` turns one
// verification email into a mail relay. The check lives in compose because that
// is the single place a message is built — putting it in each caller is the
// version where one caller forgets.
func TestComposeRefusesHeaderInjection(t *testing.T) {
	cases := map[string]struct{ to, subject string }{
		"CRLF in recipient":   {"victim@example.com\r\nBcc: everyone@example.net", "Hello"},
		"LF in recipient":     {"victim@example.com\nBcc: everyone@example.net", "Hello"},
		"CR in recipient":     {"victim@example.com\rBcc: everyone@example.net", "Hello"},
		"CRLF in subject":     {"ada@example.com", "Hello\r\nBcc: everyone@example.net"},
		"newline in subject":  {"ada@example.com", "Hello\nX-Injected: yes"},
		"injection both ways": {"a@b.c\r\nX: 1", "d\r\nY: 2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compose("cloud@atlantis.test", tc.to, tc.subject, "body")
			if err == nil {
				t.Fatal("a field containing a line break was accepted, which injects a header")
			}
			if !errors.Is(err, ErrHeaderInjection) {
				t.Errorf("got %v, want ErrHeaderInjection", err)
			}
		})
	}

	// The counterpart: without it the table above passes against a compose that
	// refuses everything.
	if _, err := compose("cloud@atlantis.test", "ada@example.com", "Perfectly ordinary", "body"); err != nil {
		t.Errorf("an ordinary message was refused: %v", err)
	}
}

// A newline in the BODY is fine — it is after the separator and cannot inject.
func TestComposeAllowsNewlinesInTheBody(t *testing.T) {
	msg, err := compose("cloud@atlantis.test", "ada@example.com", "Subject",
		"First line.\n\nSecond paragraph.")
	if err != nil {
		t.Fatalf("a multi-line body was refused, which makes every message one line: %v", err)
	}
	if !strings.Contains(string(msg), "Second paragraph.") {
		t.Error("the body did not survive")
	}
}

// The development mailer checks the same thing it would have sent.
//
// Without this, a header-injection bug is invisible in development — where
// every message goes to a log that does not care — and first appears against a
// real mail server.
func TestLogMailerStillRefusesInjection(t *testing.T) {
	m := &Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := m.Send(context.Background(), "a@b.c\r\nBcc: x@y.z", "Subject", "body"); !errors.Is(err, ErrHeaderInjection) {
		t.Fatalf("got %v, want ErrHeaderInjection", err)
	}
	if err := m.Send(context.Background(), "ada@example.com", "Subject", "body"); err != nil {
		t.Errorf("an ordinary message was refused: %v", err)
	}
}

// The development mailer warns, every time.
//
// A deployment that reaches production with no SMTP configured prints
// password-reset links into its logs while looking, from outside, exactly like
// one that is delivering them. The warning is the only thing that distinguishes
// the two, so it is asserted rather than assumed.
func TestLogMailerWarnsOnEverySend(t *testing.T) {
	var buf strings.Builder
	m := &Log{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	for range 2 {
		if err := m.Send(context.Background(), "ada@example.com", "Verify", "https://x/y"); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Errorf("two sends produced %d warnings, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "no mail server configured") {
		t.Errorf("the warning does not say what is wrong:\n%s", out)
	}
	// The link has to be in there, or the development flow does not work.
	if !strings.Contains(out, "https://x/y") {
		t.Errorf("the message body is not in the log, so there is no link to follow:\n%s", out)
	}
}

// The SMTP address is validated before anything is dialled.
func TestSMTPRejectsAMalformedAddress(t *testing.T) {
	s := &SMTP{Addr: "mail.example.com", From: "cloud@atlantis.test"}

	err := s.Send(context.Background(), "ada@example.com", "Subject", "body")
	if err == nil {
		t.Fatal("an address with no port was accepted")
	}
	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("the error does not say what is wrong with it: %v", err)
	}
}

// Injection is refused before a connection is attempted.
//
// Ordering matters: a message that would be refused must not first open a
// socket to a mail server, both because it is pointless and because the failure
// should name the injection rather than whatever the network did.
func TestSMTPRefusesInjectionWithoutDialling(t *testing.T) {
	// An address nothing is listening on. Reaching the dial would produce a
	// connection error instead of the one expected.
	s := &SMTP{Addr: "127.0.0.1:1", From: "cloud@atlantis.test"}

	err := s.Send(context.Background(), "a@b.c\r\nBcc: x@y.z", "Subject", "body")
	if !errors.Is(err, ErrHeaderInjection) {
		t.Fatalf("got %v, want ErrHeaderInjection — the message was composed after dialling", err)
	}
}
