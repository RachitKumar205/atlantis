// Package main implements the atlantis cert signer: a narrow HTTP service that
// holds the intermediate CA private key and signs caller leaf certificates on
// behalf of the console.
//
// The signer never exports the CA key. It accepts a PEM-encoded CSR
// (POST /issue) and returns a signed leaf cert. The certificate is named from
// the caller in the request body, NOT from the CSR — only the public key is
// taken from the request. That name must not be on the reserved-CN denylist,
// which covers atlantis's own infrastructure identities.
//
// /issue is mTLS, verified against SIGNER_CLIENT_CA, and the peer's CN must be
// in SIGNER_ALLOWED_CLIENT_CNS. Both are required; the signer refuses to start
// without them.
//
// SIGNER_CLIENT_CA must not be the CA this signer issues from. Every leaf it
// signs carries ExtKeyUsage: ClientAuth, so a signer trusting its own issuing
// authority accepts every certificate it has produced as a credential, and one
// caller can mint another's identity.
//
// The allowlist answers the same question in code, where a separate authority
// is a property of the deployment.
//
// /healthz answers on SIGNER_HEALTH_LISTEN in plaintext, because the container
// health check holds no certificate.
//
// This is the only component that issues caller certificates. Every caller
// authenticates by client certificate, and the multi-organisation console needs
// one certificate per organisation so a scoping bug is refused at the handshake
// rather than returning another organisation's data.
//
// It belongs in atlantis-cloud, beside provisioning and the CA it would serve,
// and stays here until that move: nothing else can issue a caller a
// certificate.
//
// The console dials it from the enrolment routes through ATL_SIGNER_ADDR. With
// that unset, console cert issuance answers 503. `make dev-signer` runs it
// locally.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

// certTTL is the lifetime of an issued leaf.
//
// Seven days is short enough for expiry to serve as passive revocation, which
// is what migration 0032 left in place of fingerprint pinning. smallstep puts
// step-ca service certificates at one month or less for the same reason; SPIRE
// defaults SVIDs to one hour and pins nothing, which is not reachable here,
// where callers are laptops and build runners rather than workloads beside a
// co-located agent.
//
// tide renews at two thirds elapsed, so a machine refreshes near day five with
// two days of slack.
const certTTL = 7 * 24 * time.Hour

var (
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	// pgPool is atlantis's database, and issuance is gated on what it says.
	//
	// Never nil in a running signer: run() refuses to start without PG_URL. This
	// used to describe a fallback to the reserved-CN denylist when the DSN was
	// absent, which was the deployment with the least configuration getting the
	// fewest checks — an unset setting must not be a way to turn a gate off.
	//
	// Tests set it directly, which is the only case where it varies.
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

	// The signer's own server identity.
	//
	// Checked here, with the rest of the configuration, rather than at the
	// ListenAndServeTLS call that consumes it. Everything that can be decided
	// by reading the environment is decided before this process connects
	// anywhere, so an operator setting it up sees every configuration problem
	// at once instead of one per restart — and a missing certificate is not
	// reported as a database failure because the database happened to be
	// checked first.
	certFile := os.Getenv("SIGNER_TLS_CERT")
	keyFile := os.Getenv("SIGNER_TLS_KEY")
	if certFile == "" || keyFile == "" {
		return errors.New("SIGNER_TLS_CERT and SIGNER_TLS_KEY are required: " +
			"the signer holds a CA private key and does not serve plaintext")
	}

	// Connect to atlantis's Postgres so issuance is gated on a registered
	// caller_identities row.
	//
	// Required. Absent, the signer falls back to a reserved-CN denylist, which
	// checks nothing that was registered — an unset setting is not a way to
	// turn a gate off.
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
	defer cancel()
	pgPool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect to PG: %w", err)
	}
	defer pgPool.Close()

	// Prove the connection, rather than assume it.
	//
	// pgxpool.NewWithConfig is lazy: it validates the DSN and opens nothing. So
	// a signer pointed at an unreachable database started perfectly happily and
	// then failed the registration check on every issuance, reporting "identity
	// lookup failed" — which reads as a problem with the caller, not with this
	// process's configuration. The whole reason PG_URL became required is that
	// the check behind it must actually run.
	if err := pgPool.Ping(ctx); err != nil {
		return fmt.Errorf("connect to PG: %w", err)
	}

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

	// certFile and keyFile were validated at the top, with the rest of the
	// configuration. They are the signer's own server identity, separate from
	// the CA it issues from.
	if err := srv.ListenAndServeTLS(certFile, keyFile); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("signer exited: %w", err)
	}
	return nil
}

// loadClientCAs reads the pool of authorities whose certificates may CALL the
// signer.
//
// This must not be the CA the signer issues from.
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
// `atlantis-console` has to say so.
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
		// Unreachable while ClientAuth is RequireAndVerifyClientCert. Checked
		// anyway, so relaxing that setting is not silent.
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

