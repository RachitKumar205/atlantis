// Package authz enforces the capability each Admin RPC declares in the proto.
//
// The admin prefix is exempt from the caller allowlist, so authorization
// written inside a method body leaves any method whose author forgot the check
// reachable by anyone who can open a connection. Per-method checks also
// disagree with each other: one falling back to a global wildcard when an env
// var is unset — the shipped default — makes every mutation-capable caller an
// operator too.
//
// Requirements are read from the service descriptor, so they live in the file
// that defines the method; one interceptor applies them to every call; and
// BuildPolicy fails when any method declares nothing, which callers wire into
// startup so the server refuses to boot rather than serving an open endpoint.
package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Policy is the extracted requirement for every method of one service, keyed by
// bare method name and resolved against grpc.UnaryServerInfo.FullMethod by
// trimming whichever governed prefix matches.
type Policy struct {
	// prefixes are the "/<service full name>/" strings this policy governs.
	//
	// A slice although one entry is used today: a service answering on two
	// paths at once needs both governed, or the ungoverned path is an open
	// endpoint. BuildPolicy's alsoGovern parameter expresses that.
	prefixes []string
	byName   map[string]adminpb.Capability
}

// requirement resolves a full method name against every governed prefix,
// returning the capability and whether this policy owns the method at all.
func (p *Policy) requirement(fullMethod string) (adminpb.Capability, bool, bool) {
	for _, prefix := range p.prefixes {
		if !strings.HasPrefix(fullMethod, prefix) {
			continue
		}
		c, known := p.byName[strings.TrimPrefix(fullMethod, prefix)]
		return c, known, true
	}
	return adminpb.Capability_CAPABILITY_UNSPECIFIED, false, false
}

// BuildPolicy reads every method's required_capability from the descriptor.
//
// alsoGovern lists additional "/<service>/" prefixes this policy answers for,
// beyond the descriptor's own. A parameter rather than a constant, so building
// a policy for another service does not claim a path that service does not
// serve and deny every call on it.
//
// A method that declares nothing is an error, not a default: the zero value of
// the enum is CAPABILITY_UNSPECIFIED, so omitting the option is
// indistinguishable from writing it wrong and both stop the server. Callers
// treat the error as fatal at startup; downgraded to a warning it serves the
// undeclared method.
//
// The error names every offending method rather than the first, so adding a
// batch of RPCs surfaces all the missing declarations in one run.
func BuildPolicy(sd protoreflect.ServiceDescriptor, alsoGovern ...string) (*Policy, error) {
	methods := sd.Methods()
	p := &Policy{
		prefixes: append([]string{"/" + string(sd.FullName()) + "/"}, alsoGovern...),
		byName:   make(map[string]adminpb.Capability, methods.Len()),
	}

	var undeclared []string
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		capability := declaredCapability(m)
		if capability == adminpb.Capability_CAPABILITY_UNSPECIFIED {
			undeclared = append(undeclared, string(m.Name()))
			continue
		}
		p.byName[string(m.Name())] = capability
	}

	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return nil, fmt.Errorf(
			"authz: %d method(s) on %s declare no required_capability: %s\n"+
				"Every RPC must set it in the proto, e.g.\n"+
				"  rpc Foo(FooRequest) returns (FooResponse) {\n"+
				"    option (required_capability) = CAPABILITY_SCHEMA_READ;\n"+
				"  }\n"+
				"Use CAPABILITY_PUBLIC if the endpoint is deliberately unauthenticated.",
			len(undeclared), sd.FullName(), strings.Join(undeclared, ", "))
	}
	return p, nil
}

