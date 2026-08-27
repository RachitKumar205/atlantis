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
// PEM text rather than file paths. Certificates are a property of an
// organisation, not of the console's filesystem, and the caller — `cloud org
// register` — does the reading, so this package has one idea of where
// credentials come from.
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
	// Optional as a group: all four or none. Empty means the process-wide
	// ATL_SIGNER_* settings, which is what `make dev-signer` serves.
	//
	// A mixture is refused rather than stored. Three of four produces a console
	// dialling one signer while presenting credentials for another, failing at
	// that signer's handshake with an error about a certificate.
	SignerAddr          string
	SignerCAPEM         string
	SignerClientCertPEM string
	SignerClientKeyPEM  []byte
}

// RegisterOrg records an organisation's atlantis and seals its private key.
//
// The only exported way into console.orgs. Registration is Cloud's job, so
// `cloud org register` writes the row directly and the entry point takes a
// connection string rather than being a handler.
//
// Upserts. Re-registering rotates a certificate in place, and a running console
// picks that up within orgClientRefresh without a restart.
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
	// The console owns this schema and applies it at startup, so registering
	// before the console has ever run gets a bare "relation does not exist".
	//
	// Matched on SQLSTATE, not on the message: PostgreSQL quotes the qualified
	// name, so the text is `relation "console.orgs" does not exist`.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
		return fmt.Errorf("%w\n\nThe console creates its own schema when it "+
			"starts, so start the console once before registering an organisation", err)
	}
	return err
}

// validateOrgCredentials refuses a registration that cannot produce a working
// channel. Called from store.registerOrg, so it covers every write to
// console.orgs including the test harness.
//
// It does not check that the client certificate chains to CAPEM, which is the
// root the console verifies the organisation's SERVER against.
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
// no longer exists: the registration and its sealed private key, its sessions,
// its redeemable enrolment tokens, and the fingerprint rows renewal looks up.
// Idempotent.
//
// The audit log is not removed; it references the organisation by name and
// outlives it.
func UnregisterOrg(ctx context.Context, pgURL, org string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	// The same connection setup the console itself uses, rather than a bare
	// pgxpool.New: AfterConnect clears any organisation arriving from outside
	// the process, so no statement here runs bound to one.
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

	// The registration goes last, and takes two tables with it.
	//
	// console.enroll_tokens and console.schema_imports reference console.orgs
	// ON DELETE CASCADE since migration 0011, so they are not named here. That
	// is not a shortcut: both are under a RESTRICTIVE policy of
	// `org = console.current_org()`, this connection binds no organisation, and
	// a DELETE against either would match nothing and report no error.
	// Referential integrity actions bypass row security, so the cascade reaches
	// rows this session cannot select.
	//
	// The tables named below carry no policy, so their DELETEs run as written.
	for _, q := range []struct{ what, sql string }{
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
