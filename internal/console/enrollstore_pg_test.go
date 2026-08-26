package console

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The enrolment token, at the statement that spends it.
//
// Everything deciding whether a token is redeemable lives in one UPDATE:
// unused, unexpired, and — through the RESTRICTIVE policy on the bound
// transaction — belonging to the right organisation. Each of these tests kills
// one of those three, and every one of them is silent when it breaks: the
// endpoint still returns a working certificate.

// expireEnrollToken backdates a token, standing in for the passage of time.
//
// Moving the row rather than the clock, because the alternative is a test that
// sleeps for the token's whole lifetime or one that reaches into the constant —
// and the constant is not what decides anything, the predicate is.
func (f *consoleFixture) expireEnrollToken(t *testing.T, secret string) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(),
		`UPDATE console.enroll_tokens SET expires_at = NOW() - INTERVAL '1 minute'
		  WHERE token_sha256 = $1`, hashEnrollToken(secret))
	if err != nil {
		t.Fatalf("backdate token: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate matched %d rows, so the test is not about the token it thinks",
			tag.RowsAffected())
	}
}

// usedAt reads the spend marker straight from the row.
func (f *consoleFixture) usedAt(t *testing.T, secret string) *time.Time {
	t.Helper()
	var used *time.Time
	err := f.pool.QueryRow(context.Background(),
		`SELECT used_at FROM console.enroll_tokens WHERE token_sha256 = $1`,
		hashEnrollToken(secret)).Scan(&used)
	if err != nil {
		t.Fatalf("read used_at: %v", err)
	}
	return used
}

