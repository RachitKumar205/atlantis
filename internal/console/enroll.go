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
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Enrolment: how a machine gets a client certificate for a caller.
//
// # What this replaced, and why
//
// handleIssueCert generated a P-256 key inside the console, built a CSR with
// it, sent the CSR to the signer, and returned the private key to a browser for
// download. The CSR half was always right. The key was simply born in the wrong
// process: it crossed the network, sat in JavaScript memory, and landed in a
// Downloads folder, for no reason anyone could name — the signer has only ever
// wanted a CSR.
//
// Now the machine that will use the key generates it, and only the CSR travels.
// The console never sees a private key and cannot leak one it does not hold.
//
// # Two routes, two different authorities
//
// An admin mints a token on the console's normal API, behind session, role,
// CSRF and sudo. A machine redeems it on the enrolment listener, which has no
// session and no cookies — the token is the whole of what authorises it.
//
// The redemption route is on a separate listener carrying only these routes.
// The console's main mux ends in a `/` catch-all serving the SPA, so mounting
// this on it would publish the entire console API on a port every machine that
// enrols can reach.

// buildEnrollListener prepares the second listener.
//
// # Its own mux, and that is the point
//
// buildMux ends with a `/` catch-all that serves the SPA, and the *Server is
// itself the handler for the main listener. Reusing either here would put the
// whole console API — sign-in, callers, the change policy, audit — on a port
// that every machine needing a certificate can reach, behind none of the
// upstream terminator, ingress limits or WAF the main listener sits behind.
// Two routes, registered here, and nothing else can be added by accident.
//
// # Requested, never required
//
// Enrolment arrives with no certificate — that is what enrolment is. Renewal
// arrives with one. The listener therefore cannot demand one, and each handler
// asserts what it needs: handleEnroll wants a token, and handleRenew wants a
// peer certificate and checks for it itself rather than assuming the listener
// did.
//
// No browser reaches this port, so the certificate-selection prompt that
// requesting a client certificate causes in a browser is not a concern here —
// which is the other reason it is not on the main listener.
func (s *Server) buildEnrollListener() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", s.handleEnroll)
	mux.HandleFunc("POST /renew", s.handleRenew)

	// RequestClientCert, and the certificate is verified in handleRenew.
	//
	// # Why the check moved out of the handshake
	//
	// This listener used to hold VerifyClientCertIfGiven against ONE pool of
	// client CAs, read from CONSOLE_ENROLL_CLIENT_CA. That works while every
	// caller in the deployment chains to one authority, and stops working the
	// moment each organisation has its own — which is what migration 0005 made
	// true and 0009 finished. One pool cannot verify every organisation's
	// callers, and the failure is the worst shape available: caller
	// certificates live seven days and `tide` renews at two thirds of that, so
	// every organisation but one would silently stop renewing around day five,
	// on a machine nobody is watching, with the refusal delivered as a
	// handshake reset that no handler ever sees and a client-side warning on
	// stderr that is not an error.
	//
	// handleRenew already resolves the organisation from the certificate's
	// fingerprint, so the right authority to verify against is known there and
	// nowhere earlier. Verifying at that point costs nothing and turns the
	// refusal into a 403 with a reason, on the server, beside the organisation
	// it concerns.
	//
	// # What this gives up, and what it does not
	//
	// It gives up a check that fails closed automatically. handleRenew MUST now
	// verify, and a route added to this listener that reads r.TLS without
	// verifying would be trusting an unverified certificate. There are two
	// routes here and there is no catch-all, which is why that is acceptable —
	// and the property is asserted directly by
	// TestACertificateFromAnotherAuthorityCannotRenew rather than left implied
	// by the configuration.
	//
	// It does NOT give up proof of possession. A client that sends a
	// certificate must also send CertificateVerify, and Go checks that
	// signature against the presented public key whatever ClientAuth is set to
	// — see crypto/tls, where the check sits inside "the client sent a
	// certificate" and not inside "we are verifying it". So a copied
	// certificate, which is public, still does not let anybody renew.
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
// The same shape as newSignerClient below, from PEM in the registry row rather
// than paths on disk — the same difference, and for the same reason, as
// buildOrgTLS versus the files the console used to load. There is one set of
// these per organisation and they arrive while the process is running.
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

// ── Minting, on the console's API ───────────────────────────────────────────

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
			"an operator sets ATL_SIGNER_ADDR and the enrolment listener",
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
// # What it can and cannot say
//
// This is the console's own record, not atlantis's. atlantis binds a caller to
// one certificate by fingerprint, and GetCallers does not return that
// fingerprint — so the console cannot report "is this caller bound", only
// "did this console enrol it, and when".
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

// ── Redeeming, on the enrolment listener ────────────────────────────────────

