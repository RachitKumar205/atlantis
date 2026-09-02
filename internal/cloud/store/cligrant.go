package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A CLI grant is one `tide login` in flight. The CLI holds the device code
// and polls with it; the person types the user code into the approval page.
// See migrations/cloud/0013.

// CLIGrantTTL bounds how long a started login can be finished.
const CLIGrantTTL = 15 * time.Minute

// cliUserCodeAlphabet has no vowels and no 0/O/1/I/5/S confusion. The code is
// read off one screen and typed into another.
const cliUserCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ2346789"

// cliGrantMaxAttempts is how many failed approval lookups a pending grant
// survives. The user code carries about 38 bits; a budget this small makes
// guessing one from a signed-in account a non-event.
const cliGrantMaxAttempts = 5

// ErrCLIGrantUnusable covers every way a grant can fail to be acted on:
// unknown, expired, already decided, attempts exhausted. One error, so a
// probe learns nothing about which.
var ErrCLIGrantUnusable = errors.New("that code is not waiting for approval")

// CLIGrant is a row as the approval page needs to see it.
type CLIGrant struct {
	Caller     string
	ClientMeta map[string]string
	Status     string
	Org        string
	ExpiresAt  time.Time
}

// CLIGrantDecision is what a consumed poll hands back to be minted from.
type CLIGrantDecision struct {
	Status string // approved | denied | expired | pending
	Org    string
	UserID string
	Caller string
}

// CreateCLIGrant opens a login attempt and returns the two codes.
//
// The device code goes back to the CLI and is stored only as a hash; the user
// code is display material. A user-code collision with another pending grant
// retries with a fresh code rather than failing the login.
func (s *Store) CreateCLIGrant(ctx context.Context, caller string, meta map[string]string) (deviceCode, userCode string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	deviceCode = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(deviceCode))

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", "", err
	}

	for range 5 {
		userCode, err = newCLIUserCode()
		if err != nil {
			return "", "", err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO cloud.cli_grants (code_sha256, user_code, caller, client_meta, expires_at)
			VALUES ($1, $2, $3, $4, NOW() + $5::interval)`,
			sum[:], userCode, caller, metaJSON, CLIGrantTTL.String())
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return deviceCode, userCode, nil
	}
	return "", "", errors.New("could not allocate a user code")
}

func newCLIUserCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, r := range raw {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(cliUserCodeAlphabet[int(r)%len(cliUserCodeAlphabet)])
	}
	return b.String(), nil
}

// CLIGrantByUserCode is the approval page's lookup, by what the person typed.
//
// Every miss against a pending grant costs that grant an attempt, so the code
// cannot be searched for from a signed-in account. The charge lands on the
// grant, not the prober: a burned grant costs the CLI user one re-run.
func (s *Store) CLIGrantByUserCode(ctx context.Context, userCode string) (*CLIGrant, error) {
	userCode = strings.ToUpper(strings.TrimSpace(userCode))
	var (
		g        CLIGrant
		metaJSON []byte
		attempts int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT caller, client_meta, status, COALESCE(org, ''), expires_at, attempts
		FROM cloud.cli_grants
		WHERE user_code = $1 AND status = 'pending' AND expires_at > NOW()`,
		userCode).Scan(&g.Caller, &metaJSON, &g.Status, &g.Org, &g.ExpiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCLIGrantUnusable
	}
	if err != nil {
		return nil, err
	}
	if attempts >= cliGrantMaxAttempts {
		return nil, ErrCLIGrantUnusable
	}
	if err := json.Unmarshal(metaJSON, &g.ClientMeta); err != nil {
		g.ClientMeta = nil
	}
	return &g, nil
}

// ChargeCLIGrantAttempt counts one failed action against a pending grant.
func (s *Store) ChargeCLIGrantAttempt(ctx context.Context, userCode string) {
	_, _ = s.pool.Exec(ctx, `
		UPDATE cloud.cli_grants SET attempts = attempts + 1
		WHERE user_code = $1 AND status = 'pending'`,
		strings.ToUpper(strings.TrimSpace(userCode)))
}

