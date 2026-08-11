package interceptors

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The interceptor must put the asserted tenant where BindPartition looks.
//
// Until this existed, runtime.WithCallerPartition had no production caller at
// all: the policy was emitted, the discriminator worked, the dispatcher bound
// it — and every request on a partitioned entity failed because nothing ever
// supplied a tenant. Two adversarial reviews reported it as "the feature is
// wired to nothing".
func TestPartitionInterceptorAttachesTheTenant(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   metadata.MD
		want string // "" means no partition should be attached
	}{
		{"header present", metadata.Pairs(PartitionHeader, "acme"), "acme"},
		{"mixed case key", metadata.Pairs("Atlantis-Tenant", "acme"), "acme"},
		{"surrounding whitespace", metadata.Pairs(PartitionHeader, "  acme  "), "acme"},
		{"no header", metadata.MD{}, ""},
		{"empty value", metadata.Pairs(PartitionHeader, ""), ""},
		{"whitespace only", metadata.Pairs(PartitionHeader, "   "), ""},
		// The same tenant twice is a duplicate, not a disagreement.
		{"same value repeated", metadata.MD{PartitionHeader: []string{"acme", "acme"}}, "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), tc.md)

			var got any
			var err error
			_, ierr := NewPartition()(ctx, nil,
				&grpc.UnaryServerInfo{FullMethod: "/x/Y"},
				func(ctx context.Context, _ any) (any, error) {
					got, err = runtime.CallerPartition(ctx)
					return nil, nil
				})
			if ierr != nil {
				t.Fatalf("interceptor: %v", ierr)
			}

			if tc.want == "" {
				if err == nil {
					t.Errorf("a tenant %v was attached from %v. The handler would "+
						"then bind it, and a request that asserted nothing would be "+
						"served as some tenant", got, tc.md)
				}
				return
			}
			if err != nil {
				t.Fatalf("no tenant attached from %v: %v. Every partitioned entity "+
					"fails for this request", tc.md, err)
			}
			if got != tc.want {
				t.Errorf("attached %q, want %q", got, tc.want)
			}
		})
	}
}

// A request with no metadata at all must not panic and must not attach.
func TestPartitionInterceptorWithoutMetadata(t *testing.T) {
	_, err := NewPartition()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/x/Y"},
		func(ctx context.Context, _ any) (any, error) {
			if _, err := runtime.CallerPartition(ctx); err == nil {
				t.Error("attached a tenant for a request carrying no metadata")
			}
			return nil, nil
		})
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
}

// The streaming flavor must attach it too, on the stream's own context.
//
// Kept under test because the two flavors are separate functions: a header
// name or a trimming rule fixed in one and not the other is the bug this
// prevents.
func TestPartitionStreamInterceptorAttachesTheTenant(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(PartitionHeader, "acme"))

	err := NewPartitionStream()(nil, fakeStream{ctx: ctx},
		&grpc.StreamServerInfo{FullMethod: "/x/Y"},
		func(_ any, ss grpc.ServerStream) error {
			got, err := runtime.CallerPartition(ss.Context())
			if err != nil {
				t.Errorf("no tenant on the stream context: %v", err)
				return nil
			}
			if got != "acme" {
				t.Errorf("attached %q, want acme", got)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

// Two different tenants in one request must be refused, not resolved.
//
// First-wins hands the decision to whichever value arrived first, which in any
// topology where an intermediary appends an authoritative tenant is the
// client's. cmd/server/proxyauth.go refuses the same ambiguity for forwarded
// client certificates; an isolation decision should not be more permissive
// about it than an identity one.
func TestPartitionInterceptorRefusesConflictingTenants(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.MD{PartitionHeader: []string{"attacker", "trusted"}})

	ran := false
	_, err := NewPartition()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"},
		func(context.Context, any) (any, error) { ran = true; return nil, nil })
	if err == nil {
		t.Error("served a request carrying two different tenants")
	}
	if ran {
		t.Error("the handler ran despite the tenant being ambiguous")
	}

	err = NewPartitionStream()(nil, fakeStream{ctx: ctx},
		&grpc.StreamServerInfo{FullMethod: "/x/Y"},
		func(any, grpc.ServerStream) error {
			t.Error("the stream handler ran despite the tenant being ambiguous")
			return nil
		})
	if err == nil {
		t.Error("the stream flavour served a request carrying two different tenants")
	}
}