func TestAnEnrolTokenIsSpentExactlyOnce(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	store := f.srv.db.forOrg(defaultOrg)

	tok, err := store.createEnrollToken(ctx, "backend", "usr_admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	caller, err := store.spendEnrollToken(ctx, tok.Secret)
	if err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if caller != "backend" {
		t.Fatalf("spend returned caller %q, want backend", caller)
	}

	if _, err := store.spendEnrollToken(ctx, tok.Secret); !errors.Is(err, ErrEnrollTokenUnusable) {
		t.Fatalf("a spent token was redeemable again: %v", err)
	}
}

// TestAnExpiredEnrolTokenIsRefusedWithoutBeingConsumed.
//
// Two assertions, and the second is the one that matters. "Refused" and
// "consumed, then refused" look identical from outside, and only one of them is
// the expiry predicate doing the work — the other is the single-use predicate
// covering for its absence, which stops covering the moment somebody redeems a
// stale token before anyone else touches it.
//
// The sweeper is not involved: expiry is enforced by the statement, not by
// housekeeping on a 24-hour tick. A test that inserted a stale row, ran the
// sweep and asserted the row was gone would prove the sweep and pass on a build
// where an hour-old fifteen-minute token still mints certificates.
func TestAnExpiredEnrolTokenIsRefusedWithoutBeingConsumed(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	store := f.srv.db.forOrg(defaultOrg)

	tok, err := store.createEnrollToken(ctx, "backend", "usr_admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	f.expireEnrollToken(t, tok.Secret)

	if _, err := store.spendEnrollToken(ctx, tok.Secret); !errors.Is(err, ErrEnrollTokenUnusable) {
		t.Fatalf("an expired token was redeemable: %v", err)
	}
	if used := f.usedAt(t, tok.Secret); used != nil {
		t.Errorf("the expired token was consumed before being refused (used_at=%v)", used)
	}
}

// TestAnEnrolTokenDoesNotCrossOrganisations.
//
// The redemption route has no session, so the organisation arrives in the
// request. It is not trusted — it is what gets BOUND, and the RESTRICTIVE
// policy compares it against the row. This is that comparison, at the layer
// where it happens.
//
// Both directions, because a policy that admitted nothing at all would pass the
// refusal half on its own while breaking every legitimate enrolment.
func TestAnEnrolTokenDoesNotCrossOrganisations(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	tok, err := f.srv.db.forOrg(defaultOrg).createEnrollToken(ctx, "backend", "usr_admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, err := f.srv.db.forOrg("globex").spendEnrollToken(ctx, tok.Secret); !errors.Is(err, ErrEnrollTokenUnusable) {
		t.Fatalf("a token minted for %s was redeemed by globex: %v", defaultOrg, err)
	}
	if used := f.usedAt(t, tok.Secret); used != nil {
		t.Errorf("the foreign redemption consumed the token (used_at=%v)", used)
	}

	// The control. Without it this test passes on a build where the policy
	// refuses everything, which is the other way this goes wrong.
	caller, err := f.srv.db.forOrg(defaultOrg).spendEnrollToken(ctx, tok.Secret)
	if err != nil {
		t.Fatalf("the token's own organisation could not redeem it: %v", err)
	}
	if caller != "backend" {
		t.Errorf("spend returned caller %q, want backend", caller)
	}
}

// TestAnUnboundRedemptionIsRefusedRatherThanAdmitted.
//
// The failure the policy makes possible, stated as a property. A handler that
// ran this on the bare pool would leave console.current_org() NULL, the
// restrictive policy would admit nothing, and every valid token would be
// refused — "enrolment is broken" rather than "a scoping mistake".
//
// orgStore.tx refuses an empty organisation before it reaches the database, so
// what this pins is that the refusal happens at all rather than the query
// running unbound.
func TestAnUnboundRedemptionIsRefusedRatherThanAdmitted(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	tok, err := f.srv.db.forOrg(defaultOrg).createEnrollToken(ctx, "backend", "usr_admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := f.srv.db.forOrg("").spendEnrollToken(ctx, tok.Secret); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("an unbound redemption was not refused as such: %v", err)
	}
	if used := f.usedAt(t, tok.Secret); used != nil {
		t.Errorf("the unbound redemption consumed the token (used_at=%v)", used)
	}
}

// TestTheTokenIsNotStoredInTheClear.
//
// The row is otherwise a bearer credential at rest: a backup, a logged query or
// a pg_dump would hand over every unredeemed enrolment. spent_assertions stores
// its jti in the clear and that is fine, because a jti identifies an assertion
// rather than authorising one — this authorises one.
func TestTheTokenIsNotStoredInTheClear(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	tok, err := f.srv.db.forOrg(defaultOrg).createEnrollToken(ctx, "backend", "usr_admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Nothing in the row equals the token, in any column.
	var found int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM console.enroll_tokens
		 WHERE encode(token_sha256, 'escape') = $1
		    OR org = $1 OR caller = $1 OR created_by = $1
	`, tok.Secret).Scan(&found); err != nil {
		t.Fatalf("search for the token: %v", err)
	}
	if found != 0 {
		t.Errorf("the token appears verbatim in %d row(s)", found)
	}

	// And the hash that is stored is the one the redemption computes.
	var stored []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT token_sha256 FROM console.enroll_tokens WHERE caller = 'backend'`).
		Scan(&stored); err != nil {
		t.Fatalf("read the stored hash: %v", err)
	}
	if string(stored) != string(hashEnrollToken(tok.Secret)) {
		t.Error("the stored hash is not what redemption looks up, so no token would ever redeem")
	}
}

// TestRecordingACertificateSupersedesTheOneBefore.
//
// Two rows claiming to be current for one caller would make renewal's lookup
// depend on which came back first, which is a bug that appears months later and
// only under a second enrolment.
func TestRecordingACertificateSupersedesTheOneBefore(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	first := callerCert{
		Fingerprint: []byte("0123456789abcdef0123456789abcdef"),
		Org:         defaultOrg, Caller: "backend",
		ExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}
	second := callerCert{
		Fingerprint: []byte("fedcba9876543210fedcba9876543210"),
		Org:         defaultOrg, Caller: "backend",
		ExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}
	for _, c := range []callerCert{first, second} {
		if err := f.srv.db.recordCallerCert(ctx, c); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	got, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(ctx, "backend")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got.Fingerprint) != string(second.Fingerprint) {
		t.Errorf("the current certificate is the first one, not the replacement")
	}

	var live int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM console.caller_certs
		 WHERE org = $1 AND caller = 'backend' AND superseded_at IS NULL
	`, defaultOrg).Scan(&live); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	if live != 1 {
		t.Errorf("%d certificates claim to be current for one caller", live)
	}
}

// A caller nobody has enrolled has no certificate on record, which is what the
// console shows as "not bound".
//
// Today that is every caller in every deployment: the handler that would have
// written a fingerprint answered 503 everywhere it ran.
func TestACallerWithNoEnrolmentHasNoCertificateOnRecord(t *testing.T) {
	f := newConsoleFixture(t)
	_, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(context.Background(), "never-enrolled")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unenrolled caller reported a certificate: %v", err)
	}
}
