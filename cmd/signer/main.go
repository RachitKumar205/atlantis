// Package main implements the atlantis cert signer: a narrow HTTP service that
// holds the intermediate CA private key and signs caller leaf certificates on
// behalf of the console.
//
// The signer never exports the CA key. It accepts a PEM-encoded CSR
// (POST /issue) and returns a signed leaf cert.  The CN in the CSR must
// match the caller name in the request body, and it must not be on the
// reserved-CN denylist — those names belong to atlantis infrastructure.
//
// # Who may ask
//
// /issue is mTLS, verified against SIGNER_CLIENT_CA, and the peer's CN must be
// in SIGNER_ALLOWED_CLIENT_CNS. Both are required; the signer refuses to start
// without them.
//
// SIGNER_CLIENT_CA must NOT be the CA this signer issues from. Every leaf it
// signs carries ExtKeyUsage: ClientAuth, so a signer trusting its own issuing
// authority would accept every certificate it has ever produced as a
// credential — and one caller could then mint another's identity. The
// allowlist is the second answer to the same question, because "a separate
// authority" is a property of how somebody deployed this, and an allowlist is
// a property of the code.
//
// /healthz answers on SIGNER_HEALTH_LISTEN in plaintext, because the container
// health check holds no certificate.
//
// # This is platform code living in the product repo
//
// It arrived with the self-host bundle, which is gone — the compose file, the
// systemd unit and the reverse-proxy configs went with it. The signer did not,
// because it is the only thing that issues caller certificates, and a managed
// atlantis needs that more than a self-hosted one did: every caller
// authenticates by client certificate, and the multi-org console needs one
// certificate per org so a scoping bug is refused at the handshake instead of
// returning another org's data.
//
// It belongs in atlantis-cloud, alongside provisioning and the CA it would
// serve. It is kept here until that move so the capability is not lost in the
// gap — deleting it would leave nothing able to issue a caller a certificate.
//
// The console dials it from the enrolment routes, through ATL_SIGNER_ADDR. That
// setting was documented and set by nothing for the whole time cert issuance
// was a button in the console that answered 503; `make dev-signer` is what runs
// this locally.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reservedCNs cannot be signed regardless of whether they are registered
// callers.  These are atlantis infrastructure identities; a compromised
// console must not be able to impersonate them.
var reservedCNs = map[string]bool{
	"atlantis":         true,
	"atlantis-console": true,
	"atlantis-signer":  true,
}

// certTTL is the lifetime of issued leaf certs. Short TTL means expiry
// acts as a natural revocation mechanism — no CRL/OCSP needed.
const certTTL = 90 * 24 * time.Hour

var (
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	// pgPool is non-nil iff PG_URL was set at boot. When nil the signer
	// falls back to the reserved-CN denylist only — sufficient for dev
	// where the bundled signer can't see atlantis's DB independently.
	// Production must set PG_URL so issuance is gated on a registered
	// caller_identities row.
	pgPool *pgxpool.Pool
)

// allowedClientCNs is who may ask for a certificate.
//
// Empty is not "everyone" — the signer refuses to start with it empty. See
// requireClientAuth for why a valid certificate is not, by itself, an answer to
// "may this peer mint identities".
var allowedClientCNs map[string]bool

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("signer refused to start", "err", err)
		os.Exit(1)
	}
}

