// Stream interceptor flavors of the security-critical chain, mirroring the
// unary versions in this package so jobsdispatcher's streaming RPC gets the
// same auth and cert-binding walls as every admin unary RPC.
//
// Each interceptor extracts its core check into a shared helper in the same
// source file as its unary partner. The stream wrapper here calls that helper
// and forwards to the handler through a ctxStream carrying the modified
// context, so a handler reading identity via stream.Context() cannot tell the
// two apart.

package interceptors

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/obs"
)

// ctxStream wraps a grpc.ServerStream so that the handler sees a
// modified Context() — used to propagate the resolved caller from
// the stream-resolveCaller interceptor into downstream handlers and
// auth checks that read ctx.Value(callerKey{}).
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the modified context. All other ServerStream
// methods (RecvMsg, SendMsg, etc.) pass through to the embedded
// stream unchanged.
func (s *ctxStream) Context() context.Context { return s.ctx }

// WithStreamContext returns a ServerStream that reports the supplied
// ctx from Context(). Exported so cmd/server/auth.go's
// resolveCallerStreamInterceptor (which lives outside this package)
// can build the wrapper.
func WithStreamContext(ss grpc.ServerStream, ctx context.Context) grpc.ServerStream {
	return &ctxStream{ServerStream: ss, ctx: ctx}
}

// NewMetricsStream mirrors NewMetrics for streaming RPCs. The same
// closed-cardinality labels and recording semantics apply. For
// long-lived streams (e.g. WorkerSession) the histogram captures the
// total stream duration when it closes — useful for detecting
// pathologically short reconnect loops.
func NewMetricsStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		code := codes.Unknown.String()
		defer func() {
			obs.GRPCRequestDuration.WithLabelValues(info.FullMethod, code).Observe(time.Since(start).Seconds())
			obs.GRPCRequestsTotal.WithLabelValues(info.FullMethod, code).Inc()
		}()
		err := handler(srv, ss)
		code = status.Code(err).String()
		return err
	}
}
