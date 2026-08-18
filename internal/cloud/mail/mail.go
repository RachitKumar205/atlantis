// Package mail sends the two messages Atlantis Cloud needs: verify this
// address, and reset this password.
//
// # Why an interface for two messages
//
// The same reason internal/secrets has one. The implementation is
// expected to change — SMTP today, very likely a transactional API later — and
// an interface means that change is a constructor rather than an edit to every
// call site. Two implementations exist from the start so the interface is
// shaped by more than one caller: SMTP for a deployment, and a logging one for
// development, which is also what the tests use.
//
// # What is deliberately not here
//
// Templates, HTML, retries and a queue. A verification email is a sentence and
// a link; making it a rendering pipeline before anything has sent one would be
// building for a product that does not exist. Retries in particular are worth
// naming as absent: a failed send surfaces to the caller, and the caller tells
// the user to try again, because a background retry needs durable state that
// nothing here has.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends one message.
//
// Deliberately narrow. A wider interface — attachments, cc, reply-to — would be
// designed against nothing, because Cloud sends two message shapes and both are
// a subject and a paragraph.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// ErrHeaderInjection reports a recipient or subject containing a line break.
//
// This is a real attack rather than tidiness. The recipient address arrives
// from a sign-up form, and an address containing CRLF followed by `Bcc:` turns
// one verification email into a mail relay. The check is here, in the one place
// that composes a message, rather than in each caller.
var ErrHeaderInjection = errors.New("line breaks are not allowed in a recipient or subject")

// compose builds an RFC 5322 message, refusing anything that would inject a
// header.
func compose(from, to, subject, body string) ([]byte, error) {
	for _, field := range []string{to, subject, from} {
		if strings.ContainsAny(field, "\r\n") {
			return nil, ErrHeaderInjection
		}
	}
	// Date is included because some receivers treat its absence as a spam
	// signal, and a verification email landing in spam is indistinguishable
	// from one that was never sent.
	msg := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		body + "\r\n"
	return []byte(msg), nil
}

// SMTP sends through a mail server.
type SMTP struct {
	// Addr is host:port. Port 587 with STARTTLS is the usual submission
	// endpoint.
	Addr string

	// From is the envelope and header sender.
	From string

	// Username and Password authenticate to the server. Both empty means no
	// authentication, which is the shape of a local relay that authorises by
	// network position.
	Username string
	Password string

	// Timeout bounds the whole exchange. This runs inside a sign-up request, so
	// a mail server that hangs must not hold the request open indefinitely.
	Timeout time.Duration
}

// Send delivers one message.
//
// Written against smtp.Client rather than smtp.SendMail because SendMail takes
// no context and dials with no timeout — a hung mail server would hold a
// request open until something else gave up.
func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	msg, err := compose(s.From, to, subject, body)
	if err != nil {
		return err
	}

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("SMTP address %q is not host:port: %w", s.Addr, err)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("connect to the mail server: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	// The deadline is what makes the context bound the exchange rather than
	// only the dial: smtp.Client's own reads and writes go through this
	// connection.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("start the SMTP session: %w", err)
	}
	defer c.Close() //nolint:errcheck

	// STARTTLS when the server offers it. Not optional in the sense a caller
	// can turn it off — the alternative is sending a password-reset link, and
	// possibly a password, across the network in clear text.
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}

	if s.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted connection
		// unless the host is localhost. That refusal is load-bearing and is why
		// the STARTTLS attempt above is not conditional on configuration: a
		// server that does not offer it will fail here rather than leak.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return fmt.Errorf("authenticate to the mail server: %w", err)
		}
	}

	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish the message: %w", err)
	}
	return c.Quit()
}

// Log writes messages to the log instead of sending them.
//
// What a development environment gets, so `make dev` needs no mail server: the
// verification link appears in the console output and can be pasted into a
// browser.
//
// It warns on every send, at every send, and that is deliberate. A deployment
// that reaches production without SMTP configured would otherwise print
// password-reset links into its logs and look, from the outside, exactly like
// one that was delivering them — while every user waited for an email that was
// never sent.
type Log struct{ Logger *slog.Logger }

func (l *Log) Send(_ context.Context, to, subject, body string) error {
	if _, err := compose("dev@localhost", to, subject, body); err != nil {
		// Checked even though nothing is transmitted, so a header-injection bug
		// is caught in development rather than first appearing in production.
		return err
	}
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("no mail server configured — printing the message instead of sending it",
		"to", to, "subject", subject, "body", body)
	return nil
}
