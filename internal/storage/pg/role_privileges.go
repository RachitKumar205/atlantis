package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// RolePrivileges describes the two attributes of the connecting role that
// decide whether row-level security means anything.
type RolePrivileges struct {
	Name string

	// Superuser bypasses every permission check in PostgreSQL, including RLS.
	Superuser bool

	// BypassRLS skips row-level security on every table, whether or not the
	// table has FORCE ROW LEVEL SECURITY set.
	BypassRLS bool
}

// ErrRoleBypassesRLS is returned when the connecting role can see through
// row-level security.
var ErrRoleBypassesRLS = errors.New("pg: the connecting role bypasses row-level security")

// DetectRolePrivileges reports whether the connected role can bypass RLS.
//
// This exists because `partition by` delegates tenant isolation to PostgreSQL
// rather than to injected predicates — precisely so that a forgotten call site
// cannot leak, and so that caller-authored SQL (custom query bodies, CHECK
// expressions, backfill expressions) is covered by the same guarantee as
// generated reads. That delegation is void if the role atlantis connects as is
// a superuser or holds BYPASSRLS: the policies are still attached to the
// tables, still visible in the catalog, and still completely inert.
//
// It is worth being precise about how this fails, because the failure looks
// like success. FORCE ROW LEVEL SECURITY closes the ordinary owner exemption,
// so a reasonable person checks for FORCE, finds it, and concludes isolation
// holds. FORCE does not close BYPASSRLS, and it does not close superuser. The
// tables are configured correctly and every read still returns every tenant's
// rows.
//
// Reports rather than refuses; refusing is the caller's decision, in the same
// shape as DetectTimescaleEdition and RequireIsolatedRole below.
func DetectRolePrivileges(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) runtime.Row
}) (RolePrivileges, error) {
	var p RolePrivileges
	err := q.QueryRow(ctx, `
SELECT rolname, rolsuper, rolbypassrls
  FROM pg_roles
 WHERE rolname = current_user`).Scan(&p.Name, &p.Superuser, &p.BypassRLS)
	if err != nil {
		return RolePrivileges{}, fmt.Errorf("pg: read role privileges: %w", err)
	}
	return p, nil
}

// CanEnforceRLS reports whether row-level security applies to this role.
func (p RolePrivileges) CanEnforceRLS() bool {
	return !p.Superuser && !p.BypassRLS
}

// RequireIsolatedRole returns an error unless row-level security actually
// applies to the connecting role.
//
// The error names which attribute is at fault, because the remedy differs: a
// superuser needs a different role entirely, while BYPASSRLS is one ALTER ROLE
// away. Both are deployment problems rather than code problems, so the message
// is written for whoever provisioned the database.
func RequireIsolatedRole(p RolePrivileges) error {
	switch {
	case p.Superuser:
		return fmt.Errorf("%w: role %q is a superuser, and superusers are exempt "+
			"from row-level security even with FORCE ROW LEVEL SECURITY set. Every "+
			"`partition by` policy is attached and inert, so reads return every "+
			"tenant's rows. Connect as a non-superuser role that owns no tenant "+
			"tables", ErrRoleBypassesRLS, p.Name)
	case p.BypassRLS:
		return fmt.Errorf("%w: role %q holds BYPASSRLS, which skips row-level "+
			"security on every table regardless of FORCE. Every `partition by` "+
			"policy is attached and inert. Fix with: ALTER ROLE %q NOBYPASSRLS",
			ErrRoleBypassesRLS, p.Name, p.Name)
	default:
		return nil
	}
}

// PgxRoleQuerier adapts a raw pgx handle — a pool, a connection, or a
// transaction — to the row shape DetectRolePrivileges takes.
//
// Needed because the two differ only in the NAME of the returned interface:
// pgx returns pgx.Row and the runtime returns runtime.Row, both of which are
// just Scan. Go requires the method signatures to match exactly, so a one-line
// adapter is the whole cost of accepting both.
//
// It matters for testing in particular: the role privileges that decide
// whether RLS applies can differ from the pool's role once SET ROLE is in
// play, and checking that requires asking on a specific transaction.
type PgxRoleQuerier struct {
	Q interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

// QueryRow satisfies the interface DetectRolePrivileges expects.
func (a PgxRoleQuerier) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return a.Q.QueryRow(ctx, sql, args...)
}
