package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Automatic renewal.
//
// Triggers at two thirds of the certificate's life, which is step-ca's rule and
// the one `tide login` prints. Computed from the certificate's own NotBefore and
// NotAfter, so changing certTTL moves the threshold with it.
//
// Runs once per process. A single `tide plan` dials twice, once to refresh the
// cache and once to plan, and renewing per dial would issue two certificates
// and two audit rows for one command.
//
// No lock. Two tide processes in one repository can renew concurrently; both
// get a valid certificate, both write by atomic rename, and the loser still
// holds a certificate for the same caller, since migration 0032 stopped pinning
// a caller to one certificate.
//
// A failure is a warning. The certificate has not expired, so refusing to run
// would turn a console outage into a caller outage.
var renewOnce sync.Once

// renewalFraction is how much of a certificate's life must elapse before tide
// replaces it. Two thirds, per step-ca.
const renewalFraction = 2.0 / 3.0

// renewIfDue replaces store-held credentials that are close to expiry.
//
// Only credentials tide itself wrote. tide cannot write back to an environment
// variable, so renewing material supplied that way rotates the identity and
// discards the replacement, locking the caller out on its next run. That is the
// CI case.
func renewIfDue(c *tideConfig) {
	renewOnce.Do(func() {
		if c.storeDir == "" || c.storeEnrollURL == "" {
			return
		}
		leaf, err := leafOf([]byte(c.TLS.CertPEM))
		if err != nil {
			return
		}
		if !renewalDue(leaf, time.Now()) {
			return
		}
		if err := renewNow(c, leaf); err != nil {
			// Warning, not an error. See the note at the top.
			fmt.Fprintf(os.Stderr,
				"tide: could not renew this machine's certificate (%v).\n"+
					"      It is still valid until %s; tide will try again.\n",
				err, leaf.NotAfter.UTC().Format(time.RFC3339))
		}
	})
}

// renewalDue reports whether two thirds of the certificate's life has passed.
func renewalDue(leaf *x509.Certificate, now time.Time) bool {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if life <= 0 {
		// A certificate whose validity window is empty or inverted is not
		// something to reason about; leave it alone and let the handshake say so.
		return false
	}
	elapsed := now.Sub(leaf.NotBefore)
	return float64(elapsed) >= float64(life)*renewalFraction
}

// renewNow presents the current certificate and stores what comes back.
func renewNow(c *tideConfig, _ *x509.Certificate) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate a key: %w", err)
	}
	// Empty subject, as at enrolment: the certificate is named by the console
	// from the row the presented certificate resolves to.
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return fmt.Errorf("build a certificate request: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// mTLS with the certificate being replaced. That is the whole of renewal's
	// authentication — there is no token, because a token would be a long-lived
	// minting credential sitting on disk.
	pair, err := tls.X509KeyPair([]byte(c.TLS.CertPEM), []byte(c.TLS.KeyPEM))
	if err != nil {
		return fmt.Errorf("load the current certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(c.TLS.CAPEM)) {
		return fmt.Errorf("the stored CA holds no certificate")
	}
	client := &http.Client{
		Timeout: enrolTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
			RootCAs:      pool,
		}},
	}

	body, err := json.Marshal(map[string]string{"csr_pem": string(csrPEM)})
	if err != nil {
		return err
	}
	resp, err := client.Post(c.storeEnrollURL+"/renew", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.storeEnrollURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out enrolResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return fmt.Errorf("unreadable response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return fmt.Errorf("refused: %s", out.Error)
		}
		return fmt.Errorf("refused with status %d", resp.StatusCode)
	}
	if out.CertPEM == "" {
		return fmt.Errorf("the response carried no certificate")
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode the key: %w", err)
	}
	var clientPEM bytes.Buffer
	if err := pem.Encode(&clientPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return err
	}
	clientPEM.WriteString(out.CertPEM)

	if err := replaceClientPEM(c.storeDir, clientPEM.Bytes()); err != nil {
		return err
	}

	// Use the new material for this run too. Without this the command that
	// triggered the renewal would still dial with the certificate it just
	// replaced — correct, since both are valid, but confusing to debug.
	c.TLS.CertPEM = clientPEM.String()
	c.TLS.KeyPEM = clientPEM.String()
	return nil
}