type enrollRequest struct {
	Org    string `json:"org"`
	Token  string `json:"token"`
	CSRPEM string `json:"csr_pem"`
}

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
	if req.Org == "" || req.Token == "" || req.CSRPEM == "" {
		jsonError(w, "org, token and csr_pem are required", http.StatusBadRequest)
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
	// The CN check is deliberately NOT here: it compares against the caller on
	// the token's row, which is not known until the row is spent. That one is an
	// authority question, and getting it wrong does cost the token.
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
	bundle, err := s.issueForCaller(r.Context(), org, caller, csr, req.CSRPEM)
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

// ── Renewing, with the certificate the machine already holds ────────────────

type renewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

// handleRenew reissues a certificate for a machine that presents the one it
// already has.
//
// # What authorises this, and what does not
//
// The peer certificate, and nothing else. There is no token: a token would be a
// long-lived minting credential sitting on disk, which is the thing enrolment
// exists to avoid. A machine that has a working certificate has already proved
// it is the caller; renewal only asks it to prove that again.
//
// # The request names neither the organisation nor the caller
//
// Both come from console.caller_certs, looked up by the SHA-256 of the leaf the
// peer presented. That is deliberate and it is the only shape that works. There
// is one signing authority and signCSR copies only the subject, so a leaf
// carries a common name and nothing more — and caller names are `backend`,
// `api`, `worker`, which collide across organisations as a matter of course. A
// request that named its organisation would be naming something the console
// could not check, and acme's `backend` could renew into globex's atlantis and
// supersede the identity working there.
//
// A certificate this console did not issue has no row, so it cannot renew. That
// includes certificates minted by `make dev-caller-cert` or straight from the
// signer: they authenticate at atlantis perfectly well and are simply not
// renewable here, which is the honest answer rather than a guess about who they
// belong to.
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

	// Now that the organisation is known, check the certificate actually chains
	// to ITS authority.
	//
	// This is the check the TLS handshake used to do, moved here because it
	// could not be done there: the listener is shared and each organisation has
	// its own root, so the pool to verify against is not known until the
	// fingerprint above has said whose certificate this is.
	//
	// What this catches, stated accurately rather than generously.
	//
	// It is NOT what stops a foreign certificate: one has a fingerprint this
	// console never recorded, so the lookup above refuses it first. Nor is it
	// what stops a copied certificate — a certificate is public, and what stops
	// a copy is the handshake signature proving the sender holds the key.
	//
	// What it catches is a certificate this console really did issue, for this
	// organisation, that its atlantis would no longer accept: the case where
	// the organisation's authority has been rotated since. Renewing it would
	// mint a successor from the new authority and supersede a working
	// identity — a certificate that authenticates nowhere, produced by a
	// request that looked entirely reasonable.
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

	// Expiry, which the handshake also used to answer.
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

	bundle, err := s.issueForCaller(r.Context(), rec.Org, rec.Caller, csr, req.CSRPEM)
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

// ── The shared issuance path ────────────────────────────────────────────────

type issuedBundle struct {
	certPEM string

	// caPEM is the root the machine verifies the SERVER against, taken from
	// console.orgs — NOT the signer's `ca_pem`.
	//
	// They are different questions and the codebase already says so: the
	// registration guard notes that the console's CAPEM "is the root this
	// console verifies the organisation's *server* against; the client leaf is
	// issued by whatever root that server trusts for clients", and that the two
	// "are the same CA in every deployment that exists today and are not
	// required to be."
	//
	// Handing back the signer's root would work everywhere it has ever been
	// tried and break the first organisation provisioned with split roots — as
	// a handshake failure at atlantis, with nothing pointing back here.
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
func (s *Server) issueForCaller(
	ctx context.Context, org, caller string, csr *x509.CertificateRequest, csrPEM string,
) (*issuedBundle, error) {
	// No comparison against the CSR's common name.
	//
	// The signer builds the certificate's subject from the caller name it is
	// handed and takes only the public key from the request, so what a CSR asks
	// to be called decides nothing. Refusing a mismatch would reject a harmless
	// request, and — more to the point — `tide login` cannot know the caller
	// name before enrolling: the token determines it, server-side, on the row
	// spent below.
	//
	// What is load-bearing, and is asserted by a test, is that the `caller` sent
	// to the signer comes from that spent row and from nowhere else. Forwarding
	// a name out of the request would hand the requester whatever identity it
	// asked for, and nothing in the response would look wrong.
	signed, err := s.callSigner(ctx, org, caller, csrPEM)
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
		Fingerprint: fingerprint,
		Org:         org,
		Caller:      caller,
		ExpiresAt:   leaf.NotAfter,
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
// Both halves come from one place, which is the point. An organisation's client
// certificate paired with the shared address — or the reverse — is refused at a
// signer's handshake with a message about a certificate, and nothing in it
// names the row that produced the mismatch.
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
func (s *Server) callSigner(ctx context.Context, org, caller, csrPEM string) (*signerResponse, error) {
	client, addr, err := s.signerFor(ctx, org)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"caller": caller, "csr_pem": csrPEM})
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
