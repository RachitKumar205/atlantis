package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The credential store `tide login` writes and every later command reads.
//
// Two properties here are the ones that would be invisible if wrong: the key's
// file mode, and the fact that login refuses to replace a key while renewal
// must. Those two pull in opposite directions and were contradictory in the
// first draft of this design — login was specified with O_EXCL and renewal was
// specified to "rewrite the store", which cannot both be true of one call.

// storeFixture points the store at a temporary directory.
func storeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ATLANTIS_HOME", dir)
	return dir
}

// fakeCredentials builds a real key and a real self-signed certificate, so the
// bytes on disk parse the way production's would.
func fakeCredentials(t *testing.T, org, caller string) *storedCredentials {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: caller},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return &storedCredentials{
		Org: org, Caller: caller,
		ClientPEM: []byte(b.String()),
		CAPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Endpoint:  "atlantis.example:9090",
		EnrollURL: "https://console.example:3443",
	}
}

// TestTheKeyIsWrittenPrivately.
//
// 0600 on the file and 0700 on the directory. Both asserted, because MkdirAll
// leaves an existing directory's mode alone — so a store created by something
// with a laxer umask would keep it, and a test that only ever ran against a
// fresh temporary directory would never notice.
func TestTheKeyIsWrittenPrivately(t *testing.T) {
	root := storeFixture(t)

	// Pre-create the org directory world-readable, which is the case a fresh
	// TempDir would hide.
	if err := os.MkdirAll(filepath.Join(root, "acme", "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeNewCredentials(fakeCredentials(t, "acme", "backend")); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := filepath.Join(root, "acme", "backend")
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
	fi, err := os.Stat(filepath.Join(dir, clientPEMName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("%s mode = %o, want 600", clientPEMName, got)
	}
}

// The existing key belongs to a certificate atlantis probably still accepts.
// Overwriting it leaves the machine holding material for an identity it cannot
// prove, and nothing records the swap.
func TestLoginRefusesToReplaceAKey(t *testing.T) {
	storeFixture(t)
	first := fakeCredentials(t, "acme", "backend")
	if err := writeNewCredentials(first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err := writeNewCredentials(fakeCredentials(t, "acme", "backend"))
	if err == nil {
		t.Fatal("a second login replaced an existing key")
	}
	if !strings.Contains(err.Error(), "already holds credentials") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}

	// And the original is untouched — a refusal that had already clobbered the
	// file would be worse than no refusal at all.
	got, err := loadCredentials("acme", "backend")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got.ClientPEM) != string(first.ClientPEM) {
		t.Error("the refused write still changed the key")
	}
}

// TestRenewalReplacesTheKeyAtomically.
//
// The counterpart to the test above, and the reason O_EXCL is scoped to
// creation. Renewal is meant to replace: it has just proved possession of the
// certificate being replaced.
func TestRenewalReplacesTheKeyAtomically(t *testing.T) {
	root := storeFixture(t)
	if err := writeNewCredentials(fakeCredentials(t, "acme", "backend")); err != nil {
		t.Fatal(err)
	}
	renewed := fakeCredentials(t, "acme", "backend")

	dir := filepath.Join(root, "acme", "backend")
	if err := replaceClientPEM(dir, renewed.ClientPEM); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := loadCredentials("acme", "backend")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got.ClientPEM) != string(renewed.ClientPEM) {
		t.Error("the store still holds the old material")
	}
	// No temporary left behind. One would be harmless but it would also be the
	// first sign that the rename did not happen.
	if _, err := os.Stat(filepath.Join(dir, clientPEMName+".tmp")); !os.IsNotExist(err) {
		t.Error("a temporary file survived the rename")
	}
	// And the mode survived the replacement.
	fi, err := os.Stat(filepath.Join(dir, clientPEMName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("after renewal the key is %o, want 600", got)
	}
}

// TestTheStoredPairLoadsAsATLSCertificate.
//
// The key and the certificate share one file so renewal is a single atomic
// rename. That only works because tls.X509KeyPair searches each buffer
// independently — hand it the same bytes twice and it finds both halves. If
// that ever stopped being true, every command would fail at dial time with a
// message about the key not matching the certificate.
func TestTheStoredPairLoadsAsATLSCertificate(t *testing.T) {
	storeFixture(t)
	if err := writeNewCredentials(fakeCredentials(t, "acme", "backend")); err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials("acme", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(got.ClientPEM, got.ClientPEM); err != nil {
		t.Fatalf("the stored pair does not load: %v", err)
	}
}

// TestTwoCallersInOneOrganisationCoexist.
//
// One developer, one organisation, two repositories is ordinary. Keyed by
// organisation alone — which is how this was first specified — the second
// enrolment would have had to overwrite the first machine's key.
func TestTwoCallersInOneOrganisationCoexist(t *testing.T) {
	storeFixture(t)
	for _, caller := range []string{"backend", "vendor"} {
		if err := writeNewCredentials(fakeCredentials(t, "acme", caller)); err != nil {
			t.Fatalf("enrol %s: %v", caller, err)
		}
	}
	for _, caller := range []string{"backend", "vendor"} {
		got, err := loadCredentials("acme", caller)
		if err != nil {
			t.Fatalf("load %s: %v", caller, err)
		}
		leaf, err := leafOf(got.ClientPEM)
		if err != nil {
			t.Fatal(err)
		}
		if leaf.Subject.CommonName != caller {
			t.Errorf("%s holds a certificate for %q", caller, leaf.Subject.CommonName)
		}
	}
	callers, err := callersInStore("acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 2 {
		t.Errorf("callersInStore = %v, want two", callers)
	}
}

// TestAPathComponentCannotEscapeTheStore.
//
// The organisation arrives from a flag and the caller from a server response,
// and both are joined into a filesystem path. `../..` has to be impossible
// rather than merely unlikely.
func TestAPathComponentCannotEscapeTheStore(t *testing.T) {
	storeFixture(t)
	for _, tc := range []struct{ org, caller string }{
		{"../../etc", "backend"},
		{"acme", "../../../tmp/evil"},
		{"acme/../..", "backend"},
		{"", "backend"},
		{"acme", ""},
		{"ACME", "backend"},
		{"-acme", "backend"},
		{strings.Repeat("a", 65), "backend"},
	} {
		if _, err := credDir(tc.org, tc.caller); err == nil {
			t.Errorf("credDir(%q, %q) was accepted", tc.org, tc.caller)
		}
	}
}

// An empty store is not an error — it is the state before anybody has logged in.
func TestAnEmptyStoreIsNotAnError(t *testing.T) {
	storeFixture(t)

	if _, err := loadCredentials("acme", "backend"); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("load on an empty store: %v", err)
	}
	orgs, err := orgsInStore()
	if err != nil {
		t.Fatalf("orgsInStore on an empty store: %v", err)
	}
	if len(orgs) != 0 {
		t.Errorf("orgsInStore = %v, want none", orgs)
	}
	callers, err := callersInStore("acme")
	if err != nil {
		t.Fatalf("callersInStore for an unknown org: %v", err)
	}
	if len(callers) != 0 {
		t.Errorf("callersInStore = %v, want none", callers)
	}
}

// The expiry comes off the certificate, not off a file beside it.
//
// A stored copy would be a second source that can disagree with the thing it
// describes, and renewal decides what to do from exactly this value.
func TestTheExpiryIsReadFromTheCertificate(t *testing.T) {
	storeFixture(t)
	if err := writeNewCredentials(fakeCredentials(t, "acme", "backend")); err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials("acme", "backend")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := leafOf(got.ClientPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("NotAfter = %v, want the certificate's %v", got.NotAfter, leaf.NotAfter)
	}
	if got.NotAfter.IsZero() {
		t.Error("no expiry was read at all")
	}
}

// TestRenewalIsDueAtTwoThirds.
//
// The threshold is computed from the certificate's own window, not from a
// constant, so changing certTTL moves it and there is no second place to keep in
// step. These pin the boundary from both sides — a test that only checked "an
// old certificate renews" would pass on a build that renewed constantly.
func TestRenewalIsDueAtTwoThirds(t *testing.T) {
	// A seven-day certificate, as the signer issues.
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	leaf := &x509.Certificate{
		NotBefore: start,
		NotAfter:  start.Add(7 * 24 * time.Hour),
	}

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fresh", start.Add(time.Minute), false},
		{"halfway", start.Add(3*24*time.Hour + 12*time.Hour), false},
		{"just before two thirds", start.Add(4*24*time.Hour + 15*time.Hour), false},
		{"just after two thirds", start.Add(4*24*time.Hour + 17*time.Hour), true},
		{"nearly expired", start.Add(6 * 24 * time.Hour), true},
		{"expired", start.Add(8 * 24 * time.Hour), true},
	} {
		if got := renewalDue(leaf, tc.at); got != tc.want {
			t.Errorf("%s: renewalDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A certificate with no validity window is left alone.
//
// Nothing sensible can be computed from it, and guessing would either renew on
// every command or never. The handshake is the right place for that to surface.
func TestAnEmptyValidityWindowIsNotRenewed(t *testing.T) {
	now := time.Now()
	for _, leaf := range []*x509.Certificate{
		{NotBefore: now, NotAfter: now},
		{NotBefore: now, NotAfter: now.Add(-time.Hour)},
		{},
	} {
		if renewalDue(leaf, now) {
			t.Errorf("a certificate with window %v..%v was considered renewable",
				leaf.NotBefore, leaf.NotAfter)
		}
	}
}

// Renewal is scoped to credentials tide wrote.
//
// Material from tide.yaml or the environment belongs to whoever put it there.
// tide cannot write back to an environment variable, so renewing it would
// rotate the identity at atlantis and discard the replacement — locking the
// caller out on its next run. That is the CI case exactly.
func TestRenewalIgnoresCredentialsTideDidNotWrite(t *testing.T) {
	storeFixture(t)
	c := &tideConfig{Caller: "backend"}
	c.TLS.CertPEM = "whatever came from the environment"
	c.TLS.KeyPEM = "likewise"

	// storeDir is empty, so this must do nothing at all — in particular it must
	// not try to parse the material or reach the network.
	renewIfDue(c)

	if c.TLS.CertPEM != "whatever came from the environment" {
		t.Error("renewal touched credentials it does not own")
	}
}
