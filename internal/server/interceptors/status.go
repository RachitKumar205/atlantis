package interceptors

import (
	"context"

	"google.golang.org/grpc"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// NewStatus returns the interceptor that turns a handler's error into a gRPC
// status, through runtime.ToStatus.
//
// It belongs innermost in the chain, last in the slice: the logging and
// metrics interceptors outside it then record the mapped code.
func NewStatus() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		return resp, runtime.ToStatus(err)
	}
}

// NewStatusStream is NewStatus for streaming handlers.
func NewStatusStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return runtime.ToStatus(handler(srv, ss))
	}
}