// DecideCLIGrant records approval or denial. The decision is a predicate on
// the row's current state, as spending an enroll token is: a grant that is
// not pending, or has expired, or is out of attempts, cannot be decided.
func (s *Store) DecideCLIGrant(ctx context.Context, userCode, userID, org string, approve bool) error {
	status := "denied"
	if approve {
		status = "approved"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.cli_grants
		SET status = $2, user_id = $3, org = $4, approved_at = NOW()
		WHERE user_code = $1 AND status = 'pending'
		  AND expires_at > NOW() AND attempts < $5`,
		strings.ToUpper(strings.TrimSpace(userCode)), status, userID, org, cliGrantMaxAttempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrCLIGrantUnusable
	}
	return nil
}

// ConsumeCLIGrant is the poll's read. An approved grant transitions to
// consumed exactly once — the UPDATE is the spend — and the caller mints the
// assertion only on that transition, so nothing token-shaped ever rests in
// the table.
func (s *Store) ConsumeCLIGrant(ctx context.Context, deviceCode string) (*CLIGrantDecision, error) {
	sum := sha256.Sum256([]byte(deviceCode))

	var d CLIGrantDecision
	err := s.pool.QueryRow(ctx, `
		UPDATE cloud.cli_grants
		SET status = 'consumed', consumed_at = NOW()
		WHERE code_sha256 = $1 AND status = 'approved' AND expires_at > NOW()
		RETURNING COALESCE(org, ''), COALESCE(user_id, ''), caller`,
		sum[:]).Scan(&d.Org, &d.UserID, &d.Caller)
	if err == nil {
		d.Status = "approved"
		return &d, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Not approved. Report which non-state it is in, so the CLI can keep
	// polling, stop with a denial, or tell the person to start over.
	var status string
	var expires time.Time
	err = s.pool.QueryRow(ctx, `
		SELECT status, expires_at FROM cloud.cli_grants WHERE code_sha256 = $1`,
		sum[:]).Scan(&status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCLIGrantUnusable
	}
	if err != nil {
		return nil, err
	}
	if status == "pending" && time.Now().After(expires) {
		status = "expired"
	}
	if status == "consumed" {
		// A second poll after pickup is either a bug or a theft; either way
		// there is nothing safe to say beyond "start over".
		return nil, ErrCLIGrantUnusable
	}
	return &CLIGrantDecision{Status: status}, nil
}

// DeleteExpiredCLIGrants is housekeeping; expiry is enforced by the
// predicates above, as it is for enroll tokens.
func (s *Store) DeleteExpiredCLIGrants(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM cloud.cli_grants WHERE expires_at < NOW() - interval '1 day'`)
	return err
}

// SetEnrollURL records where an organisation's machines enrol; see
// migrations/cloud/0012.
func (s *Store) SetEnrollURL(ctx context.Context, org, enrollURL string) error {
	enrollURL = strings.TrimRight(strings.TrimSpace(enrollURL), "/")
	tag, err := s.pool.Exec(ctx,
		`UPDATE cloud.orgs SET enroll_url = $2 WHERE name = $1`, org, enrollURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// EnrollURL returns where an organisation's machines enrol. ErrNoEnrollURL
// when none is configured, so the poll can refuse to mint an assertion that
// could not be redeemed anywhere.
func (s *Store) EnrollURL(ctx context.Context, org string) (string, error) {
	var enrollURL string
	err := s.pool.QueryRow(ctx,
		`SELECT enroll_url FROM cloud.orgs WHERE name = $1`, org).Scan(&enrollURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if enrollURL == "" {
		return "", fmt.Errorf("%s: %w", org, ErrNoEnrollURL)
	}
	return enrollURL, nil
}

// ErrNoEnrollURL reports an organisation with no registered enrolment
// listener.
var ErrNoEnrollURL = errors.New("no enrolment address is registered")
