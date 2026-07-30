package authz

import adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"

// The default capability bundles a caller receives from its can_mutate flag.
//
// can_mutate is a boolean, and a boolean cannot express an authorization model
// — that is the whole reason caller_capabilities exists. It survives here as an
// input, not as the model: RegisterCaller translates it into an explicit set of
// grants that are then the thing the server reads. An operator who wants
// something other than these bundles grants it directly, and RegisterCaller
// will not take it away (see ManagedCapabilities).
//
// These sets are the same ones migrations/infra/0018_caller_capabilities.up.sql
// backfilled onto callers that predate the table. The migration applied the
// rule once, historically; this applies it going forward. A test asserts the
// two agree, because a caller registered before and after the migration having
// different authority would be a bug nobody would notice until an incident.
var (
	// baseCapabilities go to every registered caller. A caller that can
	// authenticate at all can read the schema it is being asked to conform to,
	// and can observe the job and worker state of the system it runs against.
	baseCapabilities = []adminpb.Capability{
		adminpb.Capability_CAPABILITY_SCHEMA_READ,
		adminpb.Capability_CAPABILITY_JOBS_READ,
		adminpb.Capability_CAPABILITY_WORKERS_READ,
	}

	// mutateCapabilities are added when can_mutate is set. They cover this
	// caller's own namespace only; nothing here reaches another caller's
	// schema, which is what separates them from CAPABILITY_OPERATOR.
	mutateCapabilities = []adminpb.Capability{
		adminpb.Capability_CAPABILITY_SCHEMA_PLAN,
		adminpb.Capability_CAPABILITY_SCHEMA_APPLY,
		adminpb.Capability_CAPABILITY_JOBS_WRITE,
	}
)

// DefaultCapabilities returns the grants a caller gets from its can_mutate
// flag. The result is freshly allocated so a caller cannot mutate the bundles.
func DefaultCapabilities(canMutate bool) []adminpb.Capability {
	out := make([]adminpb.Capability, 0, len(baseCapabilities)+len(mutateCapabilities))
	out = append(out, baseCapabilities...)
	if canMutate {
		out = append(out, mutateCapabilities...)
	}
	return out
}

// ManagedCapabilities is every capability RegisterCaller owns — the union of
// both bundles, regardless of can_mutate.
//
// Registration is a full reconciliation of this set and nothing else: flipping
// can_mutate off revokes the mutating grants, and a capability outside this set
// (CAPABILITY_OPERATOR, CAPABILITY_LOGS_READ) is left exactly as an operator
// left it. Without the boundary, re-registering a caller to correct a typo in
// its created_by would silently strip operator authority someone deliberately
// granted; with it, the two mechanisms compose instead of fighting.
func ManagedCapabilities() []adminpb.Capability {
	out := make([]adminpb.Capability, 0, len(baseCapabilities)+len(mutateCapabilities))
	out = append(out, baseCapabilities...)
	out = append(out, mutateCapabilities...)
	return out
}

// Names renders capabilities as the enum names stored in the capability
// column. The column holds names rather than numbers so the table stays
// readable during an incident; see the migration for why.
func Names(caps []adminpb.Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.String())
	}
	return out
}
