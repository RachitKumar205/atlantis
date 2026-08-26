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
// Row-level security is a boundary only for a role subject to it. FORCE ROW
// LEVEL SECURITY binds a table's owner and binds neither a superuser nor a role
// holding BYPASSRLS, so a fixture connecting as the cluster's administrative
// role runs every case with the policies attached and inert.
//
// The caller migrates as this role too: creating the tables is what makes it
// their owner.
//
// Shared by internal/cloud/store, internal/cloud/server and cmd/cloud. Separate
// copies drift — one grows a grant the others lack, and the difference surfaces
// as a permission error in whichever fixture was not updated. internal/console
// keeps its own, whose grants are written against its own schema.
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
