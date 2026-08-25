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
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/secrets"
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

	// PublicEndpoint is what callers dial, when that differs from Endpoint.
	//
	// Optional. Empty means "the same", which is stored as NULL so the fallback
	// lives in one place — the COALESCE in orgCredentials — rather than being
	// copied at registration and then diverging the first time somebody changes
	// Endpoint alone.
	PublicEndpoint string

	// This organisation's own certificate signer.
	//
	// Optional, and optional AS A GROUP: all four or none. Empty means the
	// console uses its process-wide ATL_SIGNER_* settings, which is what every
	// organisation registered before migration 0009 does and what
	// `make dev-signer` serves.
	//
	// A mixture is refused rather than stored. Three of four is not "mostly
	// configured": it produces a console dialling one signer while presenting
	// credentials for another, which fails at that signer's handshake with an
	// error about a certificate rather than about this row.
	SignerAddr          string
	SignerCAPEM         string
	SignerClientCertPEM string
	SignerClientKeyPEM  []byte
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
		Org:            r.Org,
		Endpoint:       r.Endpoint,
		HealthAddr:     r.HealthAddr,
		CAPEM:          r.CAPEM,
		CertPEM:        r.CertPEM,
		KeyPEM:         r.KeyPEM,
		PublicEndpoint: r.PublicEndpoint,

		SignerAddr:          r.SignerAddr,
		SignerCAPEM:         r.SignerCAPEM,
		SignerClientCertPEM: r.SignerClientCertPEM,
		SignerClientKeyPEM:  r.SignerClientKeyPEM,
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

	return validateOrgSigner(c)
}

// validateOrgSigner refuses a half-supplied signer, and checks the rest of it
// the same way the atlantis credentials above are checked.
//
// The group check comes first and is the important one. A mixture is not a
// smaller version of a working configuration: it is a console that dials one
// signer holding another's credentials, refused at that signer's handshake with
// a message about a certificate rather than about this registration.
func validateOrgSigner(c orgCredentials) error {
	if !c.SignerConfigured() {
		if c.signerHalfConfigured() {
			return errors.New("a signer needs its address, CA, client certificate and key — " +
				"supply all four to give this organisation its own, or none to use the " +
				"console's shared one")
		}
		// None of the four. The organisation uses the process-wide signer,
		// which is the ordinary state.
		return nil
	}

	if _, err := url.Parse(c.SignerAddr); err != nil {
		return fmt.Errorf("the signer address %q is not a URL: %w", c.SignerAddr, err)
	}
	if !strings.HasPrefix(c.SignerAddr, "https://") {
		// The console presents a client certificate to this address and
		// receives a certificate authority's output back. Plain HTTP would
		// send the first in the clear and take the second on trust.
		return fmt.Errorf("the signer address %q must be https://", c.SignerAddr)
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(c.SignerCAPEM)) {
		return errors.New("the signer CA bundle contains no usable certificates")
	}
	if _, err := tls.X509KeyPair([]byte(c.SignerClientCertPEM), c.SignerClientKeyPEM); err != nil {
		return fmt.Errorf("the signer client certificate and key are not a pair: %w", err)
	}

	signerLeaf, err := leafOf(c.SignerClientCertPEM)
	if err != nil {
		return fmt.Errorf("signer client certificate: %w", err)
	}
	now := time.Now()
	switch {
	case now.Before(signerLeaf.NotBefore):
		return fmt.Errorf("the signer client certificate is not valid until %s",
			signerLeaf.NotBefore.Format(time.RFC3339))
	case now.After(signerLeaf.NotAfter):
		return fmt.Errorf("the signer client certificate expired on %s",
			signerLeaf.NotAfter.Format(time.RFC3339))
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

// UnregisterOrg removes everything the console holds for an organisation that
// no longer exists.
//
// # Why this exists
//
// Purging an organisation destroyed its namespace, its database and its
// certificate authority, and left this row untouched. So the console went on
// holding a sealed private key for an organisation that had been deleted, for
// ever, and `cloud org purge` told the operator it had destroyed everything.
// Nothing else ever deleted from console.orgs — the only DELETE in the tree was
// in a test.
//
// # What it removes, and the one thing it keeps
//
// The registration and the operational state that referenced it: sessions bound
// to the organisation, enrolment tokens that could still be redeemed, and the
// fingerprint-to-organisation rows renewal looks up. None of those mean anything
// once the certificate authority behind them is gone, and an enrolment token
// outliving its organisation is a live credential for something that no longer
// exists.
//
// The audit log is deliberately NOT removed. It references the organisation by
// name and is supposed to outlive it: "what happened to acme" has to still have
// an answer after acme stops existing, which is the same reason purgeOne writes
// an audit row on its way out.
//
// # Idempotent
//
// Deleting nothing is success. The purge path retries after a partial failure,
// and a second pass finding the rows already gone is the ordinary case rather
// than a problem.
func UnregisterOrg(ctx context.Context, pgURL, org string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	// The same connection setup the console itself uses, rather than a bare
	// pgxpool.New. console.orgs carries no row-level security today, so this is
	// not load-bearing yet — it is here so that adding a policy later does not
	// silently turn these deletes into no-ops.
	cfg, err := newPoolConfig(pgURL)
	if err != nil {
		return fmt.Errorf("console database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open console db: %w", err)
	}
	defer pool.Close()
	// pgxpool.NewWithConfig is lazy, so an unreachable database would otherwise
	// surface as a failure to delete rather than a failure to connect.
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect to console db: %w", err)
	}

	// One transaction. A half-removed organisation is the state this function
	// exists to stop: enrolment tokens outliving the registration would be
	// redeemable against an organisation the console can no longer describe.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin unregister: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Ordered dependents-first. Nothing here has a foreign key — the console
	// schema declares none — so the order buys no integrity, only a readable
	// transaction if one statement fails.
	for _, q := range []struct{ what, sql string }{
		{"enrolment tokens", `DELETE FROM console.enroll_tokens WHERE org = $1`},
		{"caller certificates", `DELETE FROM console.caller_certs WHERE org = $1`},
		{"sessions", `DELETE FROM console.sessions WHERE org = $1`},
		{"the registration", `DELETE FROM console.orgs WHERE org = $1`},
	} {
		if _, err := tx.Exec(ctx, q.sql, org); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
				// A console database that predates one of these tables. Not a
				// reason to leave the rest of the organisation registered.
				continue
			}
			return fmt.Errorf("remove %s for %s: %w", q.what, org, err)
		}
	}
	return tx.Commit(ctx)
}
