package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueAndSpendAToken(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "ada@example.com", "Ada", "", nil)
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

// The token travels in a URL, so it lands in browser history, in the referrer
// of anything the landing page loads, and in logs along the way. Stored
// verbatim, a read of this table is a working reset for every account with one
// pending.
func TestTheTokenItselfIsNotStored(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "stored@example.com", "", "", nil)
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

	// And the right hash: without this the check above passes against a column
	// of unrelated bytes.
	want := sha256.Sum256([]byte(token))
	if string(stored) != string(want[:]) {
		t.Error("the stored hash is not the SHA-256 of the token")
	}
}

func TestATokenCannotBeSpentTwice(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "once@example.com", "", "", nil)
	token, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	_, err := db.SpendEmailToken(ctx, token, PurposeResetPassword)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a reset link worked twice: %v", err)
	}
}

// Verification is issued at sign-up to an address nobody has proven; reset is
// issued to one already proven. A token good for both would be only as strong
// as the weaker path that issued it.
func TestATokenIsBoundToItsPurpose(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "purpose@example.com", "", "", nil)
	verify, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)

	if _, err := db.SpendEmailToken(ctx, verify, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a verification token was accepted to reset a password: %v", err)
	}
	// And it still works for its own purpose: without this the check above
	// passes against a spend that refuses everything.
	if _, err := db.SpendEmailToken(ctx, verify, PurposeVerifyEmail); err != nil {
		t.Errorf("the token no longer works for its own purpose: %v", err)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "expired@example.com", "", "", nil)
	token, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an expired token was accepted: %v", err)
	}
}

// One error for unknown, expired, spent and wrong-purpose. Telling them apart
// would let somebody probe for valid tokens.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	db := newTestStore(t)

	_, err := db.SpendEmailToken(context.Background(), "not-a-real-token", PurposeResetPassword)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("got %v, want ErrTokenInvalid", err)
	}
}

// Without the address check, somebody who controlled the mailbox when the reset
// was issued keeps a working reset after losing control of it.
func TestATokenDiesWithTheAddressItWasSentTo(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "old@example.com", "", "", nil)
	token, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, ResetTokenTTL)

	if _, err := db.pool.Exec(ctx,
		`UPDATE cloud.users SET email = 'new@example.com' WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("change address: %v", err)
	}

	if _, err := db.SpendEmailToken(ctx, token, PurposeResetPassword); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a reset issued to the old address still worked: %v", err)
	}
}

// Requesting three resets otherwise leaves three live credentials, and using
// one leaves two.
func TestSettingAPasswordInvalidatesOtherResets(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "many@example.com", "", "", nil)
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

// Setting a password must not void a pending address verification: the user
// would follow that link, be told it is invalid, and have no way to get another
// except by changing their address.
func TestInvalidationIsScopedToItsPurpose(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "scoped@example.com", "", "", nil)
	verify, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)

	if err := db.InvalidateEmailTokens(ctx, u.ID, PurposeResetPassword); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := db.SpendEmailToken(ctx, verify, PurposeVerifyEmail); err != nil {
		t.Errorf("invalidating resets also voided a pending verification: %v", err)
	}
}

func TestExpiredTokensAreSwept(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, _ := db.CreateUser(ctx, "sweep@example.com", "", "", nil)
	live, _ := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeVerifyEmail, VerifyTokenTTL)
	if _, err := db.IssueEmailToken(ctx, u.ID, u.Email, PurposeResetPassword, -time.Minute); err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	n, err := db.DeleteExpiredEmailTokens(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Asserted on the count: a sweep that deletes nothing also reports success.
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

	u, _ := db.CreateUser(ctx, "pw@example.com", "", "", nil)
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

	u, _ := db.CreateUser(ctx, "verify@example.com", "", "", nil)
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

	// A second click reports ErrNotFound and leaves the timestamp alone, so the
	// column means when the address was proven, not when it was last clicked.
	if err := db.MarkEmailVerified(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second verification reported %v, want ErrNotFound", err)
	}
	again, _ := db.UserByID(ctx, u.ID)
	if !again.EmailVerifiedAt.Equal(*got.EmailVerifiedAt) {
		t.Error("the verification timestamp moved on a second click")
	}
}
