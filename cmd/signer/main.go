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
// Seven days. It was ninety, with a comment saying that expiry "acts as a
// natural revocation mechanism" — which was the intent and not the effect: a
// leaked certificate that keeps working for three months is not revoked by its
// expiry in any sense an operator would recognise. What actually provided
// revocation was fingerprint pinning, and pinning is what produced the lockout
// class, the overlap window and the one-way door around enrolment.
//
// Seven days makes the original claim true instead. It sits inside smallstep's
// published guidance for step-ca, which puts service certificates at "one month
// or less" and defaults to passive revocation for this reason; SPIRE issues
// SVIDs with a one-hour default and pins nothing at all. One hour is not
// reachable here — atlantis callers are laptops and build runners, not
// workloads beside a co-located agent — but ninety days was well outside the
// band anybody operates in.
//
// tide renews at two thirds elapsed, so a machine refreshes around day five and
// has two days of slack before anything stops working. Migration 0032 removed
// the pinning this replaces.
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

// callerMayBeIssuedTo reports whether this caller still has a live identity.
//
// # Why it reads a view
//
// A revoked caller keeps its caller_identities row since migration 0033, so the
// table would answer "yes, it exists" for precisely the caller this refuses.
// atlantis.active_caller_identities is the filtered set.
//
// Getting this wrong is quiet in the worst way. The server would refuse the
// revoked caller while the signer kept issuing it fresh certificates — a
// revocation that looks complete from the console and is contradicted by the
// component whose whole job is handing out credentials.
//
// # Why it is a function rather than a query inside the handler
//
// So that it can be tested. It was inline, and nothing exercised it: the fuzz
// test sets pgPool to nil and says the identity check is "tested elsewhere",
// and elsewhere did not exist. A handler test would need CA material and a
// signed request to reach one SELECT; this needs a database and a caller.
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
	certPEM, expiresAt, err := signCSR(csr, caller)
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
// # The subject is built here, not copied from the CSR
//
// It used to be `Subject: csr.Subject`, which took the whole subject from a
// document the requester wrote — organisation, unit, locality, everything —
// and validated only the common name. Nothing downstream reads those fields
// today, which is the sort of thing that stops being true quietly.
//
// Building it from `caller` also removes a step from the client: `tide login`
// no longer has to be told which caller it is enrolling as, because the CSR's
// common name has stopped deciding anything. The token decides, the console
// reads the caller off the row it spent, and that name is what appears here.
// A CSR that asks for something else is not refused — it is ignored, which is
// a stronger property than a comparison somebody has to remember to make.
func signCSR(csr *x509.CertificateRequest, caller string) (string, time.Time, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(certTTL)

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
