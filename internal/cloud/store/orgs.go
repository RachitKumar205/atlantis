package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// Creating an organisation from a browser, which is a different act from
// creating one at a terminal.
//
// `cloud org create` is run by somebody with a shell on the machine, and it is
// documented as safe to re-run — so CreateOrgWithOwner upserts, and re-running
// it for an organisation that exists is a no-op by design.
//
// Exposed as an HTTP route that anybody who signed up can reach, those same
// semantics are a privilege-escalation bug: post the name of somebody else's
// organisation and the memberships upsert makes you its admin, while the orgs
// upsert overwrites their display name. Nothing refuses, and the response says
// success.
//
// That is why this is a separate function and not a flag on the other one. A
// shared implementation with `upsert bool` is how the two behaviours get
// confused again by somebody who has not read this comment.

// ErrOrgLimitReached reports an account that already has as many organisations
// as it may create.
//
// A distinct sentinel because the route answers it differently from every other
// refusal: it is not the caller's input that is wrong, and telling them to
// choose another name would be useless advice.
var ErrOrgLimitReached = errors.New("organisation limit reached")

// OrgSummary is one organisation as its member sees it.
//
// Deliberately does not carry LastError. That column is written for an operator
// and holds image references, cluster hostnames and API paths — a real one from
// the local walkthrough contained the full Kubernetes API server URL. The
// screen gets a state and a count; the detail stays in `cloud org status`.
type OrgSummary struct {
	Name        string
	DisplayName string
	Role        identity.Role
	State       ProvisioningState
	Attempts    int

	// ConsoleURL is empty until the organisation is provisioned, which is what
	// /authorize reports as ErrNoConsole.
	ConsoleURL string

	// CreatedByMe is whether this account created the organisation, which is
	// what the limit counts and what deletion will be gated on. False for the
	// organisations that predate created_by.
	CreatedByMe bool
}

// CreateOrgForOwner creates an organisation nobody has taken.
//
// Returns ErrAlreadyExists for a name in use, ErrOrgLimitReached when the
// account is at its limit, and refuses an invalid or reserved name before it
// reaches the database.
//
// # Why the account's row is locked
//
// The limit is a count, and a count is not a constraint. Counting rows and then
// inserting inside one transaction bounds nothing under READ COMMITTED, which
// is what this pool uses: two concurrent creates both count n-1, both pass, and
// the account ends up one over. There is no constraint that expresses "at most
// N rows per user" to fall back on.
//
// So the transaction takes the account's row first and reads the limit from the
// same statement. That serialises creates for this account and nothing else.
//
// FOR NO KEY UPDATE rather than FOR UPDATE: cloud.memberships, cloud.sessions
// and cloud.identities all reference cloud.users, and every insert into them
// takes FOR KEY SHARE on the parent row. FOR UPDATE conflicts with that, so it
// would block every concurrent sign-in and membership change for this account
// until the create finished. FOR NO KEY UPDATE excludes another create and
// nothing else.
func (s *Store) CreateOrgForOwner(
	ctx context.Context, org, displayName, ownerUserID string, role identity.Role,
) error {
	if err := identity.ValidateOrgName(org); err != nil {
		return err
	}
	if ownerUserID == "" {
		return errors.New("an owner is required: an organisation with no member is a 403 to everybody")
	}
	if !role.Valid() {
		return fmt.Errorf("unknown role %q", role)
	}

	return s.tx(ctx, func(tx pgx.Tx) error {
		var limit int
		err := tx.QueryRow(ctx, `
			SELECT org_limit FROM cloud.users WHERE id = $1 FOR NO KEY UPDATE
		`, ownerUserID).Scan(&limit)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", ownerUserID, ErrNotFound)
		}
		if err != nil {
			return err
		}

		var owned int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM cloud.orgs WHERE created_by = $1
		`, ownerUserID).Scan(&owned); err != nil {
			return err
		}
		if owned >= limit {
			return fmt.Errorf("%d of %d: %w", owned, limit, ErrOrgLimitReached)
		}

		// No ON CONFLICT. A name in use must fail, and the unique violation on
		// the primary key is what says so.
		_, err = tx.Exec(ctx, `
			INSERT INTO cloud.orgs (name, display_name, created_by) VALUES ($1, $2, $3)
		`, org, displayName, ownerUserID)
		if isUniqueViolation(err) {
			return fmt.Errorf("%s: %w", org, ErrAlreadyExists)
		}
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, $2, $3)
		`, ownerUserID, org, string(role)); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO cloud.org_provisioning (org) VALUES ($1)
		`, org)
		return err
	})
}

// OrgsForUser lists what this account can see, with enough to render it.
//
// MembershipsOf returns org and role only, which is all /authorize needed. A
// screen needs the display name, how far provisioning has got, and whether
// there is a console to send anybody to — three tables.
//
// LEFT JOIN on org_provisioning because the two organisations registered by
// hand have no queue row and never will; they are provisioned, just not by the
// provisioner. They report an empty state rather than being dropped from
// somebody's list.
func (s *Store) OrgsForUser(ctx context.Context, userID string) ([]OrgSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.name,
		       o.display_name,
		       m.role,
		       COALESCE(p.state, ''),
		       COALESCE(p.attempts, 0),
		       o.console_url,
		       (o.created_by IS NOT NULL AND o.created_by = $1)
		  FROM cloud.memberships m
		  JOIN cloud.orgs o ON o.name = m.org
		  LEFT JOIN cloud.org_provisioning p ON p.org = o.name
		 WHERE m.user_id = $1
		 ORDER BY o.name
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OrgSummary
	for rows.Next() {
		var o OrgSummary
		if err := rows.Scan(&o.Name, &o.DisplayName, &o.Role, &o.State,
			&o.Attempts, &o.ConsoleURL, &o.CreatedByMe); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrgForUser reads one organisation this account belongs to.
//
// ErrNotFound when the organisation does not exist *or* the account is not a
// member, and deliberately the same error for both: a route that distinguished
// them would report which organisations exist to anybody who asked.
func (s *Store) OrgForUser(ctx context.Context, userID, org string) (*OrgSummary, error) {
	var o OrgSummary
	err := s.pool.QueryRow(ctx, `
		SELECT o.name, o.display_name, m.role,
		       COALESCE(p.state, ''), COALESCE(p.attempts, 0), o.console_url,
		       (o.created_by IS NOT NULL AND o.created_by = $1)
		  FROM cloud.memberships m
		  JOIN cloud.orgs o ON o.name = m.org
		  LEFT JOIN cloud.org_provisioning p ON p.org = o.name
		 WHERE m.user_id = $1 AND m.org = $2
	`, userID, org).Scan(&o.Name, &o.DisplayName, &o.Role, &o.State,
		&o.Attempts, &o.ConsoleURL, &o.CreatedByMe)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// OrgLimitFor reports how many organisations this account may create.
//
// Read separately from the create path, which reads it under a lock in the same
// statement. This one is for a screen deciding whether to offer a create form
// at all, rather than offering one that refuses every time.
func (s *Store) OrgLimitFor(ctx context.Context, userID string) (int, error) {
	var limit int
	err := s.pool.QueryRow(ctx,
		`SELECT org_limit FROM cloud.users WHERE id = $1`, userID).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%s: %w", userID, ErrNotFound)
	}
	if err != nil {
		return 0, err
	}
	return limit, nil
}
