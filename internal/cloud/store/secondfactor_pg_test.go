package store

import (
	"context"
	"testing"
)

// The store-level contracts of the two spend operations.
//
// # Why these are here and not only at the HTTP layer
//
// internal/cloud/server tests both of these end to end, and one of them was
// covered twice over while the other was not covered at all. Mutation testing
// found the difference: deleting `AND used_at IS NULL` from SpendBackupCode
// changed nothing any test could see.
//
// The reason is that the server asks UnusedBackupCodeHashes first, and that
// query filters spent codes out of the candidate set — so a second presentation
// of the same code never reaches SpendBackupCode at all. The predicate the
// function's own comment describes as what stops two requests both succeeding
// was, in practice, unreachable from above.
//
// It is still load-bearing, for exactly the case the comment names: two
// requests presenting one code at the same moment both read it as unused, both
// match it, and both call this. Only the predicate decides which one wins. That
// is not reachable from an HTTP test without a race, so the contract is
// asserted here, where it lives.

// A backup code cannot be spent twice.
func TestSpendingABackupCodeTwiceFails(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "codes@example.com", "Codes", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	us := db.ForUser(u.ID)

	// Not real argon2id hashes. Nothing here verifies them — this exercises the
	// spend, and hashing ten codes for that would add half a second per run.
	if err := us.ReplaceBackupCodes(ctx, []string{"hash-a", "hash-b"}); err != nil {
		t.Fatalf("store codes: %v", err)
	}

	ids, err := us.UnusedBackupCodeHashes(ctx)
	if err != nil {
		t.Fatalf("read codes: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("stored 2 codes, read back %d", len(ids))
	}

	var first int64 = -1
	for id := range ids {
		if first < 0 || id < first {
			first = id
		}
	}

	spent, err := us.SpendBackupCode(ctx, first)
	if err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if !spent {
		t.Fatal("an unused backup code would not spend")
	}

	// The same id again. This is what a second concurrent request holds.
	spent, err = us.SpendBackupCode(ctx, first)
	if err != nil {
		t.Fatalf("second spend: %v", err)
	}
	if spent {
		t.Fatal("a backup code spent twice reported success both times, so two " +
			"requests presenting one code would both sign in")
	}

	// And the other code is untouched, so a spend is not a reset.
	left, err := us.UnusedBackupCodeHashes(ctx)
	if err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("%d codes remain, want 1", len(left))
	}
	if _, still := left[first]; still {
		t.Fatal("the spent code is still listed as available")
	}
}

