package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
)

// The storage half of enrolment. See migrations/console/0007 for why the token
// is stored as a hash and why caller_certs carries no row-level security.

// enrollTokenTTL is how long a minted token stays redeemable.
//
// Fifteen minutes, on the same reasoning as sudoTTL: the token is carried from
// a console session to a terminal by a human who is already at the keyboard.
// Long enough to paste into a shell on another machine, short enough that one
// left in scrollback is worthless by the time anybody finds it.
const enrollTokenTTL = 15 * time.Minute

// ErrEnrollTokenUnusable reports a token that is not redeemable.
//
// One error for four distinct causes — never existed, wrong organisation,
// already spent, expired — because telling them apart would let anyone holding
// a guess learn which half was wrong. The audit trail records what actually
// happened; the caller is told it did not work.
var ErrEnrollTokenUnusable = errors.New("enrolment token is not usable")

// enrollToken is a minted token and the row that backs it.
//
// Secret is the only time the token exists in a readable form anywhere. It is
// returned to the admin who minted it and then forgotten: the row holds the
// hash, so nothing can read it back afterwards.
type enrollToken struct {
	Secret    string
	Caller    string
	ExpiresAt time.Time
}

// hashEnrollToken is the one place a token becomes a lookup key.
//
// Defined once because a mint that hashed differently from a redemption would
// produce tokens that are refused with no error anywhere — the row exists, the
// key does not match, and the answer is "unknown token".
func hashEnrollToken(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// createEnrollToken mints one for a caller in this organisation.
//
// 32 bytes from crypto/rand, the same width as a session token. The value
// travels to a machine over whatever channel the operator chooses, so it is
// sized to be worth nothing to guess rather than to be short.
func (o *orgStore) createEnrollToken(ctx context.Context, caller, createdBy string) (*enrollToken, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().Add(enrollTokenTTL)

	err := o.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO console.enroll_tokens
			    (token_sha256, org, caller, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5)
		`, hashEnrollToken(secret), o.org, caller, createdBy, expires)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &enrollToken{Secret: secret, Caller: caller, ExpiresAt: expires}, nil
}

// spendEnrollToken claims a token and returns the caller it was minted for.
//
// # Everything that decides redeemability is in this one statement
//
// Unused, unexpired, and belonging to the bound organisation. Reading the row
// and then updating it would leave a window where two requests both see it as
// unused — the exact race single-use exists to close — and it would also invite
// the expiry check to drift somewhere else and stop being consulted.
//
// The organisation is not a predicate here because it does not need to be: the
// transaction is bound, and the RESTRICTIVE policy on the table compares it.
// That is the boundary doing its job rather than a WHERE clause somebody could
// forget. It also means a token from another organisation is indistinguishable
// from one that never existed, which is the right answer to give.
//
// A zero-row UPDATE surfaces as pgx.ErrNoRows on Scan rather than as a silent
// success, which is what makes this shape safe to rely on.
//
// The caller comes back from the row and is what the CSR is checked against.
// Taking it from the request instead would make the check tautological — and
// the signer's own CN check compares the CSR against the name it was handed, so
// it would not catch it either.
func (o *orgStore) spendEnrollToken(ctx context.Context, secret string) (caller string, err error) {
	err = o.tx(ctx, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx, `
			UPDATE console.enroll_tokens
			   SET used_at = NOW()
			 WHERE token_sha256 = $1
			   AND used_at IS NULL
			   AND expires_at > NOW()
			RETURNING caller
		`, hashEnrollToken(secret)).Scan(&caller)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return ErrEnrollTokenUnusable
		}
		return scanErr
	})
	if err != nil {
		return "", err
	}
	return caller, nil
}

// deleteExpiredEnrollTokens is housekeeping, and only housekeeping.
//
// An expired token stops working because spendEnrollToken says so, not because
// this ran. Worth stating plainly: the obvious way to test "an expired token is
// refused" is to insert a stale row, run this, and assert it is gone — which
// proves the sweep and says nothing at all about the refusal, on a build where
// the refusal does not happen.
//
// Runs on the pool rather than through a bound transaction, because it is not
// scoped to an organisation: every expired row goes, whoever minted it. The
// same reasoning as deleteSpentAssertions.
func (s *store) deleteExpiredEnrollTokens(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM console.enroll_tokens WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// callerCert is one issued certificate as this console recorded it.
type callerCert struct {
	Fingerprint  []byte
	Org          string
	Caller       string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	SupersededAt *time.Time
}

// recordCallerCert writes the fingerprint→(org, caller) mapping and marks any
// earlier certificate for the same caller superseded.
//
// Both in one transaction. A recorded certificate with the previous one still
// live would mean two rows claiming to be current for one caller, and renewal
// picks its answer from exactly this table — so the ambiguity would show up
// later as a renewal that resolves to whichever row was returned first.
//
// Runs on the pool with an explicit org column rather than through a bound
// transaction, because console.caller_certs carries no policy; see 0007 for
// why. The org here comes from the spent token's row, never from a request.
func (s *store) recordCallerCert(ctx context.Context, c callerCert) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `
		UPDATE console.caller_certs
		   SET superseded_at = NOW()
		 WHERE org = $1 AND caller = $2 AND superseded_at IS NULL
	`, c.Org, c.Caller); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO console.caller_certs
		    (fingerprint, org, caller, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fingerprint) DO NOTHING
	`, c.Fingerprint, c.Org, c.Caller, c.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// listCallerCerts returns the live certificate per caller in this organisation.
//
// Scoped in Go rather than by a policy, because console.caller_certs carries
// none — see 0007. The organisation comes from the orgStore handle, which a
// handler can only get from a session, so there is no call site that can ask
// for another organisation's rows without saying so.
func (o *orgStore) listCallerCerts(ctx context.Context) ([]callerCert, error) {
	rows, err := o.db.pool.Query(ctx, `
		SELECT fingerprint, caller, issued_at, expires_at
		  FROM console.caller_certs
		 WHERE org = $1 AND superseded_at IS NULL
		 ORDER BY caller
	`, o.org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []callerCert
	for rows.Next() {
		c := callerCert{Org: o.org}
		if err := rows.Scan(&c.Fingerprint, &c.Caller, &c.IssuedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// callerCertByFingerprint answers "whose certificate is this".
//
// The lookup renewal is built on, and the reason console.caller_certs carries no
// row-level security: it runs before anything knows which organisation is
// involved, which is the entire point of it. Not a search — the key is a
// SHA-256 over a certificate nobody else holds, so the only row anybody can
// find is the one for a certificate they already have.
//
// # A superseded certificate can still renew, inside its window
//
// This looked wrong on the first attempt and the test caught it. Excluding
// superseded rows is the obvious reading — "you have been replaced, renew from
// the replacement" — and it destroys the exact recovery the overlap exists for.
//
// Walk it through. A machine holding certificate A renews, the console records
// B and marks A superseded, and the response is lost. The machine still holds A
// and nothing else. If A cannot renew, it cannot get anything, ever: that is
// the lockout, reintroduced one layer up from where it was removed.
//
// So the window is the same on both sides. atlantis accepts A for
// RenewalOverlap after B replaces it, and so does this. Anything else leaves a
// band in which a machine can authenticate but cannot renew — which is a
// lockout that only appears for certificates of one particular age.
//
// What is still refused is a certificate replaced longer ago than the window,
// and one this console never issued.
func (s *store) callerCertByFingerprint(ctx context.Context, fingerprint []byte) (*callerCert, error) {
	c := callerCert{Fingerprint: fingerprint}
	err := s.pool.QueryRow(ctx, `
		SELECT org, caller, issued_at, expires_at
		  FROM console.caller_certs
		 WHERE fingerprint = $1
		   AND (superseded_at IS NULL OR superseded_at > NOW() - $2::interval)
	`, fingerprint, interceptors.RenewalOverlap.String()).
		Scan(&c.Org, &c.Caller, &c.IssuedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// currentCallerCert returns the live certificate this console issued for a
// caller, or ErrNotFound.
//
// What the Callers page reads to say whether a caller is bound, and to which
// certificate. A caller with no row here has never been enrolled through this
// console — which today is every caller everywhere, because the handler that
// would have written one has never run in any deployment.
func (o *orgStore) currentCallerCert(ctx context.Context, caller string) (*callerCert, error) {
	c := callerCert{Org: o.org, Caller: caller}
	err := o.db.pool.QueryRow(ctx, `
		SELECT fingerprint, issued_at, expires_at
		  FROM console.caller_certs
		 WHERE org = $1 AND caller = $2 AND superseded_at IS NULL
		 ORDER BY issued_at DESC
		 LIMIT 1
	`, o.org, caller).Scan(&c.Fingerprint, &c.IssuedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}
