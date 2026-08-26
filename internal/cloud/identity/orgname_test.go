package identity

import (
	"strings"
	"testing"
)

// The grammar, tested here; the agreement with SQL is tested in
// internal/cloud/store, which already has a database.
//
// Splitting them keeps this package a leaf with nothing beyond the standard
// library. Both halves of the assertion exchange import it, so a dependency
// added here is one added to the console.

func TestValidOrgNamesAreAccepted(t *testing.T) {
	for _, name := range []string{
		"a",
		"acme",
		"acme-corp",
		"a1",
		"1acme",
		"acme-corp-europe-west",
		strings.Repeat("a", 63), // exactly the limit
	} {
		if err := ValidateOrgName(name); err != nil {
			t.Errorf("ValidateOrgName(%q) = %v, want nil", name, err)
		}
	}
}

// Every rejection names the rule, not a constraint.
//
// Otherwise a browser form renders
// `pq: new row violates check constraint "orgs_name_check"`.
func TestInvalidOrgNamesAreRefusedWithAReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string // a fragment the message must contain
	}{
		{"", "required"},
		{"Acme", "lowercase"},
		{"ACME", "lowercase"},
		{"-acme", "hyphen"},
		{"acme_corp", "lowercase letters, digits and hyphens"},
		{"acme corp", "lowercase letters, digits and hyphens"},
		{"acme.corp", "lowercase letters, digits and hyphens"},
		{"acme/corp", "lowercase letters, digits and hyphens"},
		{strings.Repeat("a", 64), "63"},
	} {
		err := ValidateOrgName(tc.name)
		if err == nil {
			t.Errorf("ValidateOrgName(%q) = nil, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateOrgName(%q) = %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// Every reserved name is refused, and the message says so.
//
// These are not a security boundary — an organisation called `admin` has the
// permissions any other has. What they would do is lie, in a URL a user is
// asked to trust and in the common name of a certificate authority.
func TestReservedNamesAreRefused(t *testing.T) {
	for _, name := range []string{"admin", "api", "atlantis", "console", "support", "system", "www"} {
		if !ReservedOrgName(name) {
			t.Errorf("ReservedOrgName(%q) = false", name)
		}
		err := ValidateOrgName(name)
		if err == nil {
			t.Fatalf("ValidateOrgName(%q) = nil; a reserved name was accepted", name)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ValidateOrgName(%q) = %q, want it to say reserved", name, err)
		}
	}
}

// A reserved name is refused for being reserved, not for its shape.
//
// Every reserved word is a valid name by the grammar. A reserved word the
// pattern already rejects makes its own reserved check dead code, and the name
// is still refused, so nothing reports it.
func TestEveryReservedNameWouldOtherwiseBeValid(t *testing.T) {
	for _, name := range []string{"admin", "api", "atlantis", "console", "support", "system", "www"} {
		if !orgNamePattern.MatchString(name) {
			t.Errorf("%q is reserved but the grammar rejects it anyway, so the "+
				"reserved entry does nothing", name)
		}
	}
}
