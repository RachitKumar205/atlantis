package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/console/cloudauth"
	"github.com/rachitkumar205/atlantis/internal/console/oidcfed"
)

// Enrolment: how a machine gets a client certificate for a caller.
//
// The machine that will use the key generates it, and only the CSR travels. The
// console never holds a caller's private key.
//
// Two routes with different authorities. An admin mints a token on the
// console's normal API, behind session, role, CSRF and sudo. A machine redeems
// it on the enrolment listener, which has no session and no cookies: the token
// is the whole of what authorises it.
//
// Redemption is on a separate listener. The console's main mux ends in a `/`
// catch-all serving the SPA, so mounting these there would publish the entire
// console API on a port every enrolling machine can reach.

// buildEnrollListener prepares the second listener.
//
// It builds its own mux. buildMux ends with a `/` catch-all serving the SPA,
// and *Server is itself the handler for the main listener, so reusing either
// would put the whole console API on a port every enrolling machine can reach,
// behind none of the terminator, ingress limits or WAF the main listener sits
// behind.
//
// The client certificate is requested, never required: enrolment arrives with
// no certificate and renewal arrives with one. Each handler asserts what it
// needs, and handleRenew checks for a peer certificate itself rather than
// assuming the listener did.
//
// No browser reaches this port, so requesting a client certificate here does
// not raise a certificate-selection prompt.
func (s *Server) buildEnrollListener() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", s.handleEnroll)
	mux.HandleFunc("POST /enroll/oidc", s.handleEnrollOIDC)
	mux.HandleFunc("POST /renew", s.handleRenew)

	// RequestClientCert, and the certificate is verified in handleRenew.
	//
	// The handshake cannot verify: each organisation has its own client CA, and
	// tls.Config carries one ClientCAs pool. handleRenew resolves the
	// organisation from the certificate's fingerprint, so the authority to
	// verify against is known there and nowhere earlier.
	//
	// This is not fail-closed. handleRenew must verify, and a route added here
	// that reads r.TLS without verifying trusts an unverified certificate.
	// There are two routes and no catch-all, and
	// TestACertificateFromAnotherAuthorityCannotRenew asserts the property
	// rather than leaving it implied by this configuration.
	//
	// Proof of possession still holds. A client that sends a certificate must
	// also send CertificateVerify, and crypto/tls checks that signature against
	// the presented public key whatever ClientAuth is set to, so a copied
	// certificate does not permit renewal.
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequestClientCert,
	}

	s.enrollSrv = &http.Server{
		Addr:              s.cfg.EnrollListen,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return nil
}

// ServeEnrollment runs the enrolment listener until it is shut down.
//
// Returns nil immediately when enrolment is not configured, so a caller does
// not have to know whether the feature is on.
func (s *Server) ServeEnrollment() error {
	if s.enrollSrv == nil {
		return nil
	}
	s.log.Info("enrolment listener", "addr", s.cfg.EnrollListen)
	err := s.enrollSrv.ListenAndServeTLS(s.cfg.EnrollTLSCert, s.cfg.EnrollTLSKey)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ShutdownEnrollment stops the enrolment listener.
func (s *Server) ShutdownEnrollment(ctx context.Context) error {
	if s.enrollSrv == nil {
		return nil
	}
	return s.enrollSrv.Shutdown(ctx)
}

// signerTimeout bounds a call to the signer.
//
// The old code called http.Post on http.DefaultClient, which has no timeout at
// all: a signer that accepted the connection and never answered would hold the
// request until the client gave up, and the console would hold the goroutine
// for as long as the operating system let it.
const signerTimeout = 15 * time.Second

// orgSignerClient builds a client to one organisation's own signer.
//
// The same shape as newSignerClient below, built from PEM in the registry row
// rather than paths on disk, as buildOrgTLS is. There is one set of these per
// organisation and they arrive while the process is running.
//
// The caller has already established that all four columns are present;
// SignerConfigured is the question, and asking it here as well would be a
// second place for the answer to differ.
func orgSignerClient(creds *orgCredentials) (*http.Client, error) {
	cert, err := tls.X509KeyPair([]byte(creds.SignerClientCertPEM), creds.SignerClientKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("signer client certificate for %s does not match its key: %w",
			creds.Org, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(creds.SignerCAPEM)) {
		return nil, fmt.Errorf("signer CA for %s contains no usable certificates", creds.Org)
	}
	return &http.Client{
		Timeout: signerTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
			},
		},
	}, nil
}

// newSignerClient builds the console's client to the signer, or reports why it
// cannot.
//
// Built once at New() rather than per request, so a bad certificate pair is a
// startup failure rather than something discovered by whoever first tries to
// enrol a machine.
func newSignerClient(cfg Config) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(cfg.SignerCert, cfg.SignerKey)
	if err != nil {
		return nil, fmt.Errorf("load signer client credentials: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.SignerCA)
	if err != nil {
		return nil, fmt.Errorf("read ATL_SIGNER_CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ATL_SIGNER_CA %s contains no certificate", cfg.SignerCA)
	}
	return &http.Client{
		Timeout: signerTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
			},
		},
	}, nil
}

