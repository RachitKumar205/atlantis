package console

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orgStore is a store bound to one organisation.
//
// One console process serves many organisations, so console.audit_log holds
// several organisations' rows and every query against it must be scoped. A type
// rather than an `org string` parameter: the compiler cannot distinguish a
// correct argument from a wrong one or a missing one, and an unscoped
// `DELETE FROM console.sessions` compiles.
//
// Methods that touch organisation-scoped data hang off this type and read the
// organisation from the handle. A handler that has not said which organisation
// it is acting for cannot call them at all, and that is a compile error rather
// than a support ticket.
//
// The row-level-security policy is the actual boundary; this is what makes the
// boundary reachable without every call site remembering.
type orgStore struct {
	db  *store
	org string
}

// forOrg returns a handle scoped to org.
//
// Handlers build one from the *User already on the request context, whose Org
// came from an assertion Cloud signed and this console verified — so it is not
// a value any request can choose for itself.
func (s *store) forOrg(org string) *orgStore { return &orgStore{db: s, org: org} }

// ErrNoOrg reports an attempt to bind an empty organisation.
//
// Fails closed. An empty bind leaves console.current_org() returning NULL,
// under which the RESTRICTIVE policy admits nothing, so the caller reads an
// empty audit log rather than an error.
var ErrNoOrg = errors.New("no organisation bound")

// bindTimeout bounds the commit and rollback below, which do not use the
// request context.
const bindTimeout = 5 * time.Second

// tx runs fn inside a transaction with the organisation bound.
//
// A transaction is required. console.set_org uses set_config(..., true), which
// is transaction-local, so the value reverts when the transaction ends and the
// next request to borrow the same backend cannot inherit it. A plain SET on the
// pool would persist to an unrelated request.
func (o *orgStore) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if o.org == "" {
		return ErrNoOrg
	}

	tx, err := o.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on a fresh context, not the request's.
		//
		// pgx calls conn.die() when a rollback Exec fails, so rolling back on
		// a cancelled context destroys the pooled connection. The server-side
		// equivalent measured this: 30 of 30 reads failed and 29 new
		// connections were opened. A cancelled request is the common case —
		// somebody closed a tab — so this path is not exotic.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The bind must be the first statement in the transaction. Anything issued
	// ahead of it runs unscoped, and under the RESTRICTIVE policy that means
	// reading nothing — which looks like missing data rather than a bug.
	if _, err := tx.Exec(ctx, `SELECT console.set_org($1)`, o.org); err != nil {
		return fmt.Errorf("bind organisation: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindTimeout)
	defer cancel()
	if err := tx.Commit(cctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// listOrgNames returns every registered organisation, ordered.
//
// The list is read and the cursor closed before anything acts on it: a caller
// holding it open across per-organisation work holds a connection for the
// length of the whole sweep.
//
// console.orgs carries no policy, which is what makes this readable with no
// organisation bound.
func (s *store) listOrgNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT org FROM console.orgs ORDER BY org`)
	if err != nil {
		return nil, fmt.Errorf("list organisations: %w", err)
	}
	defer rows.Close()

	var orgs []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, fmt.Errorf("scan organisation: %w", err)
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list organisations: %w", err)
	}
	return orgs, nil
}

// eachOrg runs fn once per registered organisation, bound to it.
//
// How housekeeping crosses a row-level-security boundary. A
// `DELETE FROM console.enroll_tokens WHERE expires_at < now()` issued on the
// pool binds no organisation, so current_org() is NULL, `org = NULL` is NULL,
// and the RESTRICTIVE policy admits no row. The statement reports 0 rows
// affected and no error. Measured: one expired token seeded, swept, still
// there.
//
// A DELETE-specific policy keyed on expiry does not reach it either, which is
// worth knowing before writing one: DELETE reads the rows it operates on, and
// that read is governed by the SELECT policy.
//
// console.orgs carries no policy, which is what makes the list readable here,
// and console.enroll_tokens and console.schema_imports reference it ON DELETE
// CASCADE — so no row can name an organisation this loop does not visit.
//
// Every organisation is attempted before any error returns, so one failing does
// not stop the rest being swept.
func (s *store) eachOrg(ctx context.Context, fn func(*orgStore) error) error {
	orgs, err := s.listOrgNames(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, org := range orgs {
		if err := fn(s.forOrg(org)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", org, err))
		}
	}
	return errors.Join(errs...)
}

// newPoolConfig builds the console pool's configuration.
//
// AfterConnect clears the organisation discriminator on every new physical
// connection. The binds this package makes are transaction-local and revert on
// their own; this covers a value arriving from outside the process — a
// server-level or role-level default, a SET in the connection string's
// `options`, or a pooler returning another session's backend. Any of those
// pre-binds an organisation, and the first query that failed to bind would read
// that organisation's rows rather than none.
//
// SET to the empty string, not RESET. RESET restores the parameter's session
// default, and an `ALTER ROLE ... SET` is that default, so RESET restores
// precisely the value it is meant to clear. The empty string reads back as NULL
// through console.current_org(), because of the nullif in its body.
func newPoolConfig(pgURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, `SET "console.org" = ''`); err != nil {
			return fmt.Errorf("clear organisation discriminator: %w", err)
		}
		return nil
	}
	return cfg, nil
}
