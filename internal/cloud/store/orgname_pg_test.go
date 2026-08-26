package store

import (
	"context"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// identity.ValidateOrgName duplicates `^[a-z0-9][a-z0-9-]{0,62}$` from
// migration 0001, so a browser gets a sentence instead of a constraint name.
// Looser in Go than in SQL and the refusal is a raw constraint error from a
// route that thought it had checked; tighter and names the database would have
// accepted are refused with no rule to point at.
//
// internal/cloud/identity carries nothing beyond the standard library, so a
// database driver in its tests is one in the console's graph. The assertion
// lives here instead.
func TestTheGoNameRuleAgreesWithTheDatabase(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// Spans the grammar's edges in both directions. Reserved names are excluded:
	// they are a Go-side product rule, asserted separately below.
	for _, name := range []string{
		"a", "acme", "acme-corp", "a1", "1acme", "a-b-c",
		strings.Repeat("a", 63),
		strings.Repeat("a", 64),
		"", "Acme", "ACME", "-acme", "acme_corp", "acme corp",
		"acme.corp", "acme/corp", "acme-", "a--b",
	} {
		goOK := identity.ValidateOrgName(name) == nil

		var sqlOK bool
		err := db.pool.QueryRow(ctx,
			`SELECT $1::text ~ '^[a-z0-9][a-z0-9-]{0,62}$'`, name).Scan(&sqlOK)
		if err != nil {
			t.Fatalf("probe %q: %v", name, err)
		}

		if goOK != sqlOK {
			t.Errorf("%q: Go says valid=%v, the database says valid=%v — the "+
				"duplicated rule has drifted", name, goOK, sqlOK)
		}
	}
}

// The database has no opinion about `admin`; it is a valid DNS label. The
// asymmetry is asserted so the test above is not "fixed" by adding a denylist
// to SQL, which would need a migration every time the list changed.
func TestReservedNamesAreAProductRuleNotADatabaseOne(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"admin", "atlantis", "support"} {
		if identity.ValidateOrgName(name) == nil {
			t.Errorf("%q was accepted by Go; it is reserved", name)
		}
		var sqlOK bool
		if err := db.pool.QueryRow(ctx,
			`SELECT $1::text ~ '^[a-z0-9][a-z0-9-]{0,62}$'`, name).Scan(&sqlOK); err != nil {
			t.Fatalf("probe %q: %v", name, err)
		}
		if !sqlOK {
			t.Errorf("%q is rejected by the database's grammar, so reserving it in "+
				"Go does nothing", name)
		}
	}
}
