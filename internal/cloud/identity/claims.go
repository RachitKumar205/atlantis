// Package identity defines the claim set Atlantis Cloud puts into the
// assertions it issues, and that a console verifies before it will open a
// session.
//
// A leaf package with no dependencies beyond the standard library, imported by
// both internal/cloud/issuer, which mints these claims, and
// internal/console/cloudauth, which verifies them. One definition, so changing
// the wire format on one side stops the other compiling.
package identity

import (
	"errors"
	"fmt"
	"time"
)

// Role is a console role. Cloud decides a user's role within an organisation
// and asserts it; the console stores no role of its own.
//
// requireRole compares against these constants exactly. "administrator",
// "Admin" and the empty string are refused rather than mapped: an unmatched
// value authenticates the request and then fails every authorization check,
// which is indistinguishable from a revoked grant.
type Role string

const (
	RoleAdmin Role = "admin"

	// RoleDeveloper sits between viewer and admin: everything a viewer reads,
	// plus booting sandboxes and triggering rehearsals, plus being nameable as
	// a change policy's approver_role. Registration, minting, policy edits and
	// rollback stay admin.
	RoleDeveloper Role = "developer"

	RoleViewer Role = "viewer"
)

// Valid reports whether r is a role this console understands.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleDeveloper || r == RoleViewer }

// Private is the set of claims Cloud adds beyond the registered ones in
// RFC 7519. They are serialised alongside iss/sub/aud/exp/nbf/iat rather than
// nested under a namespace, which keeps the token readable in jwt.io and any
// other standard tool.
type Private struct {
	Org   string `json:"org"`
	Role  Role   `json:"role"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`

	// StepUp records that a second factor was presented to obtain this
	// assertion, rather than a live session being spent for one.
	//
	// Omitted when false, so an ordinary sign-in carries no claim at all. Never
	// assumed; see the field in Claims.
	StepUp bool `json:"step_up,omitempty"`

	// Orgs names every organisation the subject belongs to, sorted.
	//
	// Names only, no roles or endpoints, so a console draws its switcher
	// without calling Cloud per page. A hint; see the field in Claims.
	Orgs []string `json:"orgs,omitempty"`

	// OrgNames maps an organisation's name to what it calls itself, for the
	// organisations that call themselves something.
	//
	// Only the ones that differ. An organisation whose display name equals its
	// name is absent, so the common case adds nothing to the token, and a
	// console that finds no entry draws the name it already has.
	//
	// A hint, exactly as Orgs is: it decides what a switcher renders and never
	// what anybody may reach.
	OrgNames map[string]string `json:"org_names,omitempty"`

	// Purpose scopes an assertion to one exchange. Empty on a session
	// assertion; "cli-enroll" on one minted for certificate enrolment. The
	// session exchange refuses any non-empty value, and the enrolment listener
	// requires its own, so neither audience accepts the other's token even
	// where the two URLs are misconfigured to coincide.
	Purpose string `json:"purpose,omitempty"`

	// Caller names the caller identity a cli-enroll assertion may enrol as,
	// bound at approval so the machine cannot ask for a different one than the
	// person saw. Empty on every other assertion.
	Caller string `json:"caller,omitempty"`
}

// Claims is a verified assertion. A value of this type means the signature
// checked out against a published key, the time bounds held, and every field
// below was present — see Validate for what "present" is required to mean.
//
// It is what the console turns into a session.
type Claims struct {
	// ID is the assertion's unique identifier, the `jti` claim.
	//
	// It is what lets an assertion be spent exactly once. The token travels
	// through a browser, so a captured one could otherwise buy a second session
	// or be re-posted to /api/auth/sudo for step-up without the round trip.
	ID string

	// Subject is Cloud's immutable identifier for the user. It becomes the
	// audit actor, so it must not be an email: an email can be reassigned to
	// a different person, which would retroactively change who an audit row
	// says acted.
	Subject string

	// Org is the organisation the user is acting in. One stack per org, so
	// this is also the stack, and step 4 binds it as the RLS discriminator.
	Org string

	Role  Role
	Email string
	Name  string

	// Expiry is the assertion's own expiry, not the session's. The console
	// mints a session with its own lifetime on the strength of this; it does
	// not extend the assertion.
	Expiry time.Time

	// StepUp reports that a second factor was presented to obtain this
	// assertion.
	//
	// Single-use is not enough on its own: /authorize mints a fresh assertion
	// from a Cloud session, and that session lasts twelve hours without a
	// factor being presented.
	//
	// False on an ordinary sign-in, true only when Cloud's step-up route checked
	// a factor. Never defaulted or inferred: an assertion that does not say a
	// factor was presented did not have one presented.
	StepUp bool

	// Orgs names every organisation the subject belongs to, at the moment this
	// assertion was minted.
	//
	// A snapshot that populates a switcher. A membership revoked after minting
	// keeps appearing until the session ends, which costs one refusal page
	// because /authorize re-reads cloud.memberships.
	//
	// Do not authorize from this list.
	//
	// Empty is normal, and a console that finds it empty draws no switcher.
	Orgs []string

	// OrgNames maps organisation name to display name, for those that differ.
	// A hint, like Orgs; see the field in Private.
	OrgNames map[string]string

	// Purpose and Caller mirror the fields in Private. Both empty on a session
	// assertion; see there for what each binds.
	Purpose string
	Caller  string
}

// ErrMissingClaim reports an assertion that verified cryptographically but did
// not carry a field the console requires.
var ErrMissingClaim = errors.New("assertion is missing a required claim")

// Validate checks that every claim the console depends on is present and
// well-formed.
//
// Separate from signature and time validation, which catch a forged token
// rather than a genuine one from a misconfigured issuer. A signed token with no
// org opens a session bound to the empty organisation, which matches no rows
// under the RESTRICTIVE policies and presents as an account with no data.
//
// go-jose does not enforce the presence of exp: its validator reads
// `if c.Expiry != nil`, so an assertion omitting the claim passes every time
// check and never expires. The non-zero Expiry check closes that.
func (c Claims) Validate() error {
	switch {
	case c.ID == "":
		// Without jti the console cannot tell a fresh assertion from one it
		// has already spent.
		return fmt.Errorf("%w: jti", ErrMissingClaim)
	case c.Subject == "":
		return fmt.Errorf("%w: sub", ErrMissingClaim)
	case c.Org == "":
		return fmt.Errorf("%w: org", ErrMissingClaim)
	case c.Email == "":
		return fmt.Errorf("%w: email", ErrMissingClaim)
	case c.Role == "":
		return fmt.Errorf("%w: role", ErrMissingClaim)
	case !c.Role.Valid():
		return fmt.Errorf("unknown role %q: expected %q, %q or %q", c.Role, RoleAdmin, RoleDeveloper, RoleViewer)
	case c.Expiry.IsZero():
		return fmt.Errorf("%w: exp", ErrMissingClaim)
	}
	return nil
}
