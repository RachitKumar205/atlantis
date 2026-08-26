package interceptors

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// PartitionHeader is the request metadata key carrying the tenant a request is
// for.
//
// Lowercase by convention, not by necessity: grpc-go lowercases metadata keys
// on store and lowercases the argument to MD.Get, so any casing works. Written
// this way to match how the key appears on the wire.
const PartitionHeader = "atlantis-tenant"

// NewPartition attaches the tenant a request asserts to the request context, so
// that `partition by` has something to bind.
//
// The tenant is asserted by the calling service in request metadata. That
// service authenticated the end user; atlantis does not derive or second-guess
// it, since a caller wanting another tenant's rows could query its own database.
//
// The guarantee is not that callers cannot lie. Once a tenant is asserted for a
// request, every statement in that request's transaction is confined to it,
// including SQL atlantis did not generate and did not inspect. What that stops
// is a forgotten predicate, a new handler, or a custom query body.
//
// An interceptor rather than a per-handler read, so a handler added later
// cannot omit it.
//
// A missing header is not an error here. The request reaches the handler with
// no partition in context and the dispatcher refuses partitioned entities only,
// so unpartitioned ones keep serving.
func NewPartition() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		next, err := withPartition(ctx)
		if err != nil {
			return nil, err
		}
		return handler(next, req)
	}
}

// NewPartitionStream is the streaming flavor. Kept beside the unary one so the
// two cannot drift on the header name or the trimming rule.
func NewPartitionStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		next, err := withPartition(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &partitionStream{ServerStream: ss, ctx: next})
	}
}

type partitionStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *partitionStream) Context() context.Context { return s.ctx }

// withPartition reads the header and attaches it, or returns ctx unchanged.
//
// An empty or whitespace-only value is treated as absent. It would otherwise
// reach set_partition, which rejects it — but as an error from the database
// rather than as the missing header it is, and the two want different fixes.
//
// Two values for the tenant header are refused rather than resolved. Where an
// intermediary appends an authoritative tenant to whatever the client sent,
// first-wins hands the decision to the client, and nothing here can tell which
// value came from where.
//
// cmd/server/proxyauth.go refuses the same shape for the same reason —
// "trusted proxy %q forwarded multiple client certificates". Repeated copies of
// the same tenant are accepted, being a duplicate rather than a disagreement.
func withPartition(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, nil
	}
	var tenant string
	for _, v := range md.Get(PartitionHeader) {
		got := strings.TrimSpace(v)
		if got == "" {
			continue
		}
		if tenant != "" && got != tenant {
			return nil, status.Errorf(codes.InvalidArgument,
				"request carries %d different %s values; atlantis cannot tell which "+
					"one is authoritative and will not guess", len(md.Get(PartitionHeader)),
				PartitionHeader)
		}
		tenant = got
	}
	if tenant == "" {
		return ctx, nil
	}
	return runtime.WithCallerPartition(ctx, tenant), nil
}
