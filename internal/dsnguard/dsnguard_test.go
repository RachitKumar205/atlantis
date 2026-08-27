package dsnguard

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// Every case here is an address this must not reach. They are listed one by one
// rather than inferred from a clean run: a guard that refuses nine of ten
// ranges looks identical to one that refuses all ten until the tenth is used.

func TestAllowedRefusesEverythingOffTheInternet(t *testing.T) {
	refused := []struct{ name, ip string }{
		{"loopback v4", "127.0.0.1"},
		{"loopback v6", "::1"},
		{"private 10/8", "10.0.0.1"},
		{"private 172.16/12", "172.16.0.1"},
		{"private 192.168/16", "192.168.1.1"},
		{"unique local v6", "fd00::1"},
		// Where a cloud provider serves instance credentials. The single most
		// valuable address an SSRF can reach.
		{"link-local metadata", "169.254.169.254"},
		{"link-local v6", "fe80::1"},
		{"carrier-grade NAT", "100.64.0.1"},
		{"unspecified", "0.0.0.0"},
		{"multicast", "224.0.0.1"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if err := allowed(net.ParseIP(c.ip)); err == nil {
				t.Errorf("%s was allowed", c.ip)
			}
		})
	}

	t.Run("unparseable", func(t *testing.T) {
		// A dialer handed a string this could not read is a dialer that
		// resolves it again, somewhere this cannot see.
		if err := allowed(nil); err == nil {
			t.Error("an address that did not parse was allowed")
		}
	})

	t.Run("a public address is allowed", func(t *testing.T) {
		for _, ip := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"} {
			if err := allowed(net.ParseIP(ip)); err != nil {
				t.Errorf("%s was refused: %v", ip, err)
			}
		}
	})
}

func TestConfigRefusesUnixSockets(t *testing.T) {
	// pgconn skips LookupFunc for an absolute path, so this is the one host
	// shape that reaches the dialer with no address check behind it. On this
	// machine it reaches Cloud's own Postgres.
	// Both spellings. The URL form carries the socket directory as a query
	// parameter, which is the shape libpq documents.
	for _, dsn := range []string{
		"host=/var/run/postgresql user=u password=p dbname=db",
		"postgres:///db?host=/var/run/postgresql&sslmode=require",
	} {
		if _, err := Config(dsn, false); !errors.Is(err, ErrUnixSocket) {
			t.Errorf("Config(%q) error = %v, want ErrUnixSocket", dsn, err)
		}
	}
}

func TestConfigRefusesPlaintext(t *testing.T) {
	if _, err := Config("postgres://u:p@db.example.com/db?sslmode=disable", false); !errors.Is(err, ErrNoTLS) {
		t.Error("sslmode=disable was accepted; the password would cross the internet in clear")
	}
}

// sslmode=prefer is the libpq default, so this is the shape most people paste.
// Refusing it would make the guard look broken; accepting it as written would
// let pgx fall back to plaintext after TLS failed.
func TestConfigUpgradesPreferToRequire(t *testing.T) {
	cfg, err := Config("postgres://u:p@db.example.com/db", false)
	if err != nil {
		t.Fatalf("a DSN with no sslmode was refused: %v", err)
	}
	if cfg.ConnConfig.TLSConfig == nil {
		t.Error("the primary candidate has no TLS, so the first attempt is in clear")
	}
	for i, fb := range cfg.ConnConfig.Fallbacks {
		if fb.TLSConfig == nil {
			t.Errorf("fallback %d has no TLS, so a failed handshake falls back to plaintext", i)
		}
	}
}

// A DSN may name several hosts, and pgconn tries each. Checking only the first
// leaves the rest unguarded.
func TestConfigChecksEveryHost(t *testing.T) {
	_, err := Config("host=db.example.com,/var/run/postgresql user=u password=p dbname=db sslmode=require", false)
	if !errors.Is(err, ErrUnixSocket) {
		t.Errorf("a socket path in the second host position was not refused: %v", err)
	}
}

