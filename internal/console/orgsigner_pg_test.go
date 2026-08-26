package console

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// Per-organisation signer credentials on console.orgs.
//
// The three properties here are the ones migration 0009 exists to get right,
// and each of them fails in a way that does not look like its cause.

// An organisation registered before 0009 has four NULL columns, and must keep
// working exactly as it did.
//
// This is the eviction case. orgClients.get treats ErrOrgNotProvisioned as an
// ANSWER and closes the cached connection, so a read that reported a missing
// signer as "not provisioned" would not fail a request — it would de-provision
// every organisation registered before the migration.
func TestAnOrganisationWithNoSignerOfItsOwnStillWorks(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	creds, err := f.srv.db.orgCredentials(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("an organisation with no signer columns could not be read: %v", err)
	}
	if creds.SignerConfigured() {
		t.Error("an organisation with no signer columns reports one")
	}
	// The atlantis half is untouched — that is what "still works" means.
	if creds.CAPEM == "" || creds.CertPEM == "" || len(creds.KeyPEM) == 0 {
		t.Error("the organisation's atlantis credentials did not survive the read")
	}
}

func TestAnOrganisationsSignerRoundTrips(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	// A second, independent authority, which is what a signer per organisation
	// gets.
	signerPKI := testpki.New(t, t.TempDir())
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}

	creds := f.atl.credentials(t, defaultOrg)
	creds.SignerAddr = "https://signer.acme.svc:7070"
	creds.SignerCAPEM = read(signerPKI.CAFile)
	creds.SignerClientCertPEM = read(signerPKI.CertFile)
	creds.SignerClientKeyPEM = []byte(read(signerPKI.KeyFile))

	if err := f.srv.db.registerOrg(ctx, creds); err != nil {
		t.Fatalf("register with a signer: %v", err)
	}

	got, err := f.srv.db.orgCredentials(ctx, defaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SignerConfigured() {
		t.Fatal("the signer did not round-trip")
	}
	if got.SignerAddr != creds.SignerAddr {
		t.Errorf("signer addr = %q, want %q", got.SignerAddr, creds.SignerAddr)
	}
	if string(got.SignerClientKeyPEM) != string(creds.SignerClientKeyPEM) {
		t.Error("the signer client key did not survive sealing and opening")
	}
}

// Three of four is not a smaller version of a working configuration. It is a
// console that dials one organisation's signer holding another's credentials,
// refused at that signer with a message about a certificate.
func TestAHalfConfiguredSignerIsRefusedAtRegistration(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	creds := f.atl.credentials(t, defaultOrg)
	creds.SignerAddr = "https://signer.acme.svc:7070"
	// ...and nothing else.

	err := f.srv.db.registerOrg(ctx, creds)
	if err == nil {
		t.Fatal("a registration with one of four signer columns was accepted")
	}
}

// And if one is written behind registration's back, the read refuses it — with
// its own error, not the one that evicts.
func TestAHalfConfiguredSignerIsNotReportedAsUnprovisioned(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	if _, err := f.srv.db.pool.Exec(ctx,
		`UPDATE console.orgs SET signer_addr = $2 WHERE org = $1`,
		defaultOrg, "https://signer.acme.svc:7070"); err != nil {
		t.Fatal(err)
	}

	_, err := f.srv.db.orgCredentials(ctx, defaultOrg)
	if !errors.Is(err, ErrOrgSignerIncomplete) {
		t.Fatalf("a half-configured signer reported %v, want ErrOrgSignerIncomplete", err)
	}
	// The distinction that matters: this must not be the error the connection
	// pool treats as grounds to close a working channel.
	if errors.Is(err, ErrOrgNotProvisioned) {
		t.Error("a misconfigured signer is reported as an unprovisioned organisation, " +
			"which evicts the organisation's live atlantis connection")
	}
}

// The property the second associated-data string exists for.
//
// internal/secrets authenticates the associated data without storing it, so two
// ciphertexts sealed with the SAME associated data are interchangeable. Both
// keys in this row are sealed under the organisation name; if that were all,
// anybody who could UPDATE the table could move the signer's client key into
// client_key_ct and the console would present it to atlantis.
func TestTheTwoSealedKeysInARowAreNotInterchangeable(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	signerPKI := testpki.New(t, t.TempDir())
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}

	creds := f.atl.credentials(t, defaultOrg)
	creds.SignerAddr = "https://signer.acme.svc:7070"
	creds.SignerCAPEM = read(signerPKI.CAFile)
	creds.SignerClientCertPEM = read(signerPKI.CertFile)
	creds.SignerClientKeyPEM = []byte(read(signerPKI.KeyFile))
	if err := f.srv.db.registerOrg(ctx, creds); err != nil {
		t.Fatal(err)
	}

	// Move the signer's sealed key into the atlantis key column, which is what
	// somebody with UPDATE on this table would do.
	if _, err := f.srv.db.pool.Exec(ctx,
		`UPDATE console.orgs SET client_key_ct = signer_client_key_ct WHERE org = $1`,
		defaultOrg); err != nil {
		t.Fatal(err)
	}

	_, err := f.srv.db.orgCredentials(ctx, defaultOrg)
	if err == nil {
		t.Fatal("the signer's client key opened as the atlantis client key: " +
			"the two ciphertexts are interchangeable, so a row's own columns can be swapped")
	}
}
