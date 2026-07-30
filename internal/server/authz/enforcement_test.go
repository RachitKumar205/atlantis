package authz

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestEveryMethodIsDeniedWithoutItsGrant is the enforcement counterpart to
// TestEveryAdminMethodDeclaresACapability. That test proves every method
// declares something; this one proves the declaration is load-bearing.
//
// It enumerates methods from the descriptor rather than from a list, so an RPC
// added tomorrow is covered the moment it exists. For each method it asserts
// both directions: the declared capability admits the call, and no other single
// capability does. The second half is what catches an accidental hierarchy —
// if someone makes OPERATOR imply everything, this fails on every method.
//
// Every governed prefix is exercised. There is one today; during the proto
// migration there were two, and a policy that governed only the path not in
// production would have passed every other test here while enforcing nothing.
// Driving whatever Policy actually claims to govern keeps that true if a second
// path is ever added again.
func TestEveryMethodIsDeniedWithoutItsGrant(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}

	all := []adminpb.Capability{
		adminpb.Capability_CAPABILITY_SCHEMA_READ,
		adminpb.Capability_CAPABILITY_SCHEMA_PLAN,
		adminpb.Capability_CAPABILITY_SCHEMA_APPLY,
		adminpb.Capability_CAPABILITY_JOBS_READ,
		adminpb.Capability_CAPABILITY_JOBS_WRITE,
		adminpb.Capability_CAPABILITY_WORKERS_READ,
		adminpb.Capability_CAPABILITY_LOGS_READ,
		adminpb.Capability_CAPABILITY_OPERATOR,
	}

	methods := policy.Methods()
	if len(methods) == 0 {
		t.Fatal("policy covers no methods; the rest of this test would vacuously pass")
	}

	prefixes := policy.Prefixes()
	if len(prefixes) == 0 {
		t.Fatal("policy governs no prefix; every sub-test below would be skipped " +
			"and this test would pass having asserted nothing")
	}
	for _, prefix := range prefixes {
		for name, required := range methods {
			fullMethod := prefix + name

			t.Run(fmt.Sprintf("%s%s/granted", prefix, name), func(t *testing.T) {
				ran := false
				handler := func(context.Context, any) (any, error) { ran = true; return nil, nil }
				interceptor := policy.UnaryInterceptor(staticGrants(NewSet(required)))
				if _, err := interceptor(context.Background(), nil,
					&grpc.UnaryServerInfo{FullMethod: fullMethod}, handler); err != nil {
					t.Fatalf("%s denied to a caller holding %s: %v", fullMethod, required, err)
				}
				if !ran {
					t.Fatalf("%s: handler did not run despite a passing check", fullMethod)
				}
			})

			// CAPABILITY_PUBLIC is unauthenticated by declaration, so "denied
			// without the grant" does not apply to it. Nothing currently
			// declares it; the branch exists so adding one does not silently
			// weaken this test into a failure nobody can act on.
			if required == adminpb.Capability_CAPABILITY_PUBLIC {
				continue
			}

			t.Run(fmt.Sprintf("%s%s/ungranted", prefix, name), func(t *testing.T) {
				for _, held := range all {
					if held == required {
						continue
					}
					ran := false
					handler := func(context.Context, any) (any, error) { ran = true; return nil, nil }
					interceptor := policy.UnaryInterceptor(staticGrants(NewSet(held)))
					_, err := interceptor(context.Background(), nil,
						&grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)
					if status.Code(err) != codes.PermissionDenied {
						t.Errorf("%s admitted a caller holding only %s (wants %s): %v",
							fullMethod, held, required, err)
					}
					if ran {
						t.Errorf("%s: handler ran for a caller holding only %s", fullMethod, held)
					}
				}
			})
		}
	}
}

// TestNoCapabilityGrantsEverything guards the shape of the bug this package
// replaced: a gate that fell back to a wildcard, making one grant equivalent to
// all of them. Holding any single capability must leave at least one method
// refused — otherwise that capability is a superuser bit whatever it is named.
func TestNoCapabilityGrantsEverything(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, any) (any, error) { return nil, nil }

	for _, held := range []adminpb.Capability{
		adminpb.Capability_CAPABILITY_OPERATOR,
		adminpb.Capability_CAPABILITY_SCHEMA_APPLY,
		adminpb.Capability_CAPABILITY_LOGS_READ,
	} {
		interceptor := policy.UnaryInterceptor(staticGrants(NewSet(held)))
		refused := 0
		for name := range policy.Methods() {
			if _, err := interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: "/atlantis.admin.v1.AdminService/" + name},
				handler); err != nil {
				refused++
			}
		}
		if refused == 0 {
			t.Errorf("%s admits every method; it is a wildcard, not a capability", held)
		}
	}
}

// TestUnknownCallerHoldsNothing pins the deny-by-default posture at the layer
// that decides it. A caller with no row in caller_capabilities resolves to an
// empty set, and an empty set must satisfy no requirement.
func TestUnknownCallerHoldsNothing(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	interceptor := policy.UnaryInterceptor(staticGrants(NewSet()))
	handler := func(context.Context, any) (any, error) {
		t.Fatal("handler ran for a caller with no grants")
		return nil, nil
	}
	for name := range policy.Methods() {
		_, err := interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/atlantis.admin.v1.AdminService/" + name}, handler)
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s was not denied to a caller with no grants: %v", name, err)
		}
	}
}

// TestAdminServiceDeclaresNoStreamingRPC holds the invariant that "the policy
// covers a method" implies "the server enforces it".
//
// Policy has only a unary interceptor, and cmd/server's stream chain has no
// capability entry. Adding `rpc Tail(...) returns (stream ...)` with a
// required_capability compiles, records a requirement in BuildPolicy, and
// serves with no check whatsoever — grpcServer embeds
// UnimplementedAdminServiceServer precisely so a new method does not break the
// build, so nothing else catches it either.
//
// Whoever adds the first streaming admin RPC should add Policy.StreamInterceptor
// and install it in cmd/server's ChainStreamInterceptor, then delete this test.
// Until then the invariant is held here rather than by a comment claiming a
// guard that does not exist.
func TestAdminServiceDeclaresNoStreamingRPC(t *testing.T) {
	sd := adminpb.File_atlantis_admin_v1_admin_proto.Services().ByName("AdminService")
	if sd == nil {
		t.Fatal("AdminService not found in the compiled descriptor")
	}
	methods := sd.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		if m.IsStreamingClient() || m.IsStreamingServer() {
			t.Errorf("%s is a streaming RPC, and Policy enforces capabilities only on unary "+
				"calls. It would be served by cmd/server's stream chain, which has no "+
				"capability interceptor — authorized by nothing. Add Policy.StreamInterceptor "+
				"and wire it before shipping this method.", m.Name())
		}
	}
}
