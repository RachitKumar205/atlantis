package interceptors

import (
	"context"

	"google.golang.org/grpc"
)

// ChainUnary composes chain into one interceptor. The first element runs
// outermost, the order grpc.ChainUnaryInterceptor uses.
func ChainUnary(chain ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return unaryFrom(chain, 0, info, handler)(ctx, req)
	}
}

func unaryFrom(chain []grpc.UnaryServerInterceptor, i int, info *grpc.UnaryServerInfo, final grpc.UnaryHandler) grpc.UnaryHandler {
	if i == len(chain) {
		return final
	}
	return func(ctx context.Context, req any) (any, error) {
		return chain[i](ctx, req, info, unaryFrom(chain, i+1, info, final))
	}
}

// ChainStream composes chain into one interceptor. The first element runs
// outermost, the order grpc.ChainStreamInterceptor uses.
func ChainStream(chain ...grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return streamFrom(chain, 0, info, handler)(srv, ss)
	}
}

func streamFrom(chain []grpc.StreamServerInterceptor, i int, info *grpc.StreamServerInfo, final grpc.StreamHandler) grpc.StreamHandler {
	if i == len(chain) {
		return final
	}
	return func(srv any, ss grpc.ServerStream) error {
		return chain[i](srv, ss, info, streamFrom(chain, i+1, info, final))
	}
}

// StreamChainUnless runs chain around every stream except those for which
// skip reports true, which reach the handler directly.
//
// The dynamic entity dispatcher serves unary RPCs through the server's
// UnknownServiceHandler, a stream handler, and applies the unary chain
// itself; running the stream chain as well would authenticate, meter and log
// each of those calls twice.
func StreamChainUnless(skip func(fullMethod string) bool, chain ...grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	chained := ChainStream(chain...)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if skip(info.FullMethod) {
			return handler(srv, ss)
		}
		return chained(srv, ss, info, handler)
	}
}
