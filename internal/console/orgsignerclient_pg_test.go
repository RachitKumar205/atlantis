package console

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Which signer an organisation's enrolment actually reaches.
//
// The fallback is exercised by every other test in this package, because they
// all run against a console with process-wide ATL_SIGNER_* settings and no
// signer columns. What none of them touch is the path migration 0009 exists
// for — so without this, the per-organisation client would be code nothing
// runs, and the suite would be green either way.

// An organisation with its own signer reaches ITS signer, not the shared one.
//
// Asserted on which server received the request rather than on the certificate
// that came back: both signers issue from the same organisation authority in
// this fixture, so the response looks identical whichever one answered. That is
// exactly the shape of failure this is here to catch.
func TestAnOrganisationWithItsOwnSignerDoesNotUseTheSharedOne(t *testing.T) {
	f := newEnrolmentFixture(t)
	ctx := context.Background()

	// A second signer, issuing from the same organisation authority — so the
	// certificate it returns is equally acceptable — but a different server,
	// with its own client-trust root.
	own := newFakeSigner(t, f.atl.pki)

	creds := f.atl.credentials(t, defaultOrg)
	creds.SignerAddr = own.URL
	creds.SignerCAPEM = readFileString(t, own.pki.CAFile)
	creds.SignerClientCertPEM = readFileString(t, own.clientCert)
	creds.SignerClientKeyPEM = []byte(readFileString(t, own.clientKey))
	if err := f.srv.db.registerOrg(ctx, creds); err != nil {
		t.Fatalf("register with its own signer: %v", err)
	}

	before := len(f.signer.snapshot())

	tok := f.mintToken(t, "backend")
	_, csrPEM := newKeyAndCSR(t, "backend")
	w := f.enrol(t, enrolBody(defaultOrg, tok, csrPEM))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}

	if got := len(own.snapshot()); got == 0 {
		t.Fatal("the organisation's own signer was never called; the console used the shared one")
	}
	if got := len(f.signer.snapshot()); got != before {
		t.Errorf("the shared signer was called %d extra times for an organisation "+
			"that has its own", got-before)
	}
}

// A rotated signer comes into use on the same freshness rule as a rotated
// atlantis certificate — cached inside the refresh window, replaced past it.
//
// Both halves are asserted, and the first is the one worth having: the two
// clients live in one entry keyed on updated_at precisely so there is not a
// second rule to get wrong, and a test that only checked the rotation would
// pass just as well against a cache that re-read the row on every request.
func TestARotatedSignerIsPickedUpOnTheRefreshWindow(t *testing.T) {
	f := newEnrolmentFixture(t)
	ctx := context.Background()

	first := newFakeSigner(t, f.atl.pki)
	creds := f.atl.credentials(t, defaultOrg)
	creds.SignerAddr = first.URL
	creds.SignerCAPEM = readFileString(t, first.pki.CAFile)
	creds.SignerClientCertPEM = readFileString(t, first.clientCert)
	creds.SignerClientKeyPEM = []byte(readFileString(t, first.clientKey))
	if err := f.srv.db.registerOrg(ctx, creds); err != nil {
		t.Fatal(err)
	}

	// Warm the cache, so this is genuinely a rotation rather than a first read.
	if _, _, err := f.srv.signerFor(ctx, defaultOrg); err != nil {
		t.Fatal(err)
	}

	second := newFakeSigner(t, f.atl.pki)
	creds.SignerAddr = second.URL
	creds.SignerCAPEM = readFileString(t, second.pki.CAFile)
	creds.SignerClientCertPEM = readFileString(t, second.clientCert)
	creds.SignerClientKeyPEM = []byte(readFileString(t, second.clientKey))
	if err := f.srv.db.registerOrg(ctx, creds); err != nil {
		t.Fatal(err)
	}

	// Inside the window the cached client is still served, and that is
	// deliberate: re-reading the row on every enrolment would cost a database
	// round trip per certificate.
	_, addr, err := f.srv.signerFor(ctx, defaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	if addr != first.URL {
		t.Errorf("the row was re-read inside the refresh window: address = %q", addr)
	}

	// Past it, the rotation takes effect. Without this a replaced signer needs
	// a console restart, and the old credential keeps working until then.
	f.srv.orgs.now = func() time.Time { return time.Now().Add(2 * orgClientRefresh) }

	_, addr, err = f.srv.signerFor(ctx, defaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	if addr != second.URL {
		t.Fatalf("signer address = %q, want the rotated one %q", addr, second.URL)
	}
}

// An organisation with no signer of its own falls back, and the fallback is the
// console's process-wide client.
func TestAnOrganisationWithoutItsOwnSignerFallsBack(t *testing.T) {
	f := newEnrolmentFixture(t)

	_, addr, err := f.srv.signerFor(context.Background(), defaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	if addr != f.srv.cfg.SignerAddr {
		t.Errorf("signer address = %q, want the process-wide %q", addr, f.srv.cfg.SignerAddr)
	}
}

// snapshot returns the requests this signer has received.
//
// Copied under the lock: the console calls the signer from a request goroutine,
// so reading the slice directly races with the append in handle.
func (f *fakeSigner) snapshot() []signerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]signerRequest, len(f.requests))
	copy(out, f.requests)
	return out
}
