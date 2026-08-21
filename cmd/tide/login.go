// tide login — enrol this machine as a caller.
//
//	tide login --url https://console.example:3443 --org acme --token <token>
//
// Generates a P-256 key HERE, sends only a certificate signing request, and
// writes what comes back to ~/.atlantis/<org>/<caller>/. The private key is
// created on the machine that will use it and never leaves.
//
// # There is no --caller
//
// The token determines which caller it enrols, on the console, from the row it
// spends. The signer then names the certificate from that value and takes only
// the public key from the request — so a name supplied here would decide
// nothing, and offering the flag would invite somebody to think it did.

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
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// enrolTimeout bounds the whole exchange. One request, one signature.
const enrolTimeout = 30 * time.Second

func cmdLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	url := fs.String("url", "", "enrolment listener URL, from the console's enrol dialog")
	org := fs.String("org", "", "organisation to enrol into")
	token := fs.String("token", "", "single-use enrolment token")
	// Local development only. A deployment's enrolment endpoint carries a
	// publicly-trusted certificate, so the system roots verify it and this flag
	// has nothing to do. It exists because the certificates deploy/init-certs.sh
	// makes are signed by a CA that is in no system store.
	caFile := fs.String("ca", "",
		"local development only: PEM root that verifies the enrolment listener")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Named individually. Three required flags is enough that "which one" is the
	// question somebody actually has.
	for _, r := range []struct{ flag, val string }{
		{"--url", *url},
		{"--org", *org},
		{"--token", *token},
	} {
		if r.val == "" {
			fmt.Fprintf(os.Stderr, "tide login: %s is required\n\n", r.flag)
			fmt.Fprintln(os.Stderr, "The console's Callers page prints the whole command.")
			return 2
		}
	}
	if !validStoreName(*org) {
		fmt.Fprintf(os.Stderr, "tide login: %q is not a valid organisation name\n", *org)
		return 2
	}

	bundle, err := enrol(*url, *org, *token, *caFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}

	dir, err := credDir(bundle.Org, bundle.Caller)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}
	if err := writeNewCredentials(bundle); err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}

	leaf, err := leafOf(bundle.ClientPEM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}

	cliout.Header(os.Stdout, "enrolled")
	cliout.Row(os.Stdout, "brass", bundle.Caller, "in "+bundle.Org)
	cliout.Row(os.Stdout, "muted", "endpoint", bundle.Endpoint)
	cliout.Row(os.Stdout, "muted", "expires", leaf.NotAfter.UTC().Format(time.RFC3339))
	cliout.Row(os.Stdout, "muted", "stored", dir)
	fmt.Println()
	fmt.Println("tide renews this on its own before it expires.")
	return 0
}

// enrolResponse is what the enrolment listener answers with.
type enrolResponse struct {
	CertPEM   string `json:"cert_pem"`
	CAPEM     string `json:"ca_pem"`
	Caller    string `json:"caller"`
	Org       string `json:"org"`
	Endpoint  string `json:"endpoint"`
	EnrollURL string `json:"enroll_url"`
	Error     string `json:"error"`
}

// enrol generates a key, spends the token, and returns what to store.
//
// The key exists only in this process until writeNewCredentials puts it on
// disk. Nothing sends it anywhere; the CSR carries the public half.
func enrol(baseURL, org, token, caFile string) (*storedCredentials, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}

	// An empty subject. The signer names the certificate from the caller the
	// console resolved from the token, so anything put here would be discarded —
	// and writing a guess would invite somebody to believe it mattered.
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return nil, fmt.Errorf("build a certificate request: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	client, err := enrolClient(caFile)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{
		"org": org, "token": token, "csr_pem": string(csrPEM),
	})
	if err != nil {
		return nil, err
	}

	resp, err := client.Post(baseURL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		// A trust failure is worth naming, because the raw error does not
		// describe the situation to anybody who has to act on it. On macOS it
		// reads "certificate is not standards compliant", which is Apple's
		// phrasing for one of its own rules and says nothing about trust.
		//
		// It deliberately does NOT suggest -ca. A deployment's enrolment
		// endpoint is publicly trusted, so for anyone but us this means the
		// deployment is misconfigured — and answering that with a flag that
		// bypasses verification is advice to work around a real security
		// failure. -ca exists for local development and is documented there.
		var ce *tls.CertificateVerificationError
		if errors.As(err, &ce) {
			// An expired certificate is the one trust failure that is often
			// this machine's fault — a clock hours out of true rejects a
			// perfectly good certificate. Saying "the deployment is broken"
			// there would repeat the mistake this whole branch exists to fix:
			// naming a cause that is not the cause.
			var ci x509.CertificateInvalidError
			if errors.As(err, &ci) && ci.Reason == x509.Expired {
				return nil, fmt.Errorf(
					"the certificate at %s is outside its validity window — "+
						"check this machine's clock before anything else", baseURL)
			}
			// The URL is already in the wrapped *url.Error, so it is not
			// repeated here.
			return nil, fmt.Errorf(
				"could not verify the server's certificate: %w — "+
					"the deployment is misconfigured", err)
		}
		return nil, fmt.Errorf("reach %s: %w", baseURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out enrolResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("the console returned a response tide could not read (status %d)",
			resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, fmt.Errorf("the console refused: %s", out.Error)
		}
		return nil, fmt.Errorf("the console refused with status %d", resp.StatusCode)
	}
	if out.Caller == "" || out.CertPEM == "" || out.CAPEM == "" {
		return nil, fmt.Errorf("the console's response is missing a certificate, "+
			"a CA or a caller name: %+v", out)
	}
	if out.Org != org {
		// The organisation is echoed back; a different one means something is
		// confused about which console this is, and storing under the returned
		// name would hide it.
		return nil, fmt.Errorf("asked to enrol into %q and the console answered for %q",
			org, out.Org)
	}

	// Key first, then certificate — the order every PEM bundle in the wild uses,
	// and irrelevant to tls.X509KeyPair, which searches for both.
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the key: %w", err)
	}
	var clientPEM bytes.Buffer
	if err := pem.Encode(&clientPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return nil, err
	}
	clientPEM.WriteString(out.CertPEM)

	return &storedCredentials{
		Org:       out.Org,
		Caller:    out.Caller,
		ClientPEM: clientPEM.Bytes(),
		CAPEM:     []byte(out.CAPEM),
		Endpoint:  out.Endpoint,
		EnrollURL: out.EnrollURL,
	}, nil
}

// enrolClient dials the enrolment listener.
//
// # Why --ca exists
//
// At this moment tide holds nothing: no certificate, no CA, no store entry. It
// still has to verify the listener it is about to hand a live token to. With no
// --ca it trusts the system roots, which is right for a console behind a
// publicly-trusted certificate and wrong for a private authority — including
// every local development stack, whose root is in no system store.
//
// Without the flag the acceptance test for this feature could not be run.
func enrolClient(caFile string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read --ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("--ca %s contains no certificate", caFile)
		}
		cfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   enrolTimeout,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}, nil
}