// callerMayBeIssuedTo reports whether this caller still has a live identity.
//
// It reads atlantis.active_caller_identities, not the table. Since migration
// 0033 a revoked caller keeps its caller_identities row, so the table answers
// "it exists" for exactly the caller this refuses, and the signer would go on
// issuing certificates to a caller the server rejects.
//
// A function rather than a query inline in the handler, so a test can reach it
// with a database and a caller instead of CA material and a signed request.
func callerMayBeIssuedTo(ctx context.Context, caller string) (bool, error) {
	var registered bool
	err := pgPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM atlantis.active_caller_identities WHERE caller = $1)`,
		caller).Scan(&registered)
	// EXISTS always returns a row, so ErrNoRows here would mean something other
	// than "not registered". Tolerated rather than treated as an error because
	// the false it produces is the safe answer either way.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return registered, nil
}

func handleIssue(w http.ResponseWriter, r *http.Request, log *slog.Logger) {
	var req struct {
		Caller string `json:"caller"`
		CSRPEM string `json:"csr_pem"`
		// TTLSeconds may shorten the certificate's life and can never lengthen
		// it: the value is clamped to [minTTL, certTTL], and absent means the
		// full term. The console passes an hour for a CI workload's
		// certificate; the ceiling stays with the key.
		TTLSeconds int `json:"ttl_seconds"`
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

	// The console checks registration before the request arrives; this is the
	// layer that holds when the console is what is wrong.
	//
	// Unconditional: run() refuses to start without PG_URL, so pgPool is never
	// nil here.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	registered, err := callerMayBeIssuedTo(ctx, caller)
	if err != nil {
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
	// No CN comparison. signCSR builds the subject from `caller` and takes only
	// the public key from the request, so whatever the CSR asks to be called has
	// stopped deciding anything — ignoring it is stronger than comparing it,
	// because there is no check left to forget.
	//
	// `caller` is the console's, from the row it spent the enrolment token
	// against. It has already been checked against the reserved names and
	// against caller_identities above.
	certPEM, expiresAt, err := signCSR(csr, caller, clampTTL(req.TTLSeconds))
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

// signCSR issues a leaf for caller, using only the public key from the request.
//
// The subject is built here, not copied from the CSR. `Subject: csr.Subject`
// takes organisation, unit and locality from a document the requester wrote
// while validating only the common name.
//
// `caller` comes from the token row the console spent, so the CSR's common name
// decides nothing. A CSR asking for another name is ignored rather than
// refused, which needs no comparison to stay true.
// clampTTL bounds a requested lifetime to [minTTL, certTTL]. Zero and
// negative mean the full term.
func clampTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return certTTL
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl < minTTL {
		return minTTL
	}
	if ttl > certTTL {
		return certTTL
	}
	return ttl
}

// minTTL is the floor a requested lifetime is raised to. Below it a
// certificate can expire between issuance and first use.
const minTTL = 5 * time.Minute

func signCSR(csr *x509.CertificateRequest, caller string, ttl time.Duration) (string, time.Time, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(ttl)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: caller},
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
//
// The key may be SEC 1 or PKCS#8. It used to accept only SEC 1, which is what
// `openssl ecparam -genkey` writes and therefore what deploy/init-certs.sh had
// always produced — so the narrower parser went unnoticed for as long as a
// shell script was the only thing feeding this. internal/cloud/provision/certs
// mints the same authority in Go and marshals PKCS#8, and the signer refused it
// at boot with `parse /ca-private/ca.key`.
//
// Accepting both is the fix rather than changing what the generator emits: a CA
// key that is perfectly valid and merely wrapped differently should not be a
// boot failure, and this is the only reader in the tree fussy enough to care.
// Everything else goes through tls.X509KeyPair, which has always taken either —
// which is precisely why the gap was invisible.
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
	key, err := parseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}

	return cert, key, crtBytes, nil
}

// parseECPrivateKey accepts an EC private key in either SEC 1 or PKCS#8 form.
//
// SEC 1 is tried first because it is what every certificate this repo has
// issued so far carries, so the common path stays one call. The PKCS#8 error is
// the one reported when both fail: it is the more informative of the two, and a
// key that is neither is far more likely to be PKCS#8-shaped than SEC 1-shaped.
func parseECPrivateKey(der []byte) (*ecdsa.PrivateKey, error) {
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	any8, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := any8.(*ecdsa.PrivateKey)
	if !ok {
		// Named rather than generic: the signer issues EC leaves from an EC
		// authority, and an RSA CA here would otherwise fail later with a
		// signature error that says nothing about the key.
		return nil, fmt.Errorf("want an EC private key, got %T", any8)
	}
	return key, nil
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
