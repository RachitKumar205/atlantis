package main

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
)

// The refusal a freshly enrolled caller actually gets.
//
// Copied from a real run: enrol a machine with `tide login`, run `tide plan`,
// and this is the whole of what comes back. It is true and it is useless — it
// names an enum constant, does not say who grants it, and does not say that a
// new caller has none of the three by default, which is the situation nearly
// every time it fires.
func refusal(capability string) error {
	return status.Error(codes.PermissionDenied,
		"authz: /atlantis.admin.v1.AdminService/PlanSchema requires "+capability)
}

func TestACapabilityRefusalSaysWhatToDo(t *testing.T) {
	got := explainAuthz(refusal("CAPABILITY_SCHEMA_PLAN"))
	if got == nil {
		t.Fatal("explainAuthz swallowed the error")
	}
	msg := got.Error()

	// The original survives: a bug report needs the RPC and the capability.
	if !strings.Contains(msg, "CAPABILITY_SCHEMA_PLAN") {
		t.Error("the explanation dropped the capability the server named")
	}
	// And the part a person can act on.
	for _, want := range []string{"Callers page", "read access only"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the explanation does not mention %q:\n%s", want, msg)
		}
	}
	// The wrong remedy, ruled out explicitly. Enrolling again is the obvious
	// thing to try after `tide login` and it fixes nothing.
	if !strings.Contains(msg, "Enrolling again") {
		t.Errorf("the explanation does not rule out re-enrolling:\n%s", msg)
	}
}

func TestAnUnrelatedErrorIsUntouched(t *testing.T) {
	for _, err := range []error{
		errors.New("dial tcp: connection refused"),
		status.Error(codes.Unavailable, "server is restarting"),
		// PermissionDenied, but not a capability refusal — the message shape
		// this must not claim to understand.
		status.Error(codes.PermissionDenied, "caller is revoked"),
	} {
		if got := explainAuthz(err); got.Error() != err.Error() {
			t.Errorf("explainAuthz rewrote an unrelated error:\n got  %v\n want %v", got, err)
		}
	}
	if explainAuthz(nil) != nil {
		t.Error("explainAuthz turned nil into an error")
	}
}

// The three capabilities this file calls "mutate" are the three the server
// grants from can_mutate.
//
// Asserted against the server's own list rather than restated, because the
// remedy differs: a missing mutate capability means "re-register with mutate
// permission", and a missing one from any other bundle does not. A list that
// drifted would send people to a remedy that cannot work, which is worse than
// the unhelpful message this replaced.
func TestTheMutateCapabilitiesMatchTheServer(t *testing.T) {
	server := map[string]bool{}
	for _, c := range authz.DefaultCapabilities(true) {
		server[c.String()] = true
	}
	for _, c := range authz.DefaultCapabilities(false) {
		delete(server, c.String())
	}

	if len(server) != len(mutateCapabilities) {
		t.Fatalf("the server grants %d capabilities from can_mutate and this file lists %d:\n"+
			" server %v\n here   %v", len(server), len(mutateCapabilities), server, mutateCapabilities)
	}
	for name := range server {
		if !mutateCapabilities[name] {
			t.Errorf("%s comes from can_mutate on the server but is not listed here, "+
				"so its refusal gets the wrong remedy", name)
		}
	}
}

// A capability outside the mutate bundle gets the generic remedy, not the
// can_mutate one.
func TestANonMutateCapabilityGetsTheGenericRemedy(t *testing.T) {
	msg := explainAuthz(refusal(adminpb.Capability_CAPABILITY_OPERATOR.String())).Error()
	if strings.Contains(msg, "read access only") {
		t.Errorf("an operator capability was explained as a missing can_mutate:\n%s", msg)
	}
	if !strings.Contains(msg, "Callers page") {
		t.Errorf("the generic remedy does not say where to go:\n%s", msg)
	}
}
