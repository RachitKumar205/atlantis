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
// # What this trusts, and what it does not
//
// The tenant is asserted by the calling service in request metadata. That
// service has already authenticated the end user and is the only party that
// knows whose request this is. atlantis does not derive it and does not
// second-guess it: a caller that wanted another tenant's rows could query its
// own database directly, so a check here would protect nothing.
//
// The guarantee is narrower and more useful than "callers cannot lie". Once a
// tenant is asserted for a request, every statement in that request's
// transaction is confined to it — including SQL atlantis did not generate and
// did not inspect. The failure this prevents is the accidental one: a forgotten
// predicate, a new handler, a custom query body. That is the failure that
// actually happens.
//
// # Why an interceptor rather than a per-handler read
//
// The same reason the isolation itself is a database policy rather than a
// predicate in each generated read. An interceptor cannot be forgotten by a
// handler added later. A handler that reads the header itself can.
//
// # Absence is not an error here
//
// A request with no tenant header reaches the handler with no partition in
// context, and the dispatcher then refuses any partitioned entity while serving
// ordinary ones normally. Rejecting here instead would break every request to
// every unpartitioned entity on a deployment that has one partitioned table.
// The refusal belongs where the requirement is known.
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
// # Two different tenants is an error, not a choice
//
// Taking the first value is what this did, and it is wrong in the topology that
// matters: where an intermediary appends an authoritative tenant to whatever
// the client sent, first-wins hands the decision to the client. Nothing here
// can tell which value came from where, so the honest answer is to refuse.
//
// cmd/server/proxyauth.go already refuses the same shape for the same reason —
// "trusted proxy %q forwarded multiple client certificates" — and an isolation
// decision should not be more permissive about ambiguity than an identity one.
// Repeated copies of the SAME tenant are accepted: that is a duplicate, not a
// disagreement.
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
