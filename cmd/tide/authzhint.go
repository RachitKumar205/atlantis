package main

import (
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The server refuses with the truth and nothing else:
//
//	rpc error: code = PermissionDenied desc = authz:
//	  /atlantis.admin.v1.AdminService/PlanSchema requires CAPABILITY_SCHEMA_PLAN
//
// Accurate, and it tells the person reading it nothing they can act on. It
// names an enum constant that appears in no document they have read, does not
// say who can grant it, and does not say that a freshly registered caller has
// none of the three by default — which is the actual situation almost every
// time this fires.
//
// This was found by enrolling a machine and running `tide plan`, which is the
// first thing anybody does after `tide login`.

var capabilityPattern = regexp.MustCompile(`requires (CAPABILITY_[A-Z_]+)`)

// mutateCapabilities are the three a caller gets from can_mutate, and the ones
// a new caller is missing. Kept in sync with internal/server/authz/defaults.go
// by TestTheMutateCapabilitiesMatchTheServer, because a list that drifts would
// send people to the wrong remedy.
var mutateCapabilities = map[string]bool{
	"CAPABILITY_SCHEMA_PLAN":  true,
	"CAPABILITY_SCHEMA_APPLY": true,
	"CAPABILITY_JOBS_WRITE":   true,
}

// explainAuthz returns err with an explanation appended when it is a capability
// refusal, and unchanged otherwise.
//
// Appended rather than replacing. The original names the RPC and the
// capability, which is what a bug report carries; the explanation is what the
// terminal needs.
func explainAuthz(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		return err
	}
	m := capabilityPattern.FindStringSubmatch(st.Message())
	if m == nil {
		return err
	}
	capability := m[1]

	var b strings.Builder
	b.WriteString(err.Error())
	b.WriteString("\n\n")
	if mutateCapabilities[capability] {
		b.WriteString("  This caller was registered without permission to change anything.\n")
		b.WriteString("  A new caller gets read access only; plan, apply and job writes\n")
		b.WriteString("  come together and are not granted by default.\n\n")
		b.WriteString("  An admin turns them on from the console's Callers page, or by\n")
		b.WriteString("  re-registering the caller with mutate permission. Enrolling again\n")
		b.WriteString("  will not help — the certificate is fine; the grant is missing.\n")
	} else {
		b.WriteString(fmt.Sprintf("  This caller does not hold %s.\n\n", capability))
		b.WriteString("  An admin grants it from the console's Callers page. Enrolling again\n")
		b.WriteString("  will not help — the certificate is fine; the grant is missing.\n")
	}
	return fmt.Errorf("%s", b.String())
}
