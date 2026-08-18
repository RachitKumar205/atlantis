package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

// A token round-trips and resolves to its account.
func TestIssueAndSpendAToken(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "ada@example.com", "Ada", nil)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if token == "" {
		t.Fatal("issued an empty token")
	}

	spent, err := db.SpendEmailToken(ctx, token, PurposeVerifyEmail)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if spent.UserID != u.ID {
		t.Errorf("resolved to %s, want %s", spent.UserID, u.ID)
	}
}

// The token is not in the database — only a hash of it is.
//
// It travels in a URL, so it lands in browser history, in the referrer of
// anything the landing page loads, and in logs along the way. Storing it
// verbatim would make a read of this table a working reset for every account
// with one pending.
func TestTheTokenItselfIsNotStored(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "stored@example.com", "", nil)
	token, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	var stored []byte
	if err := db.pool.QueryRow(ctx,
		`SELECT token_hash FROM cloud.email_tokens WHERE user_id = $1`, u.ID).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(stored), token) {
		t.Fatal("the token is in the database verbatim")
	}

	// And it is the RIGHT hash — without this the test above passes against a
	// column of unrelated bytes, and the spend would then work for the wrong
	// reason or not at all.
	want := sha256.Sum256([]byte(token))
	if string(stored) != string(want[:]) {
		t.Error("the stored hash is not the SHA-256 of the token")
	}
}

// A token is single-use, and the second attempt is refused.
func TestATokenCannotBeSpentTwice(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "once@example.com", "", nil)
	token, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	_, err := db.SpendEmailToken(ctx, token, PurposeResetPassword)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a reset link worked twice: %v", err)
	}
}

// A token issued to verify an address cannot set a password.
//
// The two are issued under different conditions — one at sign-up to an address
// nobody has proven, the other on request to one already proven — so a token
// good for both would be only as strong as the weaker path that issued it.
func TestATokenIsBoundToItsPurpose(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "purpose@example.com", "", nil)
	verify, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)

	if _, err := db.SpendEmailToken(ctx, verify, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a verification token was accepted to reset a password: %v", err)
	}
	// And it still works for what it was issued for — without this the check
	// above passes against a spend that refuses everything.
	if _, err := db.SpendEmailToken(ctx, verify, PurposeVerifyEmail); err != nil {
		t.Errorf("the token no longer works for its own purpose: %v", err)
	}
}

// An expired token is refused.
func TestAnExpiredTokenIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "expired@example.com", "", nil)
	token, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an expired token was accepted: %v", err)
	}
}

// An unknown token is refused, and is indistinguishable from every other
// failure.
//
// One error for unknown, expired, spent and wrong-purpose. Telling them apart
// would let somebody probe for valid tokens, and the user's next step is the
// same in all four cases: ask for another link.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	db := newTestStore(t)

	_, err := db.SpendEmailToken(context.Background(), "not-a-real-token", PurposeResetPassword)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("got %v, want ErrTokenInvalid", err)
	}
}

// A token stops working when the account's address changes.
//
// Without the address check, somebody who controlled the mailbox when the reset
// was issued keeps a working reset after losing control of it — which is
// exactly the recovery an account takeover needs.
func TestATokenDiesWithTheAddressItWasSentTo(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "old@example.com", "", nil)
	token, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)

	if _, err := db.pool.Exec(ctx,
		`UPDATE cloud.users SET email = 'new@example.com' WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("change address: %v", err)
	}

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a reset issued to the old address still worked: %v", err)
	}
}

// Setting a password invalidates every other reset in flight.
//
// Requesting three resets otherwise leaves three live credentials, and using
// one leaves two — which matters precisely in the case a reset was requested
// because somebody else had access.
func TestSettingAPasswordInvalidatesOtherResets(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "many@example.com", "", nil)
	first, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)
	second, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)

	if _, err := db.SpendEmailToken(ctx, first, PurposeResetPassword); err != nil {
		t.Fatalf("spend the first: %v", err)
	}
	if err := db.InvalidateEmailTokens(ctx, u.ID, PurposeResetPassword); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	if _, err := db.SpendEmailToken(ctx, second, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a second outstanding reset link still worked: %v", err)
	}
}

// Invalidating one purpose leaves the other alone.
//
// Setting a password must not silently void a pending address verification —
// the user would follow that link and be told it is invalid, with no way to get
// another except by changing their address.
func TestInvalidationIsScopedToItsPurpose(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "scoped@example.com", "", nil)
	verify, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)

	if err := db.InvalidateEmailTokens(ctx, u.ID, PurposeResetPassword); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := db.SpendEmailToken(ctx, verify, PurposeVerifyEmail); err != nil {
		t.Errorf("invalidating resets also voided a pending verification: %v", err)
	}
}

// The sweep removes expired rows and leaves live ones.
func TestExpiredTokensAreSwept(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "sweep@example.com", "", nil)
	live, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)
	if _, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, -time.Minute); err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	n, err := db.DeleteExpiredEmailTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Asserted on the count, not just on absence of error. A sweep that deletes
	// nothing and reports success is a shape this repository has shipped
	// before — see the TTL sweeper in the CHANGELOG.
	if n != 1 {
		t.Errorf("swept %d rows, want 1", n)
	}
	if _, err := db.SpendEmailToken(ctx, live, PurposeVerifyEmail); err != nil {
		t.Errorf("the sweep took a live token: %v", err)
	}
}

// A password set and read back is the hash the caller supplied.
func TestSetPassword(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "pw@example.com", "", nil)
	if u.HasPassword() {
		t.Fatal("a new account already has a password")
	}

	const hash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA"
	if err := db.SetPassword(ctx, u.ID, hash); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := db.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.HasPassword() || *got.PasswordHash != hash {
		t.Errorf("password hash round-tripped as %v", got.PasswordHash)
	}

	if err := db.SetPassword(ctx, "usr_nosuch", hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("setting a password on a missing account got %v, want ErrNotFound", err)
	}
}

// Verifying an address is recorded once.
func TestMarkEmailVerified(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "verify@example.com", "", nil)
	if u.EmailVerifiedAt != nil {
		t.Fatal("a new account is already verified")
	}

	if err := db.MarkEmailVerified(ctx, u.ID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ := db.UserByID(ctx, u.ID)
	if got.EmailVerifiedAt == nil {
		t.Fatal("the account is still unverified after being marked")
	}

	// Following the link a second time changes nothing and says so, rather than
	// re-stamping the time — which would make the column mean "last clicked"
	// instead of "when proven".
	if err := db.MarkEmailVerified(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second verification reported %v, want ErrNotFound", err)
	}
	again, _ := db.UserByID(ctx, u.ID)
	if !again.EmailVerifiedAt.Equal(*got.EmailVerifiedAt) {
		t.Error("the verification timestamp moved on a second click")
	}
}
