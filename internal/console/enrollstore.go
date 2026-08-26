package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
// One statement decides redeemability: unused, unexpired, and belonging to the
// bound organisation. Reading and then updating would leave a window in which
// two requests both see the token unused.
//
// The organisation is not a WHERE clause. The transaction is bound and the
// RESTRICTIVE policy compares it, which also makes a token from another
// organisation indistinguishable from one that never existed.
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
// The lookup renewal is built on, and the reason console.caller_certs carries
// no row-level security: it runs before anything knows which organisation is
// involved. Not a search — the key is a SHA-256 over a certificate nobody else
// holds, so the only row reachable is the one for a certificate already held.
//
// Superseded rows are accepted, with no overlap window. A certificate presented
// here has already passed mTLS on the enrolment listener, so it chains to the
// organisation's authority and has not expired; a newer certificate issued for
// the same caller does not make this one invalid for the seven days it lives.
// Refusing it would lock out a machine whose renewal response was lost.
//
// Nor does accepting it grant anything. Renewal from a leaked certificate
// produces another certificate for a caller the holder could already act as,
// and the answer to that is RevokeCaller, which deletes the identity row and
// takes effect within five seconds.
//
// What is still refused is a certificate this console never issued: there is no
// row, so there is no organisation to resolve, and the honest answer is "enrol".
func (s *store) callerCertByFingerprint(ctx context.Context, fingerprint []byte) (*callerCert, error) {
	c := callerCert{Fingerprint: fingerprint}
	err := s.pool.QueryRow(ctx, `
		SELECT org, caller, issued_at, expires_at
		  FROM console.caller_certs
		 WHERE fingerprint = $1
	`, fingerprint).
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
