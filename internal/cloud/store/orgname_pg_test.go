package store

import (
	"context"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The Go rule and the SQL CHECK agree.
//
// identity.ValidateOrgName duplicates `^[a-z0-9][a-z0-9-]{0,62}$` from
// migration 0001 so a browser gets a sentence instead of a constraint name. A
// duplicated rule that drifts is worse than no duplicate at all:
//
//   - looser in Go than in SQL, and the refusal an operator sees goes back to
//     being a raw constraint error from a route that thought it had checked
//   - tighter in Go than in SQL, and names the database would have accepted are
//     refused for no reason anybody can find
//
// This lives here rather than beside ValidateOrgName because
// internal/cloud/identity is deliberately a leaf package with nothing beyond
// the standard library — both halves of the assertion exchange depend on it, so
// a database driver added to its tests is one added to the console's graph.
func TestTheGoNameRuleAgreesWithTheDatabase(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// Spans the grammar's edges in both directions. Reserved names are excluded
	// deliberately: they are a product decision Go makes alone, and the database
	// neither knows nor should know about them — asserted separately below.
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

// A reserved name is refused by Go and accepted by the database, on purpose.
//
// The database has no opinion about `admin`; it is a perfectly good DNS label.
// Asserting the asymmetry stops somebody "fixing" the test above by adding a
// denylist to SQL, which would then have to be migrated every time the product
// changed its mind about a word.
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
