// Package mail sends the two messages Atlantis Cloud needs: verify this
// address, and reset this password.
//
// Three transports implement Mailer: SMTP, Resend, and Log for development.
//
// Messages are plain text. There is no retry and no queue: Send reports the
// failure to its caller, which asks the user to try again.
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

// Mailer sends one plain-text message. There is no cc, reply-to or attachment.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// ErrHeaderInjection reports a recipient or subject containing a line break.
// The recipient arrives from a sign-up form; CRLF followed by "Bcc:" turns one
// verification message into a relay.
var ErrHeaderInjection = errors.New("line breaks are not allowed in a recipient or subject")

// checkHeaderFields reports whether from, to and subject are free of line
// breaks.
//
// Separate from compose so that a transport which does not build an RFC 5322
// message still applies it. JSON encoding escapes the break in transit and says
// nothing about what the receiving service does with it.
func checkHeaderFields(from, to, subject string) error {
	for _, field := range []string{to, subject, from} {
		if strings.ContainsAny(field, "\r\n") {
			return ErrHeaderInjection
		}
	}
	return nil
}

// compose builds an RFC 5322 message, refusing anything that would inject a
// header.
func compose(from, to, subject, body string) ([]byte, error) {
	if err := checkHeaderFields(from, to, subject); err != nil {
		return nil, err
	}
	// Some receivers treat a missing Date as a spam signal.
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
// Uses smtp.Client rather than smtp.SendMail: SendMail takes no context and
// dials with no timeout.
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

	// smtp.Client reads and writes through this connection, so the deadline
	// bounds the whole exchange and not just the dial.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("start the SMTP session: %w", err)
	}
	defer c.Close() //nolint:errcheck

	// Always attempted when offered; there is no configuration to disable it.
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}

	if s.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted connection
		// unless host is localhost, so a server that offered no STARTTLS fails
		// here rather than sending the password in clear text.
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

// Log writes messages to the log instead of sending them, so `make dev` needs
// no mail server. Every send logs at warn level: this transport puts
// password-reset links in the log, and a deployment running on it looks from
// the outside like one that is delivering mail.
type Log struct{ Logger *slog.Logger }

func (l *Log) Send(_ context.Context, to, subject, body string) error {
	if _, err := compose("dev@localhost", to, subject, body); err != nil {
		// Checked even though nothing is transmitted, so that a header-injection
		// bug fails in development.
		return err
	}
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("CLOUD_MAIL_DEV is set — printing the message instead of sending it",
		"to", to, "subject", subject, "body", body)
	return nil
}
