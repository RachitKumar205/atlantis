package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The `orgs` claim, which is what a console draws its organisation switcher
// from.
//
// It is the one thing in an assertion that authorizes nothing. Every other
// claim is a decision — the role came from a membership row, the audience is
// the destination — and this is a list of names for a menu. That distinction is
// the whole reason a stale entry is harmless, and the tests below are what keep
// it true: one proves the names arrive, one proves nothing else does, and one
// proves the gate is still /authorize re-reading the row.

// alsoMember adds an existing account to a second organisation.
//
// f.member signs a new account up, so it cannot be used twice for one person —
// and one person in several organisations is the entire subject here.
func (f *fixture) alsoMember(t *testing.T, email, org, consoleURL string, role identity.Role) {
	t.Helper()
	ctx := context.Background()

	u, err := f.db.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("read %s: %v", email, err)
	}
	if err := f.db.CreateOrg(ctx, org, ""); err != nil {
		t.Fatalf("create org %s: %v", org, err)
	}
	if consoleURL != "" {
		if err := f.db.SetConsoleURL(ctx, org, consoleURL); err != nil {
			t.Fatalf("register console for %s: %v", org, err)
		}
	}
	if err := f.db.AddMember(ctx, u.ID, org, role); err != nil {
		t.Fatalf("add %s to %s: %v", email, org, err)
	}
}

// orgsOf follows /authorize and returns the minted token's `orgs` claim.
func (f *fixture) orgsOf(t *testing.T, session, org string) (token string, orgs []string) {
	t.Helper()
	rec := f.authorize(t, session, "org="+org)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authorize %s: %d %s", org, rec.Code, rec.Body.String())
	}
	token, _ = assertionFrom(t, rec)
	_, private := claimsOf(t, token)
	return token, private.Orgs
}

// TestTheAssertionNamesEveryMembershipInOrder.
//
// Ordered because the switcher is drawn straight from it: a menu whose entries
// move between page loads is one people stop trusting they clicked correctly.
// The order comes from MembershipsOf's ORDER BY, not from this list.
func TestTheAssertionNamesEveryMembershipInOrder(t *testing.T) {
	f := newFixture(t)
	const email = "many@example.com"
	session := f.member(t, email, "acme", testConsole, identity.RoleAdmin)
	f.alsoMember(t, email, "zeta", "https://zeta.console.example", identity.RoleViewer)
	f.alsoMember(t, email, "globex", "https://globex.console.example", identity.RoleViewer)

	_, orgs := f.orgsOf(t, session, "acme")
	want := []string{"acme", "globex", "zeta"}
	if len(orgs) != len(want) {
		t.Fatalf("orgs = %v, want %v", orgs, want)
	}
	for i := range want {
		if orgs[i] != want[i] {
			t.Fatalf("orgs = %v, want %v", orgs, want)
		}
	}
}

// TestTheOrgsClaimCarriesNamesOnly.
//
// Adding roles here discloses what a user may do in an organisation this
// console cannot reach, and the claim exists to draw a menu.
//
// Read from the raw JSON rather than through identity.Private, because a struct
// with no field for a value cannot see the value arriving.
func TestTheOrgsClaimCarriesNamesOnly(t *testing.T) {
	f := newFixture(t)
	const email = "namesonly@example.com"
	session := f.member(t, email, "acme", testConsole, identity.RoleAdmin)
	f.alsoMember(t, email, "globex", "https://globex.console.example", identity.RoleViewer)

	token, _ := f.orgsOf(t, session, "acme")

	parsed, err := jwt.ParseSigned(token, allowedTestAlgorithms)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := parsed.UnsafeClaimsWithoutVerification(&raw); err != nil {
		t.Fatalf("claims: %v", err)
	}
	body, ok := raw["orgs"]
	if !ok {
		t.Fatal("the token carries no orgs claim")
	}
	var names []string
	if err := json.Unmarshal(body, &names); err != nil {
		t.Fatalf("orgs is not an array of names: %s", body)
	}
	if len(names) != 2 {
		t.Fatalf("orgs = %v, want two names", names)
	}
	for _, r := range []string{string(identity.RoleAdmin), string(identity.RoleViewer)} {
		for _, n := range names {
			if n == r {
				t.Errorf("orgs carries a role: %v", names)
			}
		}
	}
}

// TestMembershipIsTheGateNotTheClaim.
//
// The list is a snapshot and a Cloud session lasts hours, so an organisation
// removed at Cloud keeps appearing in the switcher until the next sign-in.
// /authorize re-reads cloud.memberships, so the stale entry buys a refusal page
// rather than access.
//
// The assertion for the removed organisation is checked too. Without it this
// test would pass on a build where the claim was simply empty.
func TestMembershipIsTheGateNotTheClaim(t *testing.T) {
	f := newFixture(t)
	const email = "revoked@example.com"
	const globexConsole = "https://globex.console.example"
	session := f.member(t, email, "acme", testConsole, identity.RoleAdmin)
	f.alsoMember(t, email, "globex", globexConsole, identity.RoleViewer)

	// Before: the switcher has somewhere to go, and going there works.
	_, orgs := f.orgsOf(t, session, "acme")
	if len(orgs) != 2 {
		t.Fatalf("orgs = %v, want acme and globex", orgs)
	}
	if rec := f.authorize(t, session, "org=globex"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the switch did not work before the membership was revoked: %d %s",
			rec.Code, rec.Body.String())
	}

	u, err := f.db.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := f.db.RemoveMember(context.Background(), u.ID, "globex"); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	// After: the switch is refused, and nothing was minted for it.
	rec := f.authorize(t, session, "org=globex")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked membership still authorized a switch: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatalf("the refusal still redirected to %q", rec.Header().Get("Location"))
	}

	// And the claim follows the rows from the next mint onward, so the entry
	// disappears from the menu once the person signs in again.
	_, after := f.orgsOf(t, session, "acme")
	if len(after) != 1 || after[0] != "acme" {
		t.Fatalf("orgs after the revocation = %v, want [acme]", after)
	}
}

// TestAMembershipAddedLaterIsNotInAlreadyMintedAssertions.
//
// The other direction of the snapshot: joining an organisation does not make it
// appear in a console already open. It appears at the next mint, a sign-in or a
// switch.
func TestAMembershipAddedLaterIsNotInAlreadyMintedAssertions(t *testing.T) {
	f := newFixture(t)
	const email = "joiner@example.com"
	session := f.member(t, email, "acme", testConsole, identity.RoleAdmin)

	before, orgs := f.orgsOf(t, session, "acme")
	if len(orgs) != 1 || orgs[0] != "acme" {
		t.Fatalf("orgs = %v, want [acme]", orgs)
	}

	f.alsoMember(t, email, "globex", "https://globex.console.example", identity.RoleViewer)

	if _, again := claimsOf(t, before); len(again.Orgs) != 1 {
		t.Errorf("an assertion minted earlier changed: %v", again.Orgs)
	}
	if _, next := f.orgsOf(t, session, "acme"); len(next) != 2 {
		t.Errorf("the next mint did not pick the new membership up: %v", next)
	}
}