// run is main with the exits taken out, so a test can boot the signer and read
// back why it refused.
//
// cmd/server was split the same way for the same reason: a startup guard that
// only ever calls os.Exit can be asserted on by a subprocess test at best, and
// not at all from inside the package.
func run(log *slog.Logger) error {
	caDir := envOr("CA_DIR", "/ca-private")
	listen := envOr("SIGNER_LISTEN", ":7070")
	healthListen := envOr("SIGNER_HEALTH_LISTEN", ":7071")

	var err error
	caCert, caKey, caPEM, err = loadCA(caDir)
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}

	clientCAs, err := loadClientCAs(os.Getenv("SIGNER_CLIENT_CA"))
	if err != nil {
		return err
	}
	allowedClientCNs, err = parseAllowedCNs(os.Getenv("SIGNER_ALLOWED_CLIENT_CNS"))
	if err != nil {
		return err
	}

	// Connect to atlantis's Postgres so issuance is gated on a registered
	// caller_identities row.
	//
	// Required, not optional. It used to be skipped entirely when PG_URL was
	// unset, and the comment here claimed that was fatal "in production
	// posture" — which was true only of the case that does not matter. A DSN
	// that is set and broken exited; a DSN that was absent silently reduced the
	// signer to a reserved-CN denylist, which is not a check on anything an
	// operator registered. An unset setting must not be a way to turn a gate
	// off, the same rule CLOUD_ISSUER is held to in the console.
	pgURL := os.Getenv("PG_URL")
	if pgURL == "" {
		return errors.New("PG_URL is required: without it the signer cannot tell " +
			"a registered caller from a name somebody chose, and would issue for either")
	}
	cfg, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return fmt.Errorf("parse PG_URL: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pgPool, err = pgxpool.NewWithConfig(ctx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to PG: %w", err)
	}
	defer pgPool.Close()

	log.Info("atlantis-signer ready",
		"ca_cn", caCert.Subject.CommonName,
		"ca_expires", caCert.NotAfter.Format(time.RFC3339),
		"listen", listen,
		"health_listen", healthListen,
		"allowed_client_cns", len(allowedClientCNs),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /issue", func(w http.ResponseWriter, r *http.Request) {
		if !requireClientAuth(w, r, log) {
			return
		}
		handleIssue(w, r, log)
	})

	// Health answers on its own plaintext port.
	//
	// Not fastidiousness: the container's HEALTHCHECK is a plain `wget http://`
	// (Dockerfile.signer) and it holds no client certificate — the image mounts
	// the CA directory and nothing else. Putting TLS on the one port the signer
	// used to have would leave the container permanently unhealthy, and
	// anything waiting on `condition: service_healthy` would never start.
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  clientCAs,
		},
	}
	healthSrv := &http.Server{
		Addr:              healthListen,
		Handler:           healthMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		_ = healthSrv.Shutdown(sctx)
	}()

	go func() {
		if herr := healthSrv.ListenAndServe(); !errors.Is(herr, http.ErrServerClosed) {
			log.Error("health listener exited", "err", herr)
		}
	}()

	// The certificate and key are the signer's own server identity, separate
	// from the CA it issues from.
	certFile := os.Getenv("SIGNER_TLS_CERT")
	keyFile := os.Getenv("SIGNER_TLS_KEY")
	if certFile == "" || keyFile == "" {
		return errors.New("SIGNER_TLS_CERT and SIGNER_TLS_KEY are required: " +
			"the signer holds a CA private key and does not serve plaintext")
	}
	if err := srv.ListenAndServeTLS(certFile, keyFile); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("signer exited: %w", err)
	}
	return nil
}

// loadClientCAs reads the pool of authorities whose certificates may CALL the
// signer.
//
// # This must not be the CA the signer issues from
//
// signCSR stamps every caller leaf with ExtKeyUsage: ClientAuth off the issuing
// CA. Trust that same CA here and every certificate the signer has ever issued
// becomes a valid credential to the signer — so caller `backend` dials in with
// its own legitimate certificate, asks for `payments`, and gets it, because
// `payments` is a registered caller and the CSR's CN matches the name in the
// body. It then authenticates as `payments`, because cert binding admits any
// CA-signed certificate for a caller whose fingerprint has never been recorded.
//
// A separate authority is the answer, and the CN allowlist below is the second
// answer, because "separate" is a deployment property and allowlists are not.
func loadClientCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, errors.New("SIGNER_CLIENT_CA is required: it names who may ask " +
			"for a certificate, and must not be the CA the signer issues from")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SIGNER_CLIENT_CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("SIGNER_CLIENT_CA %s contains no certificate", path)
	}
	return pool, nil
}

// parseAllowedCNs reads the comma-separated allowlist.
//
// Refuses empty rather than defaulting to a name, so a deployment that means
// `atlantis-console` has to say so. A default here would be a value nobody
// chose, protecting the most sensitive endpoint in the product.
func parseAllowedCNs(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, cn := range strings.Split(raw, ",") {
		if cn = strings.TrimSpace(cn); cn != "" {
			out[cn] = true
		}
	}
	if len(out) == 0 {
		return nil, errors.New("SIGNER_ALLOWED_CLIENT_CNS is required and must name " +
			"at least one common name (normally atlantis-console)")
	}
	return out, nil
}

