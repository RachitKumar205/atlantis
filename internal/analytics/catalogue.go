package analytics

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
	EventSignInFailed       = "account.sign_in_failed"
	EventIdentityLinked     = "oauth.identity_linked"
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
)

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
			}
		},
	},
	"org.requeued": {
		Event: EventOrgRequeued,
		Props: func(d map[string]any) map[string]any {
			return map[string]any{"attempts": num(d["attempts"])}
		},
	},
	"org.purged": {Event: EventOrgPurged},

	"org.console_credentials_rotated": {
		Why: "a scheduled certificate rotation, which answers no product question",
	},
}

// Not reported: starting a CLI login. The route is unauthenticated and no
// identity exists until the grant is approved, so an event there would create
// a person nothing can ever join to. cli.grant_approved carries the same
// funnel step with an actor behind it.

// str returns a string value, or "" for anything else.
func str(v any) string {
	s, _ := v.(string)
	return s
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
