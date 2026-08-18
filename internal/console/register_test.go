package console

import (
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// What a registration has to carry before it is written.
//
// These are cheap checks of things the TLS handshake would also catch, and the
// value is entirely in *when*. A mis-registered organisation that reaches the
// database is discovered at somebody's next page load, as a 503 naming TLS,
// long after the command that caused it printed nothing and exited 0 — and the
// person reading that error is usually not the person who ran the command.
func TestValidateOrgCredentials(t *testing.T) {
	good := goodCredentials(t)

	// Accepted first. Without this the table below passes against a function
	// that rejects everything, which is the failure mode a table of negatives
	// cannot see.
	if err := validateOrgCredentials(good); err != nil {
		t.Fatalf("a complete registration was refused: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*orgCredentials)
		wantErr string
	}{
		{
			// The organisation is also the associated data the key is sealed
			// against, so an empty one would be refused a moment later by the
			// keyring with a message about ciphertext rather than about this.
			name:    "no organisation",
			mutate:  func(c *orgCredentials) { c.Org = "" },
			wantErr: "organisation name is required",
		},
		{
			name:    "no endpoint",
			mutate:  func(c *orgCredentials) { c.Endpoint = "" },
			wantErr: "endpoint is required",
		},
		{
			// A bare hostname is the plausible mistake, and it dials nothing.
			name:    "endpoint without a port",
			mutate:  func(c *orgCredentials) { c.Endpoint = "atlantis.acme.internal" },
			wantErr: "not host:port",
		},
		{
			// The health page reaches atlantis over plain HTTP on a different
			// port and never touches the gRPC client, so nothing else in this
			// path would notice it missing.
			name:    "no health address",
			mutate:  func(c *orgCredentials) { c.HealthAddr = "" },
			wantErr: "health address is required",
		},
		{
			name:    "health address without a port",
			mutate:  func(c *orgCredentials) { c.HealthAddr = "atlantis.acme.internal" },
			wantErr: "not host:port",
		},
		{
			name:    "CA is not PEM",
			mutate:  func(c *orgCredentials) { c.CAPEM = "not a certificate" },
			wantErr: "no usable certificates",
		},
		{
			// The likely mistake at the command line, and the one that would
			// otherwise surface as a handshake failure against a healthy
			// server.
			name: "certificate and key swapped",
			mutate: func(c *orgCredentials) {
				c.CertPEM, c.KeyPEM = string(c.KeyPEM), []byte(c.CertPEM)
			},
			wantErr: "not a pair",
		},
		{
			name: "key belongs to a different certificate",
			mutate: func(c *orgCredentials) {
				other := testpki.New(t, t.TempDir())
				_, keyFile := other.ClientCert(t, "atlantis-console")
				c.KeyPEM = readFile(t, keyFile)
			},
			wantErr: "not a pair",
		},
		{
			name:    "certificate is not PEM",
			mutate:  func(c *orgCredentials) { c.CertPEM = "" },
			wantErr: "not a pair",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCredentials(t)
			tc.mutate(&c)

			err := validateOrgCredentials(c)
			if err == nil {
				t.Fatalf("accepted a registration with %s", tc.name)
			}
			// On content, because every case here is refused by *some* branch
			// and a test satisfied by any error would pass with all but one of
			// them deleted.
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error does not mention %q: %v", tc.wantErr, err)
			}
		})
	}
}

// An expired certificate is refused at registration.
//
// Separate from the table because it needs a PKI that mints one, and worth
// having at all because the handshake failure it prevents reads as the *server*
// being misconfigured — so an operator debugging it starts on the wrong
// machine.
func TestValidateOrgCredentialsRefusesAnExpiredCertificate(t *testing.T) {
	c := goodCredentials(t)

	pki := testpki.New(t, t.TempDir())
	certFile, keyFile := pki.ExpiredClientCert(t, "atlantis-console")
	c.CertPEM = string(readFile(t, certFile))
	c.KeyPEM = readFile(t, keyFile)

	err := validateOrgCredentials(c)
	if err == nil {
		t.Fatal("an expired client certificate was accepted")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the error does not say the certificate expired: %v", err)
	}
}

// goodCredentials is a registration that should be accepted.
func goodCredentials(t *testing.T) orgCredentials {
	t.Helper()
	pki := testpki.New(t, t.TempDir())
	certFile, keyFile := pki.ClientCert(t, "atlantis-console")
	return orgCredentials{
		Org:        "acme",
		Endpoint:   "atlantis.acme.internal:9090",
		HealthAddr: "atlantis.acme.internal:8081",
		CAPEM:      string(readFile(t, pki.CAFile)),
		CertPEM:    string(readFile(t, certFile)),
		KeyPEM:     readFile(t, keyFile),
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