// requireClientAuth checks the peer is allowed to ask, having already proved it
// is who it says.
//
// The handshake proves the certificate chains to SIGNER_CLIENT_CA. It does not
// say the holder should be minting identities — that is what this adds, and it
// is why the allowlist exists alongside a separate authority rather than
// instead of one.
func requireClientAuth(w http.ResponseWriter, r *http.Request, log *slog.Logger) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		// Unreachable while ClientAuth is RequireAndVerifyClientCert, and
		// checked anyway: the day somebody relaxes that to debug something, this
		// is what stops the relaxation from being silent.
		jsonError(w, "client certificate required", http.StatusUnauthorized)
		return false
	}
	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	if !allowedClientCNs[cn] {
		log.Warn("signer refused a peer", "cn", cn, "remote", r.RemoteAddr)
		jsonError(w, "this client may not request certificates", http.StatusForbidden)
		return false
	}
	return true
}

func handleIssue(w http.ResponseWriter, r *http.Request, log *slog.Logger) {
	var req struct {
		Caller string `json:"caller"`
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	caller := strings.TrimSpace(req.Caller)
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	if req.CSRPEM == "" {
		jsonError(w, "csr_pem is required", http.StatusBadRequest)
		return
	}
	if reservedCNs[caller] {
		jsonError(w, fmt.Sprintf("caller %q is a reserved infrastructure name and cannot be issued a cert", caller), http.StatusForbidden)
		return
	}

	// Defence in depth. The console verifies the caller is registered before
	// reaching us, and this is the layer that holds if the console is the thing
	// that is wrong.
	//
	// No longer conditional: run() refuses to start without PG_URL, so pgPool is
	// never nil here. It used to be skipped when the DSN was absent, which meant
	// the deployment with the least configuration had the fewest checks.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var registered bool
	err := pgPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM atlantis.caller_identities WHERE caller = $1)`,
		caller).Scan(&registered)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Error("identity lookup", "caller", caller, "err", err)
		jsonError(w, "identity lookup failed", http.StatusInternalServerError)
		return
	}
	if !registered {
		jsonError(w, fmt.Sprintf("caller %q is not registered — an operator must add it in the console first", caller), http.StatusForbidden)
		return
	}

	csr, err := parseCSR(req.CSRPEM)
	if err != nil {
		jsonError(w, "invalid CSR: "+err.Error(), http.StatusBadRequest)
		return
	}
	if csr.Subject.CommonName != caller {
		jsonError(w, fmt.Sprintf("CSR CN %q must match caller %q", csr.Subject.CommonName, caller), http.StatusBadRequest)
		return
	}

	certPEM, expiresAt, err := signCSR(csr)
	if err != nil {
		log.Error("sign CSR", "caller", caller, "err", err)
		jsonError(w, "signing failed", http.StatusInternalServerError)
		return
	}

	log.Info("issued cert",
		"caller", caller,
		"expires", expiresAt.Format(time.RFC3339),
		"remote", r.RemoteAddr,
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"cert_pem":   certPEM,
		"ca_pem":     string(caPEM),
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

func signCSR(csr *x509.CertificateRequest) (string, time.Time, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(certTTL)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		// Slight backdate to tolerate clock skew between containers.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              expiresAt,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA:                  false,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	return certPEM, expiresAt, nil
}

func parseCSR(pemStr string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a valid PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	return csr, csr.CheckSignature()
}

// loadCA reads the CA certificate and private key from dir.
// The key is expected to be a SEC1 EC private key (openssl ecparam output).
func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	crtPath := dir + "/ca.crt"
	crtBytes, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", crtPath, err)
	}
	block, _ := pem.Decode(crtBytes)
	if block == nil {
		return nil, nil, nil, fmt.Errorf("%s: no PEM block", crtPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", crtPath, err)
	}

	keyPath := dir + "/ca.key"
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil {
		return nil, nil, nil, fmt.Errorf("%s: no PEM block", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}

	return cert, key, crtBytes, nil
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