// declaredCapability pulls the extension off a method's options. A method with
// no options at all, or options carrying no extension, yields UNSPECIFIED,
// which BuildPolicy rejects.
func declaredCapability(m protoreflect.MethodDescriptor) adminpb.Capability {
	opts, ok := m.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil {
		return adminpb.Capability_CAPABILITY_UNSPECIFIED
	}
	if !proto.HasExtension(opts, adminpb.E_RequiredCapability) {
		return adminpb.Capability_CAPABILITY_UNSPECIFIED
	}
	capability, ok := proto.GetExtension(opts, adminpb.E_RequiredCapability).(adminpb.Capability)
	if !ok {
		return adminpb.Capability_CAPABILITY_UNSPECIFIED
	}
	return capability
}

// Prefixes returns the service paths this policy governs, so a test can drive
// every one of them rather than a list it maintains separately.
func (p *Policy) Prefixes() []string {
	return append([]string(nil), p.prefixes...)
}

// Methods returns the requirement per bare method name, for tests and for the
// startup log line recording what the server will enforce.
func (p *Policy) Methods() map[string]adminpb.Capability {
	out := make(map[string]adminpb.Capability, len(p.byName))
	for k, v := range p.byName {
		out[k] = v
	}
	return out
}

// Set is a caller's granted capabilities.
type Set map[adminpb.Capability]struct{}

// NewSet builds a Set from a list, dropping UNSPECIFIED so a malformed grant
// row can never satisfy a requirement.
func NewSet(caps ...adminpb.Capability) Set {
	s := make(Set, len(caps))
	for _, c := range caps {
		if c == adminpb.Capability_CAPABILITY_UNSPECIFIED {
			continue
		}
		s[c] = struct{}{}
	}
	return s
}

// Has reports whether the set contains c. There is no hierarchy: SCHEMA_APPLY
// does not imply SCHEMA_READ. Implication would make the grant recorded on an
// identity differ from the access it confers.
func (s Set) Has(c adminpb.Capability) bool {
	_, ok := s[c]
	return ok
}

// Grants resolves the capabilities held by whoever is making a request. The
// production implementation reads them from caller_identities keyed by the
// mTLS common name resolved upstream in the interceptor chain.
type Grants interface {
	For(ctx context.Context) (Set, error)
}

// GrantsFunc adapts a function to Grants.
type GrantsFunc func(ctx context.Context) (Set, error)

func (f GrantsFunc) For(ctx context.Context) (Set, error) { return f(ctx) }

// UnaryInterceptor enforces the policy on every call.
//
// A method outside this service's prefix passes through untouched: the
// interceptor is installed server-wide and the generated entity services carry
// their own authorization.
//
// A method inside the prefix and absent from the policy is denied. BuildPolicy
// makes that unreachable, so arriving here means the server's registered
// methods and its descriptor have diverged.
//
// A grant lookup that errors denies rather than falling back to a default: an
// unreachable identity store must not widen access.
func (p *Policy) UnaryInterceptor(grants Grants) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		required, known, governed := p.requirement(info.FullMethod)
		if !governed {
			return handler(ctx, req)
		}
		if !known {
			return nil, status.Errorf(codes.PermissionDenied,
				"authz: %s is not declared in the service descriptor", info.FullMethod)
		}
		if required == adminpb.Capability_CAPABILITY_PUBLIC {
			return handler(ctx, req)
		}

		held, err := grants.For(ctx)
		if err != nil {
			return nil, status.Errorf(codes.PermissionDenied,
				"authz: cannot resolve caller capabilities: %v", err)
		}
		if !held.Has(required) {
			return nil, status.Errorf(codes.PermissionDenied,
				"authz: %s requires %s", info.FullMethod, required)
		}
		return handler(ctx, req)
	}
}

// AdminPolicy builds the policy for the Admin service from the compiled-in
// descriptor. Wire the error into startup.
func AdminPolicy() (*Policy, error) {
	sd := adminpb.File_atlantis_admin_v1_admin_proto.Services().ByName("AdminService")
	if sd == nil {
		return nil, fmt.Errorf("authz: AdminService not found in the compiled descriptor")
	}
	return BuildPolicy(sd)
}
