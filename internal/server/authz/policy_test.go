package authz

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestEveryAdminMethodDeclaresACapability is the guard the whole package exists
// for. It is the same check the server runs at startup, so an RPC added without
// a required_capability fails here long before it can be deployed reachable by
// anyone. A failure means: add the option to the new method in admin.proto.
func TestEveryAdminMethodDeclaresACapability(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatalf("admin service has undeclared methods:\n%v", err)
	}

	sd := adminpb.File_atlantis_admin_v1_admin_proto.Services().ByName("AdminService")
	if got, want := len(policy.Methods()), sd.Methods().Len(); got != want {
		t.Errorf("policy covers %d methods, service declares %d", got, want)
	}
}

// TestBuildPolicyRejectsUndeclaredMethod proves the failure mode actually
// fires. Without it, TestEveryAdminMethodDeclaresACapability passing would only
// show that BuildPolicy returned nil, not that it can ever say no.
//
// The descriptor is synthesized here rather than borrowed from whatever
// capability-free service happens to be linked into the test binary: that
// approach silently skipped, which is the same as not having the test.
func TestBuildPolicyRejectsUndeclaredMethod(t *testing.T) {
	sd := syntheticService(t, "Alpha", "Beta")

	_, err := BuildPolicy(sd)
	if err == nil {
		t.Fatal("BuildPolicy accepted a service whose methods declare no capability")
	}
	// The message has to explain the fix and name every offender — a bare
	// "invalid policy" leaves whoever added the RPC guessing, and reporting
	// only the first turns a batch of new methods into a game of whack-a-mole.
	if !strings.Contains(err.Error(), "required_capability") {
		t.Errorf("error should explain the fix, got: %v", err)
	}
	for _, name := range []string{"Alpha", "Beta"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should name undeclared method %q, got: %v", name, err)
		}
	}
}

// syntheticService builds a service descriptor whose methods carry no options,
// so it is guaranteed capability-free regardless of what else is linked in.
func syntheticService(t *testing.T, methodNames ...string) protoreflect.ServiceDescriptor {
	t.Helper()

	methods := make([]*descriptorpb.MethodDescriptorProto, 0, len(methodNames))
	for _, name := range methodNames {
		methods = append(methods, &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String(".authztest.v1.Empty"),
			OutputType: proto.String(".authztest.v1.Empty"),
		})
	}

	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("authztest/v1/undeclared.proto"),
		Package:     proto.String("authztest.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:   proto.String("Undeclared"),
			Method: methods,
		}},
	}, nil)
	if err != nil {
		t.Fatalf("building synthetic descriptor: %v", err)
	}
	return fd.Services().Get(0)
}

// TestGetLogsIsNotSchemaRead pins the one mapping decision that is easy to get
// wrong on a later edit. The log ring carries driver error text — pgx renders a
// unique violation as `DETAIL: Key (email)=(alice@example.com)` — so folding it
// into SCHEMA_READ would hand every schema reader a row-sampling primitive.
func TestGetLogsIsNotSchemaRead(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	got := policy.Methods()["GetLogs"]
	if got != adminpb.Capability_CAPABILITY_LOGS_READ {
		t.Errorf("GetLogs requires %s, want CAPABILITY_LOGS_READ", got)
	}
}

// TestCrossCallerMethodsRequireOperator pins the methods whose blast radius
// lands on callers other than the one making the request. RollbackSchema in
// particular reverts the shared checkpoint for everyone in a single
// transaction.
func TestCrossCallerMethodsRequireOperator(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	methods := policy.Methods()
	for _, name := range []string{
		"RollbackSchema", "AdoptBaseline", "RegisterCaller", "RevokeCaller",
		"RecordCallerCertExpiry", "SetCallerAliases", "DrainWorker", "EvictWorker",
	} {
		if got := methods[name]; got != adminpb.Capability_CAPABILITY_OPERATOR {
			t.Errorf("%s requires %s, want CAPABILITY_OPERATOR", name, got)
		}
	}
}

