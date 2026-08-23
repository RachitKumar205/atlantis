package identity

import (
	"fmt"
	"regexp"
	"strings"
)

// Organisation names, validated in Go as well as in SQL.
//
// # Why this is not left to the database
//
// The rule has always existed — migration 0001 constrains cloud.orgs.name with
// `^[a-z0-9][a-z0-9-]{0,62}$` — but only there. Until now the only way to make
// an organisation was `cloud org create`, so a bad name came back as a raw
// check-constraint error to an operator who could read it. A browser form
// cannot show somebody `pq: new row violates check constraint
// "orgs_name_check"` and call it an answer.
//
// The duplication is deliberate and has a precedent in the same neighbourhood:
// AddMember validates the role in Go *as well as* in SQL, so a caller gets a
// sentence rather than a constraint name, and 0001's own comment records the
// Go/SQL duplication for roles as intended rather than accidental. This is the
// same trade. The database stays the enforcement; this is the explanation.
//
// A test asserts the two agree, because a Go rule that drifted looser than the
// CHECK would turn a clear refusal back into a constraint error, and one that
// drifted tighter would refuse names the database would have accepted.

// orgNamePattern mirrors the CHECK on cloud.orgs.name exactly.
//
// Sixty-three characters because that is a DNS label, and a name is a namespace
// in Kubernetes and a certificate common name before it is anything else.
var orgNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// reservedOrgNames are names nobody may take.
//
// Not a security boundary — none of these grants anything, and an organisation
// called `admin` has exactly the permissions any other has. What they would do
// is lie. A name appears in a URL a user is asked to trust
// (`/authorize?org=…`), in the path `tide` writes credentials to, and in the
// common name of a certificate authority. `atlantis` or `support` in any of
// those reads as the product speaking rather than a customer.
//
// Kept short on purpose. A long denylist is a long list of names real customers
// cannot have, and the cost of that is paid by people with ordinary names while
// the benefit is bounded.
var reservedOrgNames = map[string]bool{
	"admin":    true,
	"api":      true,
	"atlantis": true,
	"console":  true,
	"support":  true,
	"system":   true,
	"www":      true,
}

// ValidateOrgName reports why name may not be used, or nil.
//
// The messages name the rule rather than the constraint, and they never
// distinguish "somebody has this" from "nobody may have this" — see
// ErrOrgNameUnavailable at the call site for why that is honest rather than
// evasive.
func ValidateOrgName(name string) error {
	if name == "" {
		return fmt.Errorf("an organisation name is required")
	}
	// Reported before the pattern, because "must be lowercase" is a more useful
	// thing to hear about `Acme` than a description of the whole grammar.
	if strings.ToLower(name) != name {
		return fmt.Errorf("%q must be lowercase", name)
	}
	if len(name) > 63 {
		return fmt.Errorf("%q is %d characters; the limit is 63, because a name "+
			"has to fit a DNS label", name, len(name))
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%q may not start with a hyphen", name)
	}
	if !orgNamePattern.MatchString(name) {
		return fmt.Errorf("%q may use only lowercase letters, digits and hyphens, "+
			"and must start with a letter or digit", name)
	}
	if reservedOrgNames[name] {
		return fmt.Errorf("%q is reserved", name)
	}
	return nil
}

// ReservedOrgName reports whether name is one nobody may take.
//
// Exported separately so a caller can tell a reserved name from a taken one
// when it needs to — the create route deliberately does not, but a test does.
func ReservedOrgName(name string) bool { return reservedOrgNames[name] }
