package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
)

// transportCreds builds the gRPC credentials.TransportCredentials for the
// listener. mTLS is the only mode:
//
//   - Server presents Cert from TLSCertFile / TLSKeyFile.
//   - Clients are REQUIRED to present a cert signed by TLSCAFile.
//   - ClientAuth = RequireAndVerifyClientCert.
//
// # Why there is no insecure mode
//
// There used to be one, taken whenever TLS_CERT_FILE was empty, and it did far
// more than skip encryption. The same emptiness turned off the caller
// allowlist, the caller-to-cert binding, and admin capability enforcement,
// because all three read `cfg.TLSCertFile != ""` as "is this deployment
// authenticated". A developer running it was not running a less-encrypted
// atlantis; they were running one with no authorization at all, against which
// no authorization change could be tested.
//
// loadConfig now requires all three paths, so this function cannot be reached
// without them. `make dev-certs` writes a local CA and the leaf certs.
//
// The internal CA management is out of scope here; atlantis just consumes the
// resulting PEM files.
func transportCreds(cfg config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}
	pool, err := loadClientCAPool(cfg.TLSCAFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// loadClientCAPool reads the client-CA PEM bundle into an x509 pool. The
// same pool roots both the listener's mTLS verification and the trusted-
// proxy forwarded-cert re-validation, so a forwarded cert is held to the
// exact trust root a directly-connecting client would be.
func loadClientCAPool(caFile string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file %s contained no usable certs", caFile)
	}
	return pool, nil
}

// callerKey is the context key for the resolved caller identity.
type callerKey struct{}

// callerFromContext returns the resolved caller name. In TLS mode it's the
// CN of the client cert; in insecure dev mode it's the value of the
// x-caller metadata header (defaulting to "anonymous").
func callerFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(callerKey{}).(string); ok {
		return v
	}
	return "anonymous"
}

// applyResolvedCaller computes the caller identity for ctx and returns a
// derived context carrying it. In trusted-proxy mode, when the live peer is
// a configured proxy, identity comes from the re-validated forwarded client
// cert (and the forwarded DER is stashed for the cert-binding check);
// otherwise identity is the live peer cert CN / dev x-caller header. A
// non-nil error means a trusted proxy forwarded no/invalid identity — the
// RPC is rejected Unauthenticated.
func applyResolvedCaller(ctx context.Context, fa *forwardedAuth) (context.Context, error) {
	cn, certDER, forwarded, err := fa.resolve(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if forwarded {
		ctx = context.WithValue(ctx, callerKey{}, cn)
		ctx = interceptors.WithForwardedCert(ctx, certDER)
		return ctx, nil
	}
	return context.WithValue(ctx, callerKey{}, resolveCaller(ctx)), nil
}

// resolveCallerInterceptor populates the callerKey context value from the
// peer's TLS cert (production), a trusted-proxy-forwarded cert, or the
// x-caller header (dev). Subsequent interceptors and handlers read it via
// callerFromContext.
func resolveCallerInterceptor(fa *forwardedAuth) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := applyResolvedCaller(ctx, fa)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// resolveCallerStreamInterceptor is the streaming sibling of
// resolveCallerInterceptor. Wraps the ServerStream with a context
// that carries the resolved caller key so cert binding + auth
// interceptors (and the WorkerSession handler itself) can read it
// via callerFromContext just like they would on a unary RPC.
func resolveCallerStreamInterceptor(fa *forwardedAuth) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := applyResolvedCaller(ss.Context(), fa)
		if err != nil {
			return err
		}
		return handler(srv, interceptors.WithStreamContext(ss, ctx))
	}
}

func resolveCaller(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			if len(tlsInfo.State.PeerCertificates) > 0 {
				cn := tlsInfo.State.PeerCertificates[0].Subject.CommonName
				if cn != "" {
					return cn
				}
			}
		}
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vs := md.Get("x-caller"); len(vs) > 0 && vs[0] != "" {
			return vs[0]
		}
	}
	return "anonymous"
}

// loggingInterceptor logs every RPC at completion with method, caller, code,
// duration. We pair it with the resolve-caller interceptor so caller is
// already populated by the time this runs.
func loggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		logRPC(log, info.FullMethod, callerFromContext(ctx), err)
		return resp, err
	}
}

// loggingStreamInterceptor is the streaming sibling of loggingInterceptor.
// Logs at stream close with the same shape so an audit grep on
// method/caller/code finds both unary and stream events.
func loggingStreamInterceptor(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, ss)
		logRPC(log, info.FullMethod, callerFromContext(ss.Context()), err)
		return err
	}
}

func logRPC(log *slog.Logger, method, caller string, err error) {
	code := status.Code(err)
	if code == codes.OK {
		log.Debug("rpc", "method", method, "caller", caller)
		return
	}
	log.Info("rpc", "method", method, "caller", caller, "code", code.String(), "err", err)
}

// recoveryInterceptor catches panics from downstream handlers and converts
// them to Internal errors so a panic in one RPC doesn't crash the server.
// (Generated handlers SHOULD NOT panic, but human handlers might.)
func recoveryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in handler",
					"method", info.FullMethod,
					"caller", callerFromContext(ctx),
					"panic", r)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// recoveryStreamInterceptor is the streaming sibling of recoveryInterceptor.
// Long-lived streams (WorkerSession) make panic recovery especially
// important — without it, one malformed envelope in a handler could
// kill the whole server.
func recoveryStreamInterceptor(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in stream handler",
					"method", info.FullMethod,
					"caller", callerFromContext(ss.Context()),
					"panic", r)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}
