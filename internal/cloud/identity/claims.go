// Package identity defines the claim set Atlantis Cloud puts into the
// assertions it issues, and that a console verifies before it will open a
// session.
//
// It is a leaf package with no dependencies beyond the standard library
// because both halves of the exchange must agree on it: internal/cloud/issuer
// mints these claims and internal/console/cloudauth verifies them. Holding one
// definition means the wire format cannot be changed on one side only — the
// other side stops compiling.
package identity

import (
	"errors"
	"fmt"
	"time"
)

// Role is a console role. Cloud decides a user's role within an organisation
// and asserts it; the console stores no role of its own.
//
// The named type exists so that an unrecognised value cannot quietly become a
// role. Being liberal in what we accept is the wrong instinct here: a token
// carrying "administrator", or "Admin", or the empty string has to be refused
// rather than mapped onto something plausible, because every mapping that is
// not an exact match is a privilege decision made by a typo. requireRole
// compares against these constants, so an unmatched value would otherwise
// authenticate a request and then silently fail every authorization check —
// a user who appears signed in and can do nothing, with no way to tell that
// from a revoked grant.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a role this console understands.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleViewer }

// Private is the set of claims Cloud adds beyond the registered ones in
// RFC 7519. They are serialised alongside iss/sub/aud/exp/nbf/iat rather than
// nested under a namespace, which keeps the token readable in jwt.io and any
// other standard tool.
type Private struct {
	Org   string `json:"org"`
	Role  Role   `json:"role"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Claims is a verified assertion. A value of this type means the signature
// checked out against a published key, the time bounds held, and every field
// below was present — see Validate for what "present" is required to mean.
//
// It is what the console turns into a session.
type Claims struct {
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
}

// ErrMissingClaim reports an assertion that verified cryptographically but did
// not carry a field the console requires.
var ErrMissingClaim = errors.New("assertion is missing a required claim")

// Validate checks that every claim the console depends on is present and
// well-formed.
//
// This is separate from signature and time validation because it catches a
// different failure: not a forged token, but a genuine one from a
// misconfigured or newer issuer. Those must be refused just as firmly. A
// signed token with no org would otherwise open a session bound to the empty
// organisation, which under the RESTRICTIVE policies of step 4 matches no rows
// — presenting as an account with no data rather than as a failure, and
// inviting someone to "fix" it by loosening the policy.
//
// Note especially that go-jose does not enforce the presence of exp: its
// validator reads `if c.Expiry != nil`, so an assertion that simply omits the
// claim passes every time check. An assertion that never expires is a
// permanent credential, and Cloud has no way to withdraw it. Requiring a
// non-zero Expiry here is what closes that.
func (c Claims) Validate() error {
	switch {
	case c.Subject == "":
		return fmt.Errorf("%w: sub", ErrMissingClaim)
	case c.Org == "":
		return fmt.Errorf("%w: org", ErrMissingClaim)
	case c.Email == "":
		return fmt.Errorf("%w: email", ErrMissingClaim)
	case c.Role == "":
		return fmt.Errorf("%w: role", ErrMissingClaim)
	case !c.Role.Valid():
		return fmt.Errorf("unknown role %q: expected %q or %q", c.Role, RoleAdmin, RoleViewer)
	case c.Expiry.IsZero():
		return fmt.Errorf("%w: exp", ErrMissingClaim)
	}
	return nil
}
