package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rachitkumar205/atlantis/internal/console/secrets"
)

// undefinedTable is PostgreSQL's SQLSTATE for a missing relation.
//
// Named locally rather than pulled from github.com/jackc/pgerrcode, matching
// internal/runtime/sandbox/embedded, which does the same for
// insufficient_privilege. One constant does not earn a direct dependency.
const undefinedTable = "42P01"

// OrgRegistration is one organisation's atlantis, as an operator supplies it.
//
// PEM text rather than file paths, deliberately. The console stopped loading
// certificates from disk when they became a property of an organisation, and
// the reading is done by whoever is calling — `cloud org register` — so that
// this package keeps exactly one idea of where credentials come from.
type OrgRegistration struct {
	Org        string
	Endpoint   string
	HealthAddr string
	CAPEM      string
	CertPEM    string
	KeyPEM     []byte
}

// RegisterOrg records an organisation's atlantis and seals its private key.
//
// This is the only exported way into console.orgs, and it exists because
// registration is Cloud's job rather than the console's: an HTTP route that
// provisions an organisation is a route that has to prove who is asking, and
// nothing in front of the console can prove it yet. `cloud org register`
// therefore writes the row directly, which is why the entry point is a function
// on a connection string rather than a handler.
//
// Upserts. Re-registering an organisation rotates its certificate in place, and
// a running console picks that up within orgClientRefresh without a restart.
func RegisterOrg(ctx context.Context, pgURL, keyset string, r OrgRegistration) error {
	keys, err := secrets.FromEnvKeyset(keyset)
	if err != nil {
		return fmt.Errorf("CONSOLE_DATA_KEY: %w", err)
	}
	// The same keyring the console runs with, so a row written here is one the
	// console can open. Registering under a different key than the console
	// serves under produces a row that looks complete and cannot be decrypted,
	// which is a failure worth making impossible rather than diagnosable.
	db, err := newStore(ctx, pgURL, slog.Default(), keys)
	if err != nil {
		return fmt.Errorf("open console db: %w", err)
	}
	defer db.close()

	err = db.registerOrg(ctx, orgCredentials{
		Org:        r.Org,
		Endpoint:   r.Endpoint,
		HealthAddr: r.HealthAddr,
		CAPEM:      r.CAPEM,
		CertPEM:    r.CertPEM,
		KeyPEM:     r.KeyPEM,
	})
	// The console owns this schema and applies it at startup, so an operator who
	// registers before the console has ever run gets a bare "relation does not
	// exist". Naming the order is cheaper than making this command a second
	// owner of the migration tree.
	//
	// Matched on SQLSTATE rather than on the message. The first version of this
	// looked for `"orgs" does not exist`, which never matched anything —
	// PostgreSQL quotes the qualified name, so the text is `relation
	// "console.orgs" does not exist`. A guard keyed to prose nobody checked
	// against a live server is a guard that silently never fires.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
		return fmt.Errorf("%w\n\nThe console creates its own schema when it "+
			"starts, so start the console once before registering an organisation", err)
	}
	return err
}

// validateOrgCredentials refuses a registration that cannot produce a working
// channel.
//
// Called from store.registerOrg rather than from RegisterOrg, so that it covers
// every write to console.orgs — including the test harness, which would
// otherwise be the one path allowed to store credentials nothing checks.
//
// # Why check at all, when the dial checks
//
// buildOrgTLS parses the same material and fails on the same faults, so none of
// this makes a bad registration safe that would otherwise be unsafe. What it
// changes is when the operator finds out. A mis-registered organisation is
// discovered at somebody's next page load, as a 503 naming TLS, hours after the
// command that caused it printed nothing and exited 0 — and the person reading
// that error is usually not the person who ran the command.
//
// # What it deliberately does not check
//
// That the client certificate chains to CAPEM. CAPEM is the root this console
// verifies the organisation's *server* against; the client leaf is issued by
// whatever root that server trusts for clients. Those are the same CA in every
// deployment that exists today and are not required to be, so a chain check
// here would refuse a correct configuration. The handshake is the authority on
// that pairing, and it is the layer this step is built on.
func validateOrgCredentials(c orgCredentials) error {
	if c.Org == "" {
		// Also what the private key is sealed against, so an empty one would
		// be refused by the keyring a moment later with a less useful message.
		return errors.New("organisation name is required")
	}
	for _, a := range []struct{ what, addr string }{
		{"endpoint", c.Endpoint},
		{"health address", c.HealthAddr},
	} {
		if a.addr == "" {
			return fmt.Errorf("%s is required", a.what)
		}
		if _, _, err := net.SplitHostPort(a.addr); err != nil {
			return fmt.Errorf("%s %q is not host:port: %w", a.what, a.addr, err)
		}
	}

	if !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CAPEM)) {
		return errors.New("the CA bundle contains no usable certificates")
	}
	if _, err := tls.X509KeyPair([]byte(c.CertPEM), c.KeyPEM); err != nil {
		// The common cause is the two files swapped on the command line, which
		// this message is trying to make guessable.
		return fmt.Errorf("the client certificate and key are not a pair: %w", err)
	}

	leaf, err := leafOf(c.CertPEM)
	if err != nil {
		return err
	}
	// Validity is checked here and nowhere else useful. An expired client
	// certificate is refused by the server during the handshake, and the error
	// the console surfaces for that is indistinguishable from the server being
	// misconfigured — so an operator debugging it starts on the wrong machine.
	now := time.Now()
	switch {
	case now.Before(leaf.NotBefore):
		return fmt.Errorf("the client certificate is not valid until %s",
			leaf.NotBefore.Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		return fmt.Errorf("the client certificate expired on %s",
			leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// leafOf returns the first certificate in a PEM bundle.
//
// First rather than only: a client bundle may carry intermediates after the
// leaf, and Go's own convention in tls.X509KeyPair is that the leaf comes
// first.
func leafOf(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("the client certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the client certificate could not be parsed: %w", err)
	}
	return leaf, nil
}
