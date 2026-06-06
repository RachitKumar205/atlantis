package console

// connectivity backs the setup wizard's second step. Each row the SPA
// displays corresponds to one real probe here. Downstream probes are
// gated on upstream success — if TCP fails, the TLS / cert / gRPC
// rows render as "wait" rather than running into a guaranteed error.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	grpcreflectv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

const probeDialTimeout = 3 * time.Second

type probeStatus string

const (
	probeOK   probeStatus = "ok"
	probeErr  probeStatus = "err"
	probeWait probeStatus = "wait"
)

type probeResult struct {
	Label  string      `json:"label"`
	Status probeStatus `json:"status"`
	Meta   string      `json:"meta,omitempty"`
}

type connectivityResponse struct {
	Endpoint string        `json:"endpoint"`
	Overall  probeStatus   `json:"overall"`
	Probes   []probeResult `json:"probes"`
}

// handleSetupConnectivity runs the five connectivity probes against the
// atlantis gRPC endpoint. Unauthenticated by design — the wizard runs
// before an admin user exists. The probe surface is purely diagnostic;
// no sensitive state is returned.
func (s *Server) handleSetupConnectivity(w http.ResponseWriter, r *http.Request) {
	endpoint := s.cfg.ATLEndpoint
	resp := connectivityResponse{
		Endpoint: endpoint,
		Overall:  probeOK,
		Probes:   make([]probeResult, 0, 5),
	}

	// ── 1. TCP reachable ─────────────────────────────────────────────────
	tcpStart := time.Now()
	rawConn, err := net.DialTimeout("tcp", endpoint, probeDialTimeout)
	if err != nil {
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "TCP reachable",
			Status: probeErr,
			Meta:   shortErr(err),
		})
		resp.Probes = appendWaiting(resp.Probes,
			"TLS 1.3 handshake",
			"Server cert chain",
			"Client cert accepted",
			"gRPC reflection",
		)
		resp.Overall = probeErr
		jsonOK(w, resp)
		return
	}
	rtt := time.Since(tcpStart)
	_ = rawConn.Close()
	resp.Probes = append(resp.Probes, probeResult{
		Label:  "TCP reachable",
		Status: probeOK,
		Meta:   fmt.Sprintf("%dms", rtt.Milliseconds()),
	})

	// Dev mode (no ATL_TLS_CERT) — surface the three TLS rows as
	// err/wait so the user sees the gap rather than fabricated success.
	// gRPC reflection still runs on the insecure channel so they get
	// one real reachability signal.
	if s.cfg.ATLTLSCert == "" {
		resp.Probes = append(resp.Probes,
			probeResult{Label: "TLS 1.3 handshake", Status: probeErr, Meta: "TLS not configured"},
			probeResult{Label: "Server cert chain", Status: probeWait},
			probeResult{Label: "Client cert accepted", Status: probeWait},
		)
		// gRPC reflection still works without TLS — try it on the
		// insecure transport so the user sees gRPC reachability.
		appendReflectionProbe(r.Context(), endpoint, insecure.NewCredentials(), &resp)
		resp.Overall = probeErr
		jsonOK(w, resp)
		return
	}

	// ── 2 + 3. TLS handshake + server cert chain ────────────────────────
	// One TLS dial covers both: handshake success → row 2 ok; the
	// negotiated PeerCertificates carry the chain we render in row 3.
	caPool, caErr := loadCAPool(s.cfg.ATLTLSCA)
	if caErr != nil {
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "TLS 1.3 handshake",
			Status: probeErr,
			Meta:   "CA load failed: " + shortErr(caErr),
		})
		resp.Probes = appendWaiting(resp.Probes,
			"Server cert chain",
			"Client cert accepted",
			"gRPC reflection",
		)
		resp.Overall = probeErr
		jsonOK(w, resp)
		return
	}

	clientCert, certErr := tls.LoadX509KeyPair(s.cfg.ATLTLSCert, s.cfg.ATLTLSKey)
	if certErr != nil {
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "TLS 1.3 handshake",
			Status: probeErr,
			Meta:   "client cert load failed: " + shortErr(certErr),
		})
		resp.Probes = appendWaiting(resp.Probes,
			"Server cert chain",
			"Client cert accepted",
			"gRPC reflection",
		)
		resp.Overall = probeErr
		jsonOK(w, resp)
		return
	}

	host, _, splitErr := net.SplitHostPort(endpoint)
	if splitErr != nil {
		// Endpoint without port — fall back to the whole string as host.
		host = endpoint
	}

	tlsCfg := &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{clientCert},
		ServerName:   host,
		MinVersion:   tls.VersionTLS13,
	}

	tlsConn, tlsErr := tls.DialWithDialer(&net.Dialer{Timeout: probeDialTimeout}, "tcp", endpoint, tlsCfg)
	if tlsErr != nil {
		// A failed mTLS dial conflates three causes: handshake itself,
		// server cert chain validity, and client cert acceptance. We
		// pick the closest match by inspecting the error string.
		label, mTLSAccepted := classifyTLSError(tlsErr)
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "TLS 1.3 handshake",
			Status: ternaryProbe(label == "tls", probeErr, probeOK),
			Meta:   ternaryStr(label == "tls", shortErr(tlsErr), "TLS_1.3"),
		})
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "Server cert chain",
			Status: ternaryProbe(label == "cert", probeErr, ternaryProbe(label == "tls", probeWait, probeOK)),
			Meta:   ternaryStr(label == "cert", shortErr(tlsErr), ""),
		})
		resp.Probes = append(resp.Probes, probeResult{
			Label:  "Client cert accepted",
			Status: ternaryProbe(mTLSAccepted, probeOK, probeErr),
			Meta:   ternaryStr(!mTLSAccepted, shortErr(tlsErr), ""),
		})
		resp.Probes = appendWaiting(resp.Probes, "gRPC reflection")
		resp.Overall = probeErr
		jsonOK(w, resp)
		return
	}
	state := tlsConn.ConnectionState()
	_ = tlsConn.Close()

	// Row 2 — handshake success. Report TLS version + cipher suite.
	resp.Probes = append(resp.Probes, probeResult{
		Label:  "TLS 1.3 handshake",
		Status: probeOK,
		Meta:   fmt.Sprintf("%s · %s", tlsVersionName(state.Version), tls.CipherSuiteName(state.CipherSuite)),
	})

	// Row 3 — server cert chain. Walk to the issuer; report its CN.
	chainMeta := "valid"
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		issuer := strings.TrimSpace(leaf.Issuer.CommonName)
		if issuer == "" {
			issuer = "unknown CA"
		}
		chainMeta = "valid → " + issuer
	}
	resp.Probes = append(resp.Probes, probeResult{
		Label:  "Server cert chain",
		Status: probeOK,
		Meta:   chainMeta,
	})

	// Row 4 — client cert accepted. The successful handshake above
	// confirms the server accepted our cert; report the CN we sent.
	clientCN := "console"
	if x509Leaf, err := x509.ParseCertificate(clientCert.Certificate[0]); err == nil {
		if cn := strings.TrimSpace(x509Leaf.Subject.CommonName); cn != "" {
			clientCN = cn
		}
	}
	resp.Probes = append(resp.Probes, probeResult{
		Label:  "Client cert accepted",
		Status: probeOK,
		Meta:   "CN=" + clientCN,
	})

	// Row 5 — gRPC reflection. Real probe using the mTLS-bound channel.
	appendReflectionProbe(r.Context(), endpoint, credentials.NewTLS(tlsCfg), &resp)

	// Roll up the overall flag.
	for _, p := range resp.Probes {
		if p.Status == probeErr {
			resp.Overall = probeErr
			break
		}
	}
	jsonOK(w, resp)
}

