package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// ErrOrgLimitReached reports an account that already has as many organisations
// as it may create.
//
// A distinct sentinel: the input is not what is wrong, so the route answers it
// differently from every other refusal.
var ErrOrgLimitReached = errors.New("organisation limit reached")

// OrgSummary is one organisation as its member sees it.
//
// It does not carry LastError, which is written for an operator and holds image
// references, cluster hostnames and API paths; one produced locally contained
// the whole Kubernetes API server URL. `cloud org status` reads it.
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
	// what the limit counts. False for the organisations that predate
	// created_by.
	//
	// Deletion is not gated on this. created_by is nullable under ON DELETE SET
	// NULL, so an organisation whose creator closed their account would be
	// undeletable; SoftDeleteOrg checks for an admin membership.
	CreatedByMe bool

	// PurgeAfter is when a deleted organisation stops being restorable. Nil in
	// every other state.
	//
	// Surfaced so the screen can render the date rather than compute it: the
	// window is a property of the row.
	PurgeAfter *time.Time
}

// CreateOrgForOwner creates an organisation under an unclaimed name. This is
// the browser-reachable path; CreateOrgWithOwner upserts, so posting a name
// already in use through it grants admin on the existing organisation.
//
// Returns ErrAlreadyExists for a name in use, ErrOrgLimitReached when the
// account is at its limit, and refuses an invalid or reserved name before it
// reaches the database.
//
// The transaction locks the account's row and reads the limit in the same
// statement, which serialises creates for that account. Counting and then
// inserting bounds nothing under READ COMMITTED, which this pool uses: two
// concurrent creates both count n-1 and both pass. No constraint expresses "at
// most N rows per user".
//
// FOR NO KEY UPDATE rather than FOR UPDATE. cloud.memberships, cloud.sessions
// and cloud.identities all reference cloud.users, and inserts into them take
// FOR KEY SHARE on the parent row, which FOR UPDATE conflicts with; that would
// block every concurrent sign-in for this account until the create finished.
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

// OrgsForUser lists what this account can see, with enough to render it:
// display name, provisioning state, and whether there is a console to send
// anybody to. MembershipsOf returns org and role only.
//
// LEFT JOIN on org_provisioning: an organisation registered by hand has no
// queue row and would otherwise drop out of its members' lists.
func (s *Store) OrgsForUser(ctx context.Context, userID string) ([]OrgSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.name,
		       o.display_name,
		       m.role,
		       COALESCE(p.state, ''),
		       COALESCE(p.attempts, 0),
		       o.console_url,
		       (o.created_by IS NOT NULL AND o.created_by = $1),
		       p.purge_after
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
			&o.Attempts, &o.ConsoleURL, &o.CreatedByMe, &o.PurgeAfter); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrgForUser reads one organisation this account belongs to.
//
// ErrNotFound when the organisation does not exist or the account is not a
// member. The same error for both: distinguishing them would enumerate
// organisations.
func (s *Store) OrgForUser(ctx context.Context, userID, org string) (*OrgSummary, error) {
	var o OrgSummary
	err := s.pool.QueryRow(ctx, `
		SELECT o.name, o.display_name, m.role,
		       COALESCE(p.state, ''), COALESCE(p.attempts, 0), o.console_url,
		       (o.created_by IS NOT NULL AND o.created_by = $1),
		       p.purge_after
		  FROM cloud.memberships m
		  JOIN cloud.orgs o ON o.name = m.org
		  LEFT JOIN cloud.org_provisioning p ON p.org = o.name
		 WHERE m.user_id = $1 AND m.org = $2
	`, userID, org).Scan(&o.Name, &o.DisplayName, &o.Role, &o.State,
		&o.Attempts, &o.ConsoleURL, &o.CreatedByMe, &o.PurgeAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// OrgLimitFor reports how many organisations this account may create. For a
// screen deciding whether to offer a create form; the create path reads the
// same column under a lock.
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