func TestSetHasNoImplicitHierarchy(t *testing.T) {
	s := NewSet(adminpb.Capability_CAPABILITY_SCHEMA_APPLY)
	if s.Has(adminpb.Capability_CAPABILITY_SCHEMA_READ) {
		t.Error("SCHEMA_APPLY must not imply SCHEMA_READ")
	}
	if !s.Has(adminpb.Capability_CAPABILITY_SCHEMA_APPLY) {
		t.Error("SCHEMA_APPLY should be present")
	}
}

func TestNewSetDropsUnspecified(t *testing.T) {
	s := NewSet(adminpb.Capability_CAPABILITY_UNSPECIFIED)
	if s.Has(adminpb.Capability_CAPABILITY_UNSPECIFIED) {
		t.Error("a malformed grant must never satisfy a requirement")
	}
}

func TestInterceptor(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}

	tests := []struct {
		name    string
		method  string
		grants  Grants
		wantErr codes.Code
		wantRun bool
	}{
		{
			name:    "grant satisfies requirement",
			method:  "/atlantis.admin.v1.AdminService/GetMergedSchema",
			grants:  staticGrants(NewSet(adminpb.Capability_CAPABILITY_SCHEMA_READ)),
			wantRun: true,
		},
		{
			name:    "wrong capability is denied",
			method:  "/atlantis.admin.v1.AdminService/ApplyMigration",
			grants:  staticGrants(NewSet(adminpb.Capability_CAPABILITY_SCHEMA_READ)),
			wantErr: codes.PermissionDenied,
		},
		{
			name:    "empty grant set is denied",
			method:  "/atlantis.admin.v1.AdminService/GetMergedSchema",
			grants:  staticGrants(NewSet()),
			wantErr: codes.PermissionDenied,
		},
		{
			// An unreachable identity store must not widen access.
			name:    "grant lookup failure denies",
			method:  "/atlantis.admin.v1.AdminService/GetMergedSchema",
			grants:  GrantsFunc(func(context.Context) (Set, error) { return nil, context.DeadlineExceeded }),
			wantErr: codes.PermissionDenied,
		},
		{
			// Entity CRUD lives on the same server and carries its own authz.
			name:    "other services pass through",
			method:  "/atlantis.consumer.v1.UserService/GetUser",
			grants:  staticGrants(NewSet()),
			wantRun: true,
		},
		{
			name:    "unknown method under the admin prefix is denied",
			method:  "/atlantis.admin.v1.AdminService/MethodThatIsNotDeclared",
			grants:  staticGrants(NewSet(adminpb.Capability_CAPABILITY_OPERATOR)),
			wantErr: codes.PermissionDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			interceptor := policy.UnaryInterceptor(tc.grants)
			_, err := interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: tc.method}, handler)

			if tc.wantErr != codes.OK {
				if status.Code(err) != tc.wantErr {
					t.Fatalf("got %v (%v), want code %v", err, status.Code(err), tc.wantErr)
				}
				if called {
					t.Error("handler ran despite denial")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if called != tc.wantRun {
				t.Errorf("handler ran = %v, want %v", called, tc.wantRun)
			}
		})
	}
}

func staticGrants(s Set) Grants {
	return GrantsFunc(func(context.Context) (Set, error) { return s, nil })
}

// The policy governs both service paths. Enforcing only the generated one
// would leave the path production actually uses ungoverned — the same shape as
// the allowlist exemption this replaces.
func TestLegacyAdminPathIsGoverned(t *testing.T) {
	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	for _, prefix := range []string{"/atlantis.admin.v1.AdminService/", LegacyAdminPrefix} {
		t.Run(prefix, func(t *testing.T) {
			interceptor := policy.UnaryInterceptor(staticGrants(NewSet(adminpb.Capability_CAPABILITY_SCHEMA_READ)))

			// A read capability satisfies a read method on either path.
			if _, err := interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: prefix + "GetMergedSchema"}, handler); err != nil {
				t.Errorf("read denied on %s: %v", prefix, err)
			}
			// And is refused for an operator method on either path.
			_, err := interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: prefix + "RollbackSchema"}, handler)
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("operator method allowed on %s with only SCHEMA_READ: %v", prefix, err)
			}
		})
	}
}
