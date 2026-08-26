package identity

import (
	"fmt"
	"regexp"
	"strings"
)

// Organisation names, validated in Go as well as in SQL.
//
// Duplicated from the database, where migration 0001 constrains cloud.orgs.name
// with `^[a-z0-9][a-z0-9-]{0,62}$`. The constraint alone reports a bad name as
// `pq: new row violates check constraint "orgs_name_check"`, which a browser
// form cannot render. AddMember duplicates its role check the same way.
//
// The database stays the enforcement; this is the message. A test asserts the
// two agree.

// orgNamePattern mirrors the CHECK on cloud.orgs.name exactly.
//
// Sixty-three characters because that is a DNS label, and a name is a namespace
// in Kubernetes and a certificate common name before it is anything else.
var orgNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// reservedOrgNames are the names an organisation may not take.
//
// Not a security boundary: an organisation called `admin` holds exactly the
// permissions any other does. A name appears in `/authorize?org=…`, in the path
// `tide` writes credentials to, and in a certificate common name, where
// `atlantis` or `support` reads as the product rather than a customer.
//
// Kept short: every entry is a name no customer can have.
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
// The messages name the rule rather than the constraint. They do not
// distinguish a taken name from a reserved one; see ErrOrgNameUnavailable.
func ValidateOrgName(name string) error {
	if name == "" {
		return fmt.Errorf("an organisation name is required")
	}
	// Before the pattern, so `Acme` reports the case rule rather than the whole
	// grammar.
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

// ReservedOrgName reports whether name is reserved.
//
// Exported so a caller can tell a reserved name from a taken one. The create
// route does not; a test does.
func ReservedOrgName(name string) bool { return reservedOrgNames[name] }
