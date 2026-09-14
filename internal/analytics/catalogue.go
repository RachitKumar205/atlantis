package analytics

import "reflect"

// The catalogue turns an audit action into an event.
//
// Audit detail maps were written for a log behind row-level security. They
// carry customer text, credentials and infrastructure detail: a device code, an
// organisation's display name, a Kubernetes API URL. Nothing here copies a map
// through. Each Projection names the keys it emits and the rest is dropped,
// so reviewing this file is the whole review.

// Projection turns one audit action into an event.
type Projection struct {
	// Event is the event name. Empty means the action is deliberately not
	// reported, and Why says so.
	Event string

	// Props builds the event properties from the audit detail. Nil emits none.
	// It reads named keys and writes named keys.
	Props func(detail map[string]any) map[string]any

	// Why records the reason an action is not reported. Required when Event
	// is empty.
	Why string
}

// Cloud event names.
const (
	EventSignedUp           = "account.signed_up"
	EventEmailVerified      = "account.email_verified"
	EventSecondFactor       = "account.second_factor_enrolled"
	EventSignedIn           = "account.signed_in"
	EventConsoleAuthorized  = "console.authorized"
	EventOrgCreated         = "org.created"
	EventOrgDeleted         = "org.deleted"
	EventOrgRestored        = "org.restored"
	EventCLIGrantApproved   = "cli.grant_approved"
	EventCLIGrantDenied     = "cli.grant_denied"
	EventCLIGrantConsumed   = "cli.grant_consumed"
	EventOrgProvisioned     = "org.provisioned"
	EventOrgProvisionFailed = "org.provision.failed"
	EventOrgRequeued        = "org.requeued"
	EventOrgPurged          = "org.purged"
	EventOrgPurgeFailed     = "org.purge.failed"

	// EventOrgProvisionStep is captured by the worker. cloud.audit_log holds a
	// handful of rows per organisation for its lifetime, and four per attempt
	// times every retry is a different table.
	EventOrgProvisionStep = "org.provision.step"

	// EventCLICommand is one finished tide run. One name with a command
	// property: PostHog breaks an event down by a property, and 22 names would
	// be 22 insights to union.
	EventCLICommand = "cli.command"
)

// Organisation group properties. GroupIdentify is called with these four keys,
// so this is where a new one is reviewed.
//
// Each is a fact about the organisation's lifecycle that this deployment
// produced. $group_set merges the keys it is given, so a call may send a
// subset.
//
// provision_attempts counts every attempt the row has taken, including those
// before a requeue; provisioning.go does not reset it.
const (
	GroupState         = "state"
	GroupProvisionedAt = "provisioned_at"
	GroupPurgedAt      = "purged_at"
	GroupAttempts      = "provision_attempts"
)

// States an organisation group carries.
const (
	StateReady  = "ready"
	StatePurged = "purged"
)

// Person properties. Event.Person is set with these keys and no others, so
// this is where a new one is reviewed.
//
// PersonEmail is a customer's address held by a third party for as long as the
// project retains it, which is what separates it from every other value this
// package emits: the projections reduce customer text to booleans and classes,
// and this does not.
const PersonEmail = "$email"

// CloudActions projects the actions written to cloud.audit_log.
//
// A key here has a matching string literal at a LogAction call site, and every
// such literal has a key. TestEveryCloudAuditActionIsProjected holds both
// directions.
var CloudActions = map[string]Projection{
	"org.created": {
		Event: EventOrgCreated,
		// The display name is customer text.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"display_name_set": str(d["display_name"]) != ""}
		},
	},
	"org.deleted": {
		Event: EventOrgDeleted,
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"restorable_for": str(d["restorable_for"])}
		},
	},
	"org.restored": {Event: EventOrgRestored},

	// The caller name is a customer-chosen identifier with unbounded
	// cardinality; the console's caller count answers the same question.
	"cli_grant_consumed": {Event: EventCLIGrantConsumed},

	// user_code is a live credential for the few minutes a grant is pending.
	"cli_grant_approved": {Event: EventCLIGrantApproved},
	"cli_grant_denied":   {Event: EventCLIGrantDenied},

	"org.provisioned": {
		Event: EventOrgProvisioned,
		// endpoint is a cluster hostname.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{
				"attempts": num(d["attempts"]),
				"took_ms":  num(d["took_ms"]),
			}
		},
	},
	"org.provision.failed": {
		Event: EventOrgProvisionFailed,
		// The error text carries image references and cluster addresses, and
		// orgs.go already withholds it from the API for that reason.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{
				"attempts":    num(d["attempts"]),
				"error_class": ErrorClass(str(d["error"])),
				"retry_in":    str(d["retry_in"]),
				"phase":       str(d["phase"]),
				"fault":       str(d["fault"]),
			}
		},
	},
	// The reason is free text bound for the console's audit view, and reconcile
	// holds no attempt count: Requeue does not touch the column.
	"org.requeued": {Event: EventOrgRequeued},
	"org.purged":   {Event: EventOrgPurged},

	// A deletion that did not finish. The row concerns the account and outlives
	// the organisation, which is what puts it in the audit log; purgeOne
	// releases the claim, so one arrives per reap pass until a teardown
	// succeeds.
	"org.purge.failed": {
		Event: EventOrgPurgeFailed,
		// The error text carries cluster addresses, as org.provision.failed's
		// does.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{
				"step":        str(d["step"]),
				"error_class": ErrorClass(str(d["error"])),
				"fault":       str(d["fault"]),
			}
		},
	},

	"org.console_credentials_rotated": {
		Why: "a scheduled certificate rotation, which answers no product question",
	},

	"org.images_rolled": {
		Why: "an operator moving an organisation onto a built image, which answers " +
			"no product question. The audit row is where a restart is traced back " +
			"to the command that asked for it",
	},
}

