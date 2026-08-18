package pgcatalog

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

// IsolatedRoleDSN creates a NOSUPERUSER NOBYPASSRLS role and returns a DSN for
// it against the same database.
//
// # Why a test needs this at all
//
// Row-level security is only a boundary if the connecting role is subject to
// it. FORCE ROW LEVEL SECURITY binds a table's owner; it binds a superuser to
// nothing, and it binds a role holding BYPASSRLS to nothing. A test fixture
// that connects as the cluster's administrative role therefore runs every case
// with the policies attached and inert — and an isolation assertion under those
// conditions passes for precisely the reason the isolation was meant to
// prevent.
//
// So the returned role is also the one the caller should MIGRATE as, because
// creating the tables is what makes it their owner.
//
// # Why it lives here
//
// Three packages needed the same thing within a few weeks of each other:
// internal/console, internal/cloud/store and internal/cloud/server. The copies
// would drift in the way that matters — one grows a grant the others lack, and
// the difference shows up as a permission error in whichever fixture was not
// updated. The console keeps its own for now because its grants are commented
// against its own schema; new callers should use this.
func IsolatedRoleDSN(t *testing.T, dbDSN, dbName, role string) string {
	t.Helper()

	const password = "probe"
	Do(t, dbDSN, func(conn *pgx.Conn) error {
		ctx := context.Background()
		// Dropped first: a previous run in the same cluster may have left it,
		// and CREATE ROLE is not idempotent.
		_, _ = conn.Exec(ctx, `DROP OWNED BY `+pgx.Identifier{role}.Sanitize())
		_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
		for _, sql := range []string{
			fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
				pgx.Identifier{role}.Sanitize(), password),
			// CREATE on the database because the caller runs its own migrations
			// as this role, and therefore owns what they create.
			fmt.Sprintf(`GRANT CREATE, CONNECT ON DATABASE %s TO %s`,
				pgx.Identifier{dbName}.Sanitize(), pgx.Identifier{role}.Sanitize()),
			// internal/migrate pins search_path=public for its history table,
			// so the role has to be able to write there too.
			fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`,
				pgx.Identifier{role}.Sanitize()),
		} {
			if _, err := conn.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		return nil
	})
	t.Cleanup(func() {
		Exec(t, dbDSN,
			`DROP OWNED BY `+pgx.Identifier{role}.Sanitize(),
			`DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
	})

	u, err := url.Parse(dbDSN)
	if err != nil {
		t.Fatalf("parse %q: %v", dbDSN, err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}