// A backup code belonging to somebody else cannot be spent.
//
// The row-level-security policy is the boundary, but SpendBackupCode also names
// the user in its WHERE clause. Both are asserted because they fail
// differently: a missing bind returns nothing, and a missing user_id predicate
// spends a row the policy would have hidden from a read.
func TestABackupCodeCannotBeSpentByAnotherAccount(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	mine, err := db.CreateUser(ctx, "mine@example.com", "Mine", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs, err := db.CreateUser(ctx, "theirs@example.com", "Theirs", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := db.ForUser(mine.ID).ReplaceBackupCodes(ctx, []string{"hash-a"}); err != nil {
		t.Fatalf("store codes: %v", err)
	}
	ids, err := db.ForUser(mine.ID).UnusedBackupCodeHashes(ctx)
	if err != nil {
		t.Fatalf("read codes: %v", err)
	}
	var id int64
	for k := range ids {
		id = k
	}

	spent, err := db.ForUser(theirs.ID).SpendBackupCode(ctx, id)
	if err != nil {
		t.Fatalf("cross-account spend: %v", err)
	}
	if spent {
		t.Fatal("one account spent another account's backup code")
	}

	// Still available to its owner.
	left, err := db.ForUser(mine.ID).UnusedBackupCodeHashes(ctx)
	if err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if len(left) != 1 {
		t.Fatal("the code was consumed by the account that does not own it")
	}
}

// A TOTP step cannot be spent twice, and an earlier step cannot be replayed
// after a later one.
//
// The server proves the first half end to end. The second half — that the
// comparison is `<` and not `!=` — is only visible from here, because reaching
// it over HTTP means presenting a code from a window that has already passed.
func TestSpendingATOTPStepTwiceFails(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "steps@example.com", "Steps", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	us := db.ForUser(u.ID)
	if err := us.PutTOTPSecret(ctx, []byte("not-a-real-ciphertext")); err != nil {
		t.Fatalf("store secret: %v", err)
	}

	spent, err := us.SpendTOTPStep(ctx, 1000)
	if err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if !spent {
		t.Fatal("the first code from a fresh secret would not spend")
	}

	spent, err = us.SpendTOTPStep(ctx, 1000)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if spent {
		t.Fatal("the same TOTP step spent twice, so a captured code stays " +
			"usable for the rest of its window")
	}

	// An earlier step, presented after a later one. Refused, which is what the
	// `<` comparison buys over an equality check.
	spent, err = us.SpendTOTPStep(ctx, 999)
	if err != nil {
		t.Fatalf("earlier step: %v", err)
	}
	if spent {
		t.Fatal("a step earlier than the last one spent was accepted")
	}

	// The next one still works, so this is a replay guard and not a lock.
	spent, err = us.SpendTOTPStep(ctx, 1001)
	if err != nil {
		t.Fatalf("next step: %v", err)
	}
	if !spent {
		t.Fatal("the next window's code was refused")
	}
}

// Re-enrolling clears the spent-step marker.
//
// Otherwise an account that re-enrols keeps the previous secret's high-water
// mark, and every code from the new authenticator below it is refused — which
// presents as "my new authenticator does not work" with nothing in the logs.
func TestReEnrollingResetsTheStepMarker(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "again@example.com", "Again", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	us := db.ForUser(u.ID)

	if err := us.PutTOTPSecret(ctx, []byte("first")); err != nil {
		t.Fatalf("store secret: %v", err)
	}
	if err := us.ConfirmTOTP(ctx, 5000); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	confirmed, err := us.HasConfirmedFactor(ctx)
	if err != nil {
		t.Fatalf("check factor: %v", err)
	}
	if !confirmed {
		t.Fatal("a confirmed factor reports as absent")
	}

	if err := us.PutTOTPSecret(ctx, []byte("second")); err != nil {
		t.Fatalf("re-enrol: %v", err)
	}

	// Unconfirmed again — a replaced secret is not a factor until it is proved.
	confirmed, err = us.HasConfirmedFactor(ctx)
	if err != nil {
		t.Fatalf("check factor: %v", err)
	}
	if confirmed {
		t.Fatal("replacing the secret left the old enrolment confirmed, so a " +
			"half-finished re-enrol would lock the account out")
	}

	// A step below the old marker is accepted, because the marker went with it.
	spent, err := us.SpendTOTPStep(ctx, 1)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if !spent {
		t.Fatal("the replaced secret inherited the previous one's spent step")
	}
}

// An unbound handle reads nothing and writes nothing, and says so.
//
// This is the failure the whole bound-handle design exists to make impossible:
// under the RESTRICTIVE policy an unbound query matches no rows, so it looks
// like an account with no second factor rather than like a bug. The check is an
// error rather than an empty result for that reason.
func TestAnUnboundHandleRefusesRatherThanReadingNothing(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	us := db.ForUser("")

	if _, err := us.HasConfirmedFactor(ctx); err != ErrNoUser {
		t.Errorf("HasConfirmedFactor returned %v, want ErrNoUser", err)
	}
	if _, _, err := us.TOTPSecret(ctx); err != ErrNoUser {
		t.Errorf("TOTPSecret returned %v, want ErrNoUser", err)
	}
	if err := us.PutTOTPSecret(ctx, []byte("x")); err != ErrNoUser {
		t.Errorf("PutTOTPSecret returned %v, want ErrNoUser", err)
	}
	if _, err := us.SpendTOTPStep(ctx, 1); err != ErrNoUser {
		t.Errorf("SpendTOTPStep returned %v, want ErrNoUser", err)
	}
	if _, err := us.UnusedBackupCodeHashes(ctx); err != ErrNoUser {
		t.Errorf("UnusedBackupCodeHashes returned %v, want ErrNoUser", err)
	}
	if _, err := us.SpendBackupCode(ctx, 1); err != ErrNoUser {
		t.Errorf("SpendBackupCode returned %v, want ErrNoUser", err)
	}
}