// appendReflectionProbe dials atlantis over the supplied credentials and
// calls the v1 reflection RPC. Falls back to the gRPC Health service
// when reflection isn't enabled (rare; atlantis registers reflection
// unconditionally but other servers might not).
func appendReflectionProbe(ctx context.Context, endpoint string, creds credentials.TransportCredentials, resp *connectivityResponse) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		resp.Probes = append(resp.Probes, probeResult{
			Label: "gRPC reflection", Status: probeErr, Meta: shortErr(err),
		})
		return
	}
	defer conn.Close() //nolint:errcheck

	// Try v1 reflection first.
	probeCtx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	defer cancel()
	refClient := grpcreflectv1.NewServerReflectionClient(conn)
	stream, err := refClient.ServerReflectionInfo(probeCtx)
	if err == nil {
		if sendErr := stream.Send(&grpcreflectv1.ServerReflectionRequest{
			MessageRequest: &grpcreflectv1.ServerReflectionRequest_ListServices{ListServices: ""},
		}); sendErr == nil {
			if recv, recvErr := stream.Recv(); recvErr == nil {
				count := 0
				if list := recv.GetListServicesResponse(); list != nil {
					count = len(list.Service)
				}
				_ = stream.CloseSend()
				resp.Probes = append(resp.Probes, probeResult{
					Label:  "gRPC reflection",
					Status: probeOK,
					Meta:   fmt.Sprintf("%d services", count),
				})
				return
			}
		}
		_ = stream.CloseSend()
	}

	// Fallback: probe the gRPC Health service so a reflection-disabled
	// server still shows "gRPC up" rather than a misleading failure.
	healthClient := grpc_health_v1.NewHealthClient(conn)
	healthCtx, healthCancel := context.WithTimeout(ctx, probeDialTimeout)
	defer healthCancel()
	h, hErr := healthClient.Check(healthCtx, &grpc_health_v1.HealthCheckRequest{})
	if hErr != nil {
		resp.Probes = append(resp.Probes, probeResult{
			Label: "gRPC reflection", Status: probeErr, Meta: shortErr(hErr),
		})
		return
	}
	resp.Probes = append(resp.Probes, probeResult{
		Label:  "gRPC reflection",
		Status: probeOK,
		Meta:   "health=" + h.Status.String() + " (reflection disabled)",
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func loadCAPool(caFile string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("no usable certs in CA file")
	}
	return pool, nil
}

// shortErr trims long Go error strings down to something the SPA can
// render in a single cell. The full error is fine in logs; the UI cell
// just needs the gist.
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, ':'); i >= 0 && i < 40 {
		// Common Go pattern: "dial tcp 10.0.0.1:9090: connect: refused"
		// — keep up to the last meaningful segment.
		parts := strings.Split(msg, ":")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if len(msg) > 80 {
		return msg[:77] + "…"
	}
	return msg
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS_1.3"
	case tls.VersionTLS12:
		return "TLS_1.2"
	case tls.VersionTLS11:
		return "TLS_1.1"
	case tls.VersionTLS10:
		return "TLS_1.0"
	}
	return fmt.Sprintf("0x%04x", v)
}

// classifyTLSError tries to attribute a TLS dial failure to one of
// "tls" (handshake itself), "cert" (server cert chain), or "client"
// (client cert rejection). Heuristic — Go's TLS errors are strings,
// not typed.
//
// Returns (which, clientAccepted). When clientAccepted is false the
// server explicitly rejected our client cert; when it's true the
// failure was earlier in the handshake.
func classifyTLSError(err error) (string, bool) {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "bad certificate"),
		strings.Contains(msg, "certificate required"),
		strings.Contains(msg, "unknown ca") && strings.Contains(msg, "client"):
		return "client", false
	case strings.Contains(msg, "x509"),
		strings.Contains(msg, "certificate"),
		strings.Contains(msg, "unknown authority"):
		return "cert", true
	}
	return "tls", true
}

func appendWaiting(probes []probeResult, labels ...string) []probeResult {
	for _, l := range labels {
		probes = append(probes, probeResult{Label: l, Status: probeWait})
	}
	return probes
}

func ternaryProbe(cond bool, a, b probeStatus) probeStatus {
	if cond {
		return a
	}
	return b
}

func ternaryStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
