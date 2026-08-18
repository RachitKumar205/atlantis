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
// # Why this is a type rather than an argument
//
// One console process serves many organisations, so `console.audit_log` holds
// several organisations' rows and every query against it has to be scoped. The
// obvious implementation is an `org string` parameter on each method, and it
// fails in the way scoping always fails: the compiler cannot tell the
// difference between the right value, the wrong one, and one somebody forgot to
// thread through. `deleteAllSessions` was `DELETE FROM console.sessions` with no
// WHERE for exactly that reason — nobody decided it should cross organisations,
// it simply never occurred to anyone that it would.
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
// Fails closed on purpose. An empty bind would leave console.current_org()
// returning NULL, under which the RESTRICTIVE policy admits nothing — so the
// caller would see an empty audit log rather than an error, and "the page is
// blank" is a much harder thing to diagnose than a refusal.
var ErrNoOrg = errors.New("no organisation bound")

// bindTimeout bounds the commit and rollback below, which deliberately do not
// use the request context.
const bindTimeout = 5 * time.Second

// tx runs fn inside a transaction with the organisation bound.
//
// # Why a transaction is required, not merely convenient
//
// console.set_org uses set_config(..., true), which is transaction-local. That
// is what makes it safe on a pooled connection: the value reverts when the
// transaction ends, so the next request to borrow the same backend cannot
// inherit it. A plain SET on the pool would persist to whatever unrelated
// request came next, which is the whole hazard.
//
// Before this, every console query ran directly on the pool and nothing here
// opened a transaction at all.
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

// newPoolConfig builds the console pool's configuration.
//
// # The AfterConnect hook
//
// Every new physical connection has its organisation discriminator cleared.
// atlantis's own binds are transaction-local and revert by themselves, so this
// is not about them — it covers a value arriving from outside this process: a
// server-level or role-level default, a SET in the connection string's
// `options`, or a connection pooler handing back somebody else's backend. Any
// of those would pre-bind an organisation, and the first query that forgot to
// bind would read that organisation's rows instead of nothing.
//
// SET to the empty string, deliberately NOT RESET. RESET restores the
// parameter's session default — and an `ALTER ROLE ... SET` *is* that default,
// so RESET would restore precisely the value it is meant to clear. The
// server-side version was written as RESET first and a test caught it. The
// empty string reads back as NULL through console.current_org(), because of the
// nullif in its body.
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