// handleMintEnrollToken issues a token for a caller, to be carried to a
// machine.
//
// Admin, CSRF and sudo, the same gating as setting the change policy: this
// produces a credential that becomes a caller's identity, which is the class of
// action the console asks somebody to prove themselves for.
func (s *Server) handleMintEnrollToken(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	if !s.cfg.EnrollmentEnabled() {
		// Says which, rather than 404ing. The predecessor of this route was
		// unconfigured in every deployment that ever ran, and the only way to
		// find that out was to press the button.
		jsonError(w, "certificate enrolment is not configured on this console: "+
			"there is no enrolment listener, so a machine would have nowhere to "+
			"redeem a token. An operator sets CONSOLE_ENROLL_LISTEN and its "+
			"certificate",
			http.StatusServiceUnavailable)
		return
	}
	u := r.Context().Value(ctxUser).(*User)

	// The caller has to exist in this organisation's atlantis before a token
	// for it means anything. The signer checks this too, from its own
	// connection — but that check answers after a token has been minted and
	// carried to a machine, which is a worse place to discover a typo.
	atl, err := s.atlFor(r.Context(), u.Org)
	if err != nil {
		s.log.Error("resolve organisation's atlantis", "org", u.Org, "err", err)
		jsonError(w, fmt.Sprintf("no atlantis is registered for %q", u.Org),
			http.StatusServiceUnavailable)
		return
	}
	callers, err := atl.GetCallers(r.Context(), &adminpb.GetCallersRequest{})
	if err != nil {
		s.log.Error("list callers", "org", u.Org, "err", err)
		jsonError(w, "cannot reach this organisation's atlantis", http.StatusBadGateway)
		return
	}
	known := false
	for _, c := range callers.GetCallers() {
		if c.GetCaller() == caller {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, fmt.Sprintf("caller %q is not registered — add it first", caller),
			http.StatusNotFound)
		return
	}

	tok, err := s.db.forOrg(u.Org).createEnrollToken(r.Context(), caller, u.Subject)
	if err != nil {
		s.log.Error("mint enrolment token", "org", u.Org, "caller", caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Audited at mint, not only at redemption. A token minted and never
	// redeemed would otherwise leave no trace anywhere, and "fifty tokens were
	// issued and none used" is exactly the shape somebody would want to see.
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "enroll_token_minted",
		map[string]any{"caller": caller, "expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339)})

	jsonOK(w, map[string]any{
		"token":  tok.Secret,
		"caller": tok.Caller,
		"org":    u.Org,
		// Where to redeem it. The page cannot work this out and must not read it
		// from the Host header, so it comes from configuration the console
		// refuses to start without.
		"enroll_url": s.cfg.EnrollPublicURL,
		"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleGetCallerCerts lists what this console has enrolled, per caller.
//
// This is the console's own record, not atlantis's. atlantis binds a caller to
// one certificate by fingerprint and GetCallers does not return it, so this
// reports when the console enrolled a caller, not whether it is bound.
//
// The distinction matters for the warning the page shows, which is why the
// warning is unconditional: enrolling supersedes whatever certificate that
// caller was using, whether or not this console knows about it. A caller with
// no row here is the normal case today — the handler that would have written
// one answered 503 in every deployment it ever ran in.
func (s *Server) handleGetCallerCerts(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)
	certs, err := s.db.forOrg(u.Org).listCallerCerts(r.Context())
	if err != nil {
		s.log.Error("list caller certificates", "org", u.Org, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(certs))
	for _, c := range certs {
		out = append(out, map[string]any{
			"caller":      c.Caller,
			"fingerprint": hex.EncodeToString(c.Fingerprint),
			"issued_at":   c.IssuedAt.UTC().Format(time.RFC3339),
			"expires_at":  c.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	jsonOK(w, map[string]any{"certs": out, "enrolment_enabled": s.cfg.EnrollmentEnabled()})
}

// enrollRequest is what a machine posts to the enrolment listener to redeem a
// token.
type enrollRequest struct {
	Org    string `json:"org"`
	Token  string `json:"token"`
	CSRPEM string `json:"csr_pem"`

	// The assertion arm, `tide login`'s path: a cli-enroll assertion from
	// Cloud in place of an admin-minted token, naming the caller it was
	// approved for. Exactly one of Token and Assertion is present.
	Assertion string `json:"assertion"`
	Caller    string `json:"caller"`
}

// cliEnrollPurpose is the purpose claim a cli-enroll assertion carries.
// One string with internal/cloud/server.cliEnrollPurpose.
const cliEnrollPurpose = "cli-enroll"

// handleEnroll trades a token and a CSR for a signed certificate.
//
// Unauthenticated in every ordinary sense: no session, no cookie, no client
// certificate. The token row is the authority, and it is spent here.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	// Same limiter as sign-in, and this route needs it more: it is
	// unauthenticated, it is reachable by every machine that enrols, and a
	// token is the only thing between a caller and a certificate.
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		jsonError(w, "too many attempts, try again shortly", http.StatusTooManyRequests)
		return
	}
	var req enrollRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Token != "" && req.Assertion != "" {
		jsonError(w, "send a token or an assertion, not both", http.StatusBadRequest)
		return
	}
	if req.Org == "" || req.CSRPEM == "" || (req.Token == "" && req.Assertion == "") {
		jsonError(w, "org, csr_pem and one of token or assertion are required", http.StatusBadRequest)
		return
	}

	// Parse the CSR BEFORE spending the token.
	//
	// Whether the request is a well-formed, self-signed certificate request is a
	// question about its bytes. It has nothing to do with whether the token
	// authorises anything, and answering it costs no secret — so a machine that
	// sends a malformed CSR should get a 400 and keep its token, rather than
	// having to go back to an admin for another one because of a typo.
	//
	// The CN check is not here: it compares against the caller on the token's
	// row, which is not known until the row is spent. That one is an authority
	// question, and getting it wrong does cost the token.
	csr, err := parseCSRPEM(req.CSRPEM)
	if err != nil {
		jsonError(w, "invalid CSR: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The organisation comes from the request and is not trusted — it is what
	// gets BOUND, and the RESTRICTIVE policy on console.enroll_tokens is what
	// compares it against the row. A token minted for another organisation
	// matches nothing and is refused as unknown.
	//
	// This is also why the org is a parameter at all. Reading it off the token
	// row instead would mean nothing was ever compared, and a test named "a
	// token does not cross organisations" would have no field in which to name
	// the other one.
	org := req.Org
	if req.Assertion != "" {
		s.enrollWithAssertion(w, r, req, csr)
		return
	}
	caller, err := s.db.forOrg(org).spendEnrollToken(r.Context(), req.Token)
	if err != nil { //nolint:nestif // the branches are one refusal and one 500
		if !errors.Is(err, ErrEnrollTokenUnusable) {
			s.log.Error("spend enrolment token", "org", org, "err", err)
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.log.Info("enrolment refused", "org", org, "remote", r.RemoteAddr)
		jsonError(w, "enrolment token is not usable", http.StatusForbidden)
		return
	}

	// From here the token is spent whatever happens. Every later failure costs
	// the operator a new token, which is the right trade: the alternative is a
	// token that survives a partial enrolment and can be replayed against it.
	bundle, err := s.issueForCaller(r.Context(), org, caller, csr, req.CSRPEM, issueOptions{})
	if err != nil {
		s.log.Error("enrolment failed after the token was spent",
			"org", org, "caller", caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.forOrg(org).logAction(r.Context(), enrolmentActor, "", "enroll_token_spent",
		map[string]any{
			"caller":      caller,
			"fingerprint": hex.EncodeToString(bundle.fingerprint),
			"expires_at":  bundle.expiresAt,
			"remote":      r.RemoteAddr,
		})

	jsonOK(w, map[string]any{
		"cert_pem": bundle.certPEM,
		"ca_pem":   bundle.caPEM,
		"caller":   caller,
		"org":      org,
		// Everything the machine needs and cannot work out for itself: where
		// atlantis is, and where to come back to renew. The certificate carries
		// neither, and renewal is on this listener rather than on atlantis.
		"endpoint":   bundle.endpoint,
		"enroll_url": bundle.enrollURL,
		"expires_at": bundle.expiresAt,
	})
}

// renewRequest is a renewal, authenticated by the certificate the machine
// already holds.
type renewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

// handleRenew reissues a certificate for a machine that presents the one it
// already has.
//
// The peer certificate authorises this, and nothing else. There is no token: a
// renewal token would be a long-lived minting credential on disk.
//
// The request names neither the organisation nor the caller. Both come from
// console.caller_certs, looked up by the SHA-256 of the presented leaf. signCSR
// copies only the subject, so a leaf carries a common name and nothing more,
// and caller names such as `backend` collide across organisations. A request
// that named its organisation would name something the console cannot check,
// letting acme's `backend` renew into globex's atlantis.
//
// A certificate this console did not issue has no row and cannot renew. That
// includes certificates from `make dev-caller-cert` or straight from the
// signer, which authenticate at atlantis but are not renewable here.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		jsonError(w, "too many attempts, try again shortly", http.StatusTooManyRequests)
		return
	}

	// The listener only REQUESTS a certificate, because enrolment arrives with
	// none. So this handler asserts what it needs rather than assuming the
	// listener did.
	//
	// This is presence, not validity. What the certificate proves at this point
	// is possession of its private key, which the handshake established; whose
	// it is and whether this organisation trusts it are settled below, in that
	// order, because the second question cannot be asked until the first is
	// answered.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		jsonError(w, "renewal requires the certificate you are replacing",
			http.StatusUnauthorized)
		return
	}
	peerLeaf := r.TLS.PeerCertificates[0]
	sum := sha256.Sum256(peerLeaf.Raw)

	var req renewRequest
	if err := readJSON(r, &req); err != nil || req.CSRPEM == "" {
		jsonError(w, "csr_pem is required", http.StatusBadRequest)
		return
	}
	csr, err := parseCSRPEM(req.CSRPEM)
	if err != nil {
		jsonError(w, "invalid CSR: "+err.Error(), http.StatusBadRequest)
		return
	}

	rec, err := s.db.callerCertByFingerprint(r.Context(), sum[:])
	if errors.Is(err, ErrNotFound) {
		s.log.Info("renewal refused: unknown certificate",
			"cn", peerLeaf.Subject.CommonName, "remote", r.RemoteAddr)
		jsonError(w, "this console did not issue the certificate you presented, "+
			"so it cannot renew it — enrol instead", http.StatusForbidden)
		return
	}
	if err != nil {
		s.log.Error("look up the presented certificate", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// With the organisation known, check the certificate chains to that
	// organisation's authority.
	//
	// The TLS handshake cannot do this: the listener is shared and each
	// organisation has its own root, so the pool to verify against is unknown
	// until the fingerprint above says whose certificate this is.
	//
	// It is not what stops a foreign certificate, which has a fingerprint this
	// console never recorded and is refused by the lookup above. Nor a copied
	// one: a certificate is public, and the handshake signature proving the
	// sender holds the key is what stops a copy.
	//
	// It catches a certificate this console did issue, for this organisation,
	// that its atlantis would no longer accept, the organisation's authority
	// having been rotated since. Renewing it mints a successor from the new
	// authority and supersedes a working identity, producing a certificate that
	// authenticates nowhere.
	creds, err := s.db.orgCredentials(r.Context(), rec.Org)
	if err != nil {
		s.log.Error("read credentials while renewing", "org", rec.Org, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := verifyLeafForOrg(rec.Org, creds, peerLeaf); err != nil {
		s.log.Info("renewal refused: the certificate does not chain to the organisation",
			"org", rec.Org, "caller", rec.Caller,
			"cn", peerLeaf.Subject.CommonName, "remote", r.RemoteAddr, "err", err)
		jsonError(w, "the certificate you presented is not one this organisation's "+
			"atlantis trusts", http.StatusForbidden)
		return
	}

	// Expiry, which a handshake verifying against one pool would also answer.
	//
	// A machine whose certificate has already lapsed has to enrol again rather
	// than renew: renewal proves identity with the credential being replaced,
	// and an expired one has stopped being a credential.
	if now := time.Now(); now.After(peerLeaf.NotAfter) {
		s.log.Info("renewal refused: the presented certificate has expired",
			"org", rec.Org, "caller", rec.Caller, "expired", peerLeaf.NotAfter)
		jsonError(w, "the certificate you presented expired on "+
			peerLeaf.NotAfter.UTC().Format(time.RFC3339)+
			" — enrol again rather than renewing", http.StatusForbidden)
		return
	}

	// A bounded certificate spends its budget here. The successor inherits
	// the presented leaf's own lifetime rather than the signer's default, so
	// a one-hour CI certificate renews into another hour, not into a week.
	var opts issueOptions
	if rec.RenewalsRemaining != nil {
		if *rec.RenewalsRemaining <= 0 {
			s.log.Info("renewal refused: budget exhausted",
				"org", rec.Org, "caller", rec.Caller)
			jsonError(w, "renewal budget exhausted — obtain a fresh OIDC token instead",
				http.StatusForbidden)
			return
		}
		left := *rec.RenewalsRemaining - 1
		opts.renewalsRemaining = &left
		if life := int(peerLeaf.NotAfter.Sub(peerLeaf.NotBefore).Seconds()); life > 0 {
			opts.ttlSeconds = life
		}
	}

	bundle, err := s.issueForCaller(r.Context(), rec.Org, rec.Caller, csr, req.CSRPEM, opts)
	if err != nil {
		s.log.Error("renewal failed", "org", rec.Org, "caller", rec.Caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Both fingerprints, because a renewal is the one event where two
	// certificates are briefly valid for one caller. An audit trail that named
	// only the new one could not answer "which certificate was this?" for
	// anything that happened during the overlap.
	s.db.forOrg(rec.Org).logAction(r.Context(), enrolmentActor, "", "certificate_renewed",
		map[string]any{
			"caller":       rec.Caller,
			"replaced":     hex.EncodeToString(rec.Fingerprint),
			"fingerprint":  hex.EncodeToString(bundle.fingerprint),
			"expires_at":   bundle.expiresAt,
			"remote":       r.RemoteAddr,
			"presented_cn": peerLeaf.Subject.CommonName,
		})

	jsonOK(w, map[string]any{
		"cert_pem": bundle.certPEM,
		"ca_pem":   bundle.caPEM,
		"caller":   rec.Caller,
		"org":      rec.Org,
		// Returned on renewal too, so a machine picks up a moved address or a
		// rotated CA without being re-enrolled by hand.
		"endpoint":   bundle.endpoint,
		"enroll_url": bundle.enrollURL,
		"expires_at": bundle.expiresAt,
	})
}

// enrolmentActor is who the audit log records for a redemption.
//
// A redemption has no session, so there is no person to name. A defined
// constant rather than an empty string, because an audit row whose actor is
// blank reads as a bug in the logging rather than as a machine acting on its
// own behalf. The admin who authorised it is on the matching
// enroll_token_minted row.
const enrolmentActor = "enrolment"

// issuedBundle is what both the enrolment and renewal paths return, minted by
// the shared issuance path below.
type issuedBundle struct {
	certPEM string

	// caPEM is the root the machine verifies the server against, taken from
	// console.orgs rather than from the signer's `ca_pem`.
	//
	// The two answer different questions: console.orgs holds the root this
	// console verifies the organisation's server against, while the client leaf
	// is issued by whatever root that server trusts for clients. They are the
	// same CA in every deployment today and are not required to be.
	//
	// Handing back the signer's root works until the first organisation
	// provisioned with split roots, where it is a handshake failure at atlantis
	// with nothing pointing back here.
	caPEM string

	// Where the machine talks to atlantis, and where it renews. Neither is
	// derivable from the certificate, and renewal lives on the enrolment
	// listener rather than on atlantis, so both have to travel.
	endpoint  string
	enrollURL string

	expiresAt   string
	fingerprint []byte
}

// issueForCaller sends a CSR to the signer, checks what comes back, and records
// the result in both places that need it.
//
// Shared so K7b's renewal cannot drift from enrolment. Everything that makes
// the result safe lives here exactly once.
// issueOptions is how an issuance differs from the default: a bounded
// lifetime, a renewal countdown. The zero value is a human enrolment — the
// signer's full TTL, unlimited renewals.
type issueOptions struct {
	ttlSeconds        int
	renewalsRemaining *int
}

func (s *Server) issueForCaller(
	ctx context.Context, org, caller string, csr *x509.CertificateRequest, csrPEM string, opts issueOptions,
) (*issuedBundle, error) {
	// No comparison against the CSR's common name.
	//
	// The signer builds the certificate's subject from the caller name it is
	// handed and takes only the public key from the request, so what a CSR asks
	// to be called decides nothing. Refusing a mismatch would reject a harmless
	// request, and `tide login` cannot know the caller name before enrolling:
	// the token determines it, server-side, on the row spent below.
	//
	// A test asserts that the `caller` sent to the signer comes from that spent
	// row and nowhere else. Forwarding a name out of the request hands the
	// requester whatever identity it asked for, with nothing in the response
	// looking wrong.
	signed, err := s.callSigner(ctx, org, caller, csrPEM, opts.ttlSeconds)
	if err != nil {
		return nil, err
	}

	// Parser-strict, and it matters: a malformed or hostile signer response
	// that got as far as the fingerprint write would bind a caller to a hash of
	// something that is not its certificate, and nothing it presents afterwards
	// would ever match. That is a permanent lockout with no error at the time
	// it happens.
	block, _ := pem.Decode([]byte(signed.CertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the signer returned something that is not a certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the signer returned an unparseable certificate: %w", err)
	}

	// Read once, used three times: to verify the leaf below, and to tell the
	// machine which CA to trust and which address to dial.
	creds, err := s.db.orgCredentials(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("read credentials for %q: %w", org, err)
	}

	// The certificate has to be one this organisation's atlantis will accept.
	//
	// There is one signer and one CA, while each organisation has its own — so
	// enrolling into an organisation the signer does not issue for produces a
	// certificate that fails the handshake at that atlantis. Without this check
	// it would also supersede the caller that was working there, which turns a
	// misconfiguration into an outage. Refuse before recording anything.
	if err := verifyLeafForOrg(org, creds, leaf); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(leaf.Raw)
	fingerprint := sum[:]
	expiresAt := leaf.NotAfter.UTC().Format(time.RFC3339)

	// atlantis first, because that is the write that decides whether the
	// certificate authenticates. The console's own row is bookkeeping: if it
	// fails, the machine still has a working certificate and the console has
	// lost the ability to tell whose it is — bad, but recoverable by enrolling
	// again. The other order would mint a certificate the console believes in
	// and atlantis refuses.
	atl, err := s.atlFor(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("no atlantis is registered for %q", org)
	}
	if _, err := atl.RecordCallerCertExpiry(ctx, &adminpb.RecordCallerCertExpiryRequest{
		Caller:      caller,
		ExpiresAt:   expiresAt,
		Fingerprint: hex.EncodeToString(fingerprint),
	}); err != nil {
		return nil, fmt.Errorf("certificate minted but binding write failed; "+
			"the previous certificate still authenticates — enrol again to rotate: %w", err)
	}

	if err := s.db.recordCallerCert(ctx, callerCert{
		Fingerprint:       fingerprint,
		Org:               org,
		Caller:            caller,
		ExpiresAt:         leaf.NotAfter,
		RenewalsRemaining: opts.renewalsRemaining,
	}); err != nil {
		// Not fatal. The certificate works; what is lost is the console's
		// record of it, which renewal needs. Loud, because renewal will then
		// refuse and the reason will be here and nowhere else.
		s.log.Error("record issued certificate", "org", org, "caller", caller, "err", err)
	}

	return &issuedBundle{
		certPEM: signed.CertPEM,
		// creds.CAPEM, not signed.CAPEM. See the field's comment.
		caPEM:       creds.CAPEM,
		endpoint:    creds.PublicEndpoint,
		enrollURL:   s.cfg.EnrollPublicURL,
		expiresAt:   expiresAt,
		fingerprint: fingerprint,
	}, nil
}

// verifyLeafForOrg checks a freshly signed certificate against the CA the
// organisation's atlantis actually trusts.
//
// Takes the credentials rather than reading them, because issueForCaller now
// needs the same row for the caller-facing endpoint and the CA it hands back —
// and reading it twice invites the two reads to disagree across a rotation.
func verifyLeafForOrg(org string, creds *orgCredentials, leaf *x509.Certificate) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(creds.CAPEM)) {
		return fmt.Errorf("organisation %q has no usable CA on file", org)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("the signer issued a certificate that %q's atlantis would not accept "+
			"— the signer holds a different certificate authority: %w", org, err)
	}
	return nil
}

type signerResponse struct {
	CertPEM   string `json:"cert_pem"`
	CAPEM     string `json:"ca_pem"`
	ExpiresAt string `json:"expires_at"`
	Error     string `json:"error"`
}

// signerFor returns the client and address for an organisation's signer.
//
// Its own if it has one, the console's process-wide client otherwise. That
// fallback is not a transitional convenience: it is what every organisation
// registered before migration 0009 uses, and what `make dev-signer` serves.
//
// Both halves come from one place. An organisation's client certificate paired
// with the shared address, or the reverse, is refused at a signer's handshake
// with a message about a certificate that names no row.
func (s *Server) signerFor(ctx context.Context, org string) (*http.Client, string, error) {
	e, err := s.orgs.get(ctx, org)
	if err != nil {
		return nil, "", err
	}
	if e.signer != nil {
		return e.signer, e.signerAddr, nil
	}
	if s.signer == nil {
		return nil, "", errors.New("certificate enrolment is not configured on this console")
	}
	return s.signer, s.cfg.SignerAddr, nil
}

// callSigner posts a CSR over mTLS and returns what came back.
func (s *Server) callSigner(ctx context.Context, org, caller, csrPEM string, ttlSeconds int) (*signerResponse, error) {
	client, addr, err := s.signerFor(ctx, org)
	if err != nil {
		return nil, err
	}
	req0 := map[string]any{"caller": caller, "csr_pem": csrPEM}
	// Only when it shortens something. The signer owns the default and the
	// ceiling; absent means its full term, as every request before this field
	// existed meant.
	if ttlSeconds > 0 {
		req0["ttl_seconds"] = ttlSeconds
	}
	body, err := json.Marshal(req0)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		addr+"/issue", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signer unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out signerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return nil, errors.New("the signer returned a response this console could not read")
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, errors.New(out.Error)
		}
		return nil, fmt.Errorf("the signer refused with status %d", resp.StatusCode)
	}
	return &out, nil
}

func parseCSRPEM(pemStr string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	// A CSR nobody signed proves nothing about who holds the private key, which
	// is the only thing a CSR is for.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the CSR's signature does not verify: %w", err)
	}
	return csr, nil
}

// enrollWithAssertion is `tide login`'s arm: a cli-enroll assertion in place
// of an admin-minted token.
//
// The checks run in the order the failures cost least: verification and the
// static claim comparisons first, the jti spend last before issuance, so a
// request refused for its role or its caller does not burn the assertion.
func (s *Server) enrollWithAssertion(w http.ResponseWriter, r *http.Request, req enrollRequest, csr *x509.CertificateRequest) {
	if s.enrollCloud == nil {
		jsonError(w, "enrolment is not enabled here", http.StatusServiceUnavailable)
		return
	}
	if req.Caller == "" {
		jsonError(w, "caller is required with an assertion", http.StatusBadRequest)
		return
	}

	claims, err := s.enrollCloud.Verify(r.Context(), req.Assertion)
	if errors.Is(err, cloudauth.ErrKeysUnavailable) {
		jsonError(w, "cannot reach the identity provider; try again shortly", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.log.Warn("enrolment assertion rejected", "ip", clientIP(r), "err", err)
		jsonError(w, "invalid assertion", http.StatusUnauthorized)
		return
	}

	// Scoped to this exchange. A session assertion that reached this listener
	// carries no purpose and is refused, whatever its audience says.
	if claims.Purpose != cliEnrollPurpose {
		s.log.Warn("assertion without the enrolment purpose", "ip", clientIP(r))
		jsonError(w, "invalid assertion", http.StatusUnauthorized)
		return
	}
	// The token arm's RESTRICTIVE row comparison has no analogue here, so the
	// organisations are compared outright.
	if req.Org != claims.Org {
		jsonError(w, "the assertion names a different organisation", http.StatusForbidden)
		return
	}
	// Bound at approval: the person saw this caller name on the approval page.
	if claims.Caller != req.Caller {
		jsonError(w, "the assertion names a different caller", http.StatusForbidden)
		return
	}

	// Admins always; viewers where the organisation opted this caller in. A
	// caller certificate authenticates as the caller, not the person, so
	// viewer self-enrolment is a per-caller decision.
	if claims.Role != identity.RoleAdmin {
		allowed, err := s.db.forOrg(claims.Org).developersMayEnroll(r.Context(), req.Caller)
		if err != nil {
			s.log.Error("read enrolment policy", "org", claims.Org, "caller", req.Caller, "err", err)
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !allowed {
			jsonError(w, "developers may not enrol as this caller; ask an administrator", http.StatusForbidden)
			return
		}
	}

	// The caller must exist in this organisation's atlantis, the same check
	// token minting makes before a token exists.
	atl, err := s.atlFor(r.Context(), claims.Org)
	if err != nil {
		s.log.Error("reach atlantis", "org", claims.Org, "err", err)
		jsonError(w, "cannot reach the organisation", http.StatusBadGateway)
		return
	}
	callers, err := atl.GetCallers(r.Context(), &adminpb.GetCallersRequest{})
	if err != nil {
		s.log.Error("list callers", "org", claims.Org, "err", err)
		jsonError(w, "cannot reach the organisation", http.StatusBadGateway)
		return
	}
	known := false
	for _, c := range callers.GetCallers() {
		if c.GetCaller() == req.Caller {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, "no such caller in this organisation", http.StatusNotFound)
		return
	}

	// Spent last: everything after this costs the person a fresh login.
	if err := s.db.spendAssertion(r.Context(), claims.ID, claims.Expiry); errors.Is(err, ErrAssertionSpent) {
		s.log.Warn("enrolment assertion replayed", "ip", clientIP(r), "subject", claims.Subject)
		jsonError(w, "invalid assertion", http.StatusUnauthorized)
		return
	} else if err != nil {
		s.log.Error("record spent assertion", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	bundle, err := s.issueForCaller(r.Context(), claims.Org, req.Caller, csr, req.CSRPEM, issueOptions{})
	if err != nil {
		s.log.Error("enrolment failed after the assertion was spent",
			"org", claims.Org, "caller", req.Caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	// The actor is the person the assertion names, which the token arm never
	// has: a spent token knows who minted it, not who redeemed it.
	s.db.forOrg(claims.Org).logAction(r.Context(), "cloud:"+claims.Subject, claims.Email,
		"enroll_assertion_spent",
		map[string]any{
			"caller":      req.Caller,
			"role":        string(claims.Role),
			"fingerprint": hex.EncodeToString(bundle.fingerprint),
			"expires_at":  bundle.expiresAt,
			"remote":      r.RemoteAddr,
		})

	jsonOK(w, map[string]any{
		"cert_pem":   bundle.certPEM,
		"ca_pem":     bundle.caPEM,
		"caller":     req.Caller,
		"org":        claims.Org,
		"endpoint":   bundle.endpoint,
		"enroll_url": bundle.enrollURL,
		"expires_at": bundle.expiresAt,
	})
}

// handleGetCallerEnrollment reports the self-enrolment flag for one caller.
// Readable by any signed-in user, as the issued-certificates list is.
func (s *Server) handleGetCallerEnrollment(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	allowed, err := s.db.forOrg(u.Org).developersMayEnroll(r.Context(), caller)
	if err != nil {
		s.log.Error("read enrolment policy", "org", u.Org, "caller", caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"caller": caller, "developers_may_enroll": allowed})
}

// handleSetCallerEnrollment records the flag. Admin, CSRF and sudo, as
// minting is: the flag decides who can obtain the caller's identity.
func (s *Server) handleSetCallerEnrollment(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	var body struct {
		DevelopersMayEnroll bool `json:"developers_may_enroll"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	actor, actorEmail, _ := u.Actor()
	if err := s.db.forOrg(u.Org).setDevelopersMayEnroll(r.Context(), caller, body.DevelopersMayEnroll, actor); err != nil {
		s.log.Error("set enrolment policy", "org", u.Org, "caller", caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.forOrg(u.Org).logAction(r.Context(), actor, actorEmail, "caller_enrollment_set",
		map[string]any{"caller": caller, "developers_may_enroll": body.DevelopersMayEnroll})
	jsonOK(w, map[string]any{"caller": caller, "developers_may_enroll": body.DevelopersMayEnroll})
}

// oidcCertTTLSeconds is the lifetime a workload's certificate is issued
// with. An hour: longer than any ordinary CI job, and short enough that
// expiry is the revocation.
const oidcCertTTLSeconds = 3600

type oidcEnrollRequest struct {
	Org     string `json:"org"`
	Caller  string `json:"caller"`
	IDToken string `json:"id_token"`
	CSRPEM  string `json:"csr_pem"`
}

// handleEnrollOIDC trades a CI workload's OIDC id_token for a short-lived
// certificate, under a federation rule the organisation configured.
//
// Unauthenticated in the same sense /enroll is: the token is the whole of
// what authorises it, verified against the rule's issuer rather than minted
// by anybody here.
func (s *Server) handleEnrollOIDC(w http.ResponseWriter, r *http.Request) {
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		jsonError(w, "too many attempts, try again shortly", http.StatusTooManyRequests)
		return
	}
	var req oidcEnrollRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Org == "" || req.Caller == "" || req.IDToken == "" || req.CSRPEM == "" {
		jsonError(w, "org, caller, id_token and csr_pem are required", http.StatusBadRequest)
		return
	}
	csr, err := parseCSRPEM(req.CSRPEM)
	if err != nil {
		jsonError(w, "invalid CSR: "+err.Error(), http.StatusBadRequest)
		return
	}

	rules, err := s.db.forOrg(req.Org).federationRulesFor(r.Context(), req.Caller)
	if err != nil {
		s.log.Error("read federation rules", "org", req.Org, "caller", req.Caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// First rule the token satisfies wins. One refusal for every way to fail —
	// no rules, no match, bad token — so a probe cannot map which rules exist.
	var matched *federationRule
	var workload *oidcfed.Identity
	for i := range rules {
		id, verr := s.oidc.Verify(r.Context(), req.IDToken,
			rules[i].IssuerURL, rules[i].Audience, rules[i].SubjectPattern)
		if verr == nil {
			matched, workload = &rules[i], id
			break
		}
	}
	if matched == nil {
		s.log.Info("oidc enrolment refused", "org", req.Org, "caller", req.Caller,
			"rules", len(rules), "remote", r.RemoteAddr)
		jsonError(w, "no federation rule admits this token", http.StatusForbidden)
		return
	}

	budget := matched.RenewalBudget
	bundle, err := s.issueForCaller(r.Context(), req.Org, req.Caller, csr, req.CSRPEM, issueOptions{
		ttlSeconds:        oidcCertTTLSeconds,
		renewalsRemaining: &budget,
	})
	if err != nil {
		s.log.Error("oidc enrolment failed", "org", req.Org, "caller", req.Caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.forOrg(req.Org).logAction(r.Context(), "oidc:"+workload.Subject, "", "oidc_enrolled",
		map[string]any{
			"caller":      req.Caller,
			"rule_id":     matched.ID,
			"issuer":      workload.Issuer,
			"subject":     workload.Subject,
			"fingerprint": hex.EncodeToString(bundle.fingerprint),
			"expires_at":  bundle.expiresAt,
			"remote":      r.RemoteAddr,
		})

	jsonOK(w, map[string]any{
		"cert_pem":   bundle.certPEM,
		"ca_pem":     bundle.caPEM,
		"caller":     req.Caller,
		"org":        req.Org,
		"endpoint":   bundle.endpoint,
		"enroll_url": bundle.enrollURL,
		"expires_at": bundle.expiresAt,
	})
}

// handleListFederationRules returns the unrevoked rules for one caller.
func (s *Server) handleListFederationRules(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	rules, err := s.db.forOrg(u.Org).federationRulesFor(r.Context(), caller)
	if err != nil {
		s.log.Error("list federation rules", "org", u.Org, "caller", caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	type wire struct {
		ID             int64  `json:"id"`
		IssuerURL      string `json:"issuer_url"`
		Audience       string `json:"audience"`
		SubjectPattern string `json:"subject_pattern"`
		RenewalBudget  int    `json:"renewal_budget"`
		CreatedBy      string `json:"created_by"`
		CreatedAt      string `json:"created_at"`
	}
	out := make([]wire, 0, len(rules))
	for _, ru := range rules {
		out = append(out, wire{
			ID: ru.ID, IssuerURL: ru.IssuerURL, Audience: ru.Audience,
			SubjectPattern: ru.SubjectPattern, RenewalBudget: ru.RenewalBudget,
			CreatedBy: ru.CreatedBy, CreatedAt: ru.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	jsonOK(w, map[string]any{"rules": out})
}

// handleAddFederationRule records one binding.
func (s *Server) handleAddFederationRule(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	var body struct {
		IssuerURL      string `json:"issuer_url"`
		Audience       string `json:"audience"`
		SubjectPattern string `json:"subject_pattern"`
		RenewalBudget  *int   `json:"renewal_budget"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(body.IssuerURL, "https://") {
		jsonError(w, "issuer_url must be https", http.StatusBadRequest)
		return
	}
	if body.Audience == "" {
		jsonError(w, "audience is required", http.StatusBadRequest)
		return
	}
	if err := oidcfed.ValidatePattern(body.SubjectPattern); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	budget := 1
	if body.RenewalBudget != nil {
		if *body.RenewalBudget < 0 {
			jsonError(w, "renewal_budget must be zero or more", http.StatusBadRequest)
			return
		}
		budget = *body.RenewalBudget
	}

	u := r.Context().Value(ctxUser).(*User)
	actor, actorEmail, _ := u.Actor()
	id, err := s.db.forOrg(u.Org).addFederationRule(r.Context(), federationRule{
		Caller: caller, IssuerURL: strings.TrimRight(body.IssuerURL, "/"),
		Audience: body.Audience, SubjectPattern: body.SubjectPattern,
		RenewalBudget: budget, CreatedBy: actor,
	})
	if err != nil {
		s.log.Error("add federation rule", "org", u.Org, "caller", caller, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.forOrg(u.Org).logAction(r.Context(), actor, actorEmail, "federation_rule_created",
		map[string]any{"caller": caller, "rule_id": id,
			"issuer": body.IssuerURL, "subject_pattern": body.SubjectPattern})
	jsonOK(w, map[string]any{"id": id})
}

// handleRevokeFederationRule ends one binding.
func (s *Server) handleRevokeFederationRule(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if caller == "" || err != nil {
		jsonError(w, "caller and rule id are required", http.StatusBadRequest)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	if err := s.db.forOrg(u.Org).revokeFederationRule(r.Context(), id); errors.Is(err, ErrNotFound) {
		jsonError(w, "no such rule", http.StatusNotFound)
		return
	} else if err != nil {
		s.log.Error("revoke federation rule", "org", u.Org, "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	actor, actorEmail, _ := u.Actor()
	s.db.forOrg(u.Org).logAction(r.Context(), actor, actorEmail, "federation_rule_revoked",
		map[string]any{"caller": caller, "rule_id": id})
	jsonOK(w, map[string]any{"revoked": id})
}