// Not reported: starting a CLI login. The route is unauthenticated and no
// identity exists until the grant is approved, so an event there would create
// a person nothing can ever join to. cli.grant_approved carries the same
// funnel step with an actor behind it.

// ConsoleActions projects the actions written to console.audit_log.
//
// A second table because the console is a second database with its own
// actions; TestEveryConsoleAuditActionIsProjected holds this one to
// internal/console the way CloudActions is held to internal/cloud.
//
// What the console records is operator work on a customer's own schema, so
// most of these details are customer text: caller names, entity patterns,
// OIDC subjects, certificate fingerprints, remote addresses and free-text
// reasons. What crosses is the shape of the action — an enum, a count, a
// boolean — and the identifiers stay in Postgres.
var ConsoleActions = map[string]Projection{
	// Callers.
	"register_caller": {
		Event: "console.caller_registered",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"can_mutate": d["can_mutate"] == true}
		},
	},
	"revoke_caller":  {Event: "console.caller_revoked"},
	"restore_caller": {Event: "console.caller_restored"},
	"revoke_all_callers": {
		Event: "console.all_callers_revoked",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"revoked": num(d["revoked"]), "failures": num(d["failures"])}
		},
	},
	"set_caller_aliases": {
		Event: "console.caller_aliases_set",
		// The aliases are names a customer chose.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"aliases": count(d["aliases"])}
		},
	},
	"caller_enrollment_set": {
		Event: "console.caller_enrolment_set",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"developers_may_enroll": d["developers_may_enroll"] == true}
		},
	},

	// Enrolment. Every detail on these paths identifies a machine or a person:
	// fingerprints, common names, remote addresses, OIDC issuers and subjects.
	"enroll_token_minted": {Event: "console.enrol_token_minted"},
	"enroll_token_spent":  {Event: "console.enrol_token_spent"},
	"enroll_assertion_spent": {
		Event: "console.enrol_assertion_spent",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"role": str(d["role"])}
		},
	},
	"oidc_enrolled": {Event: "console.oidc_enrolled"},
	"certificate_renewed": {
		Event: "console.certificate_renewed",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"replaced": d["replaced"] == true}
		},
	},
	"federation_rule_created": {Event: "console.federation_rule_created"},
	"federation_rule_revoked": {Event: "console.federation_rule_revoked"},

	// Schema changes and the gates around them.
	"plan_rehearsed": {
		Event: "console.plan_rehearsed",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"verdict": str(d["verdict"])}
		},
	},
	"approve_schema_plan": {
		Event: "console.plan_approved",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"role": str(d["role"])}
		},
	},
	"reject_schema_plan": {
		Event: "console.plan_rejected",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"role": str(d["role"])}
		},
	},
	// The reason is what an operator typed.
	"override_schema_plan": {Event: "console.plan_overridden"},
	"rollback_schema": {
		Event: "console.schema_rolled_back",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"to_version": num(d["to_version"])}
		},
	},
	"schema_import_applied": {
		Event: "console.schema_import_applied",
		// The entity names are the customer's schema.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"entities": count(d["entities"])}
		},
	},

	// Policy.
	"apply_policy_set": {
		Event: "console.apply_policy_set",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{
				"apply_policy":      str(d["apply_policy"]),
				"rehearsal_enabled": d["rehearsal_enabled"] == true,
			}
		},
	},
	"set_change_policy": {
		Event: "console.change_policy_set",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"entries": count(d["entries"])}
		},
	},
	"protected_entity_put": {
		Event: "console.protected_entity_put",
		// The pattern names a customer entity.
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"floor": str(d["floor"])}
		},
	},
	"protected_entity_deleted": {Event: "console.protected_entity_deleted"},
	"freeze_window_created":    {Event: "console.freeze_window_created"},
	"freeze_window_deleted":    {Event: "console.freeze_window_deleted"},

	// Sandboxes. The two sandbox_booted call sites share backend and a schema
	// hash; the hash identifies one customer's schema.
	"sandbox_booted": {
		Event: "console.sandbox_booted",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"backend": str(d["backend"])}
		},
	},
	"sandbox_destroyed": {Event: "console.sandbox_destroyed"},

	// Jobs and workers.
	"retry_dead_job": {Event: "console.dead_job_retried"},
	"worker_drained": {Event: "console.worker_drained"},
	"worker_evicted": {Event: "console.worker_evicted"},

	// Sessions.
	"signed_in": {Event: "console.signed_in"},
	"sudo_granted": {
		Event: "console.sudo_granted",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"ttl_seconds": num(d["ttl_seconds"])}
		},
	},
	"sign_out_others": {
		Event: "console.signed_out_others",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"sessions_removed": num(d["sessions_removed"])}
		},
	},
	"sign_out_all": {
		Event: "console.signed_out_all",
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"sessions_removed": num(d["sessions_removed"])}
		},
	},
}

// str returns a string value, or "" for anything else.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// count returns the length of a slice or map, or 0.
//
// It reports how many of something an action touched without reading any of
// them: the elements are entity names, caller aliases and policy entries.
// Reflection because the call sites build []string, []any and richer element
// types, and a type switch over each is a list that goes stale.
func count(v any) int {
	if v == nil {
		return 0
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Len()
	default:
		return 0
	}
}

// num returns a numeric value as a float64, or 0.
//
// An audit detail read back from JSONB arrives as float64; one built in
// process is whatever the handler put there.
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