func TestConfigSetsBounds(t *testing.T) {
	cfg, err := Config("postgres://u:p@db.example.com/db?sslmode=require", false)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.ConnConfig.ConnectTimeout != ConnectTimeout {
		t.Error("no connect timeout, so a host that accepts and never answers holds the request open")
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != StatementTimeout {
		t.Errorf("statement_timeout = %q, want %q — introspection runs queries this package never sees", got, StatementTimeout)
	}
	if cfg.ConnConfig.LookupFunc == nil || cfg.ConnConfig.DialFunc == nil {
		t.Error("pgx was left to resolve or dial on its own, which is where rebinding wins")
	}
}

func TestLookupPublicDropsPrivateAnswers(t *testing.T) {
	// localhost resolves to loopback and nothing else, so a resolver that
	// filters correctly returns no address at all.
	if _, err := lookupPublic(context.Background(), "localhost"); !errors.Is(err, ErrNotPublic) {
		t.Errorf("localhost resolved to something dialable: %v", err)
	}
}

func TestDialPublicRefusesPrivateAddresses(t *testing.T) {
	// The backstop. Even if a resolver returned a private address, this is what
	// sees the final target.
	if _, err := dialPublic(context.Background(), "tcp", "169.254.169.254:5432"); !errors.Is(err, ErrNotPublic) {
		t.Error("the dialer accepted the cloud metadata address")
	}
}

func TestRedactRemovesTheConnectionString(t *testing.T) {
	dsn := "postgres://user:hunter2@db.example.com/db"
	err := Redact(errors.New(`failed to connect to `+dsn+`: timeout`), dsn)
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the password survived redaction: %s", err)
	}
	if strings.Contains(err.Error(), dsn) {
		t.Errorf("the connection string survived redaction: %s", err)
	}
}

// The acknowledged-insecure path.
//
// A public read-only dataset that offers no TLS is a real thing to want to
// read, and refusing it outright refuses the case this feature exists for. The
// switch is per request and never a default.
func TestConfigAllowsPlaintextWhenAcknowledged(t *testing.T) {
	cfg, err := Config("postgres://u:p@db.example.com/db?sslmode=disable", true)
	if err != nil {
		t.Fatalf("an acknowledged plaintext DSN was still refused: %v", err)
	}
	if cfg.ConnConfig.TLSConfig != nil {
		t.Error("sslmode=disable produced a TLS candidate, so the acknowledgement changed nothing")
	}
}

// Insecure means unencrypted, not unrestricted. Every address rule has to hold
// with the switch on, or the checkbox becomes a way to reach the metadata
// service.
func TestAcknowledgementDoesNotRelaxTheAddressRules(t *testing.T) {
	if _, err := Config("host=/var/run/postgresql user=u password=p dbname=db", true); !errors.Is(err, ErrUnixSocket) {
		t.Error("the acknowledgement let a unix socket through")
	}

	cfg, err := Config("postgres://u:p@db.example.com/db?sslmode=disable", true)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.ConnConfig.LookupFunc == nil || cfg.ConnConfig.DialFunc == nil {
		t.Fatal("the acknowledgement unpinned resolution or dialling")
	}
	if _, err := dialPublic(context.Background(), "tcp", "169.254.169.254:5432"); !errors.Is(err, ErrNotPublic) {
		t.Error("the metadata address became reachable")
	}
}

// The driver blames the server for a policy this package set, so a caller
// relaying it verbatim tells somebody their database is broken.
func TestIsTLSRefusalRecognisesTheDriverWording(t *testing.T) {
	for _, msg := range []string{
		"tls error: server refused TLS connection",
		"server does not support SSL, but SSL was required",
	} {
		if !IsTLSRefusal(errors.New(msg)) {
			t.Errorf("not recognised as a TLS refusal: %s", msg)
		}
	}
	if IsTLSRefusal(errors.New("connection refused")) {
		t.Error("an ordinary connection failure was reported as a TLS refusal")
	}
	if IsTLSRefusal(nil) {
		t.Error("nil was reported as a TLS refusal")
	}
}
