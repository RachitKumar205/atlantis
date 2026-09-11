package console

import (
	"context"
	"testing"
	"time"
)

// The onboarding flow is answered once per organisation, and the record is the
// organisation's rather than a browser's.
//
// Before migration 0018 the record lived in localStorage keyed by slug, which
// answered for one browser: a second admin was offered the flow again, and so
// was the same person on a second machine.

func TestAnOrganisationHasNotOnboardedUntilItSaysSo(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	done, err := f.srv.db.onboarded(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("onboarded: %v", err)
	}
	if done {
		t.Error("a fresh organisation reports having answered the onboarding flow")
	}

	if err := f.srv.db.markOnboarded(ctx, defaultOrg); err != nil {
		t.Fatalf("markOnboarded: %v", err)
	}

	done, err = f.srv.db.onboarded(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("onboarded after marking: %v", err)
	}
	if !done {
		t.Error("an organisation that answered still reports it has not")
	}
}

// The timestamp records when the organisation first answered. Reopening the
// flow from the schema page and closing it again does not move it.
func TestMarkingOnboardedTwiceKeepsTheFirstAnswer(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	if err := f.srv.db.markOnboarded(ctx, defaultOrg); err != nil {
		t.Fatalf("markOnboarded: %v", err)
	}
	var first time.Time
	if err := f.srv.db.pool.QueryRow(ctx,
		`SELECT onboarded_at FROM console.orgs WHERE org = $1`, defaultOrg).Scan(&first); err != nil {
		t.Fatalf("read the first answer: %v", err)
	}

	if err := f.srv.db.markOnboarded(ctx, defaultOrg); err != nil {
		t.Fatalf("markOnboarded again: %v", err)
	}
	var second time.Time
	if err := f.srv.db.pool.QueryRow(ctx,
		`SELECT onboarded_at FROM console.orgs WHERE org = $1`, defaultOrg).Scan(&second); err != nil {
		t.Fatalf("read the second answer: %v", err)
	}
	if !first.Equal(second) {
		t.Errorf("the answer moved from %s to %s", first, second)
	}
}

// One organisation answering says nothing about another. The console serves
// several, and they onboard at different times.
func TestOnboardingIsPerOrganisation(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()

	const other = "globex"
	if err := f.srv.db.rememberOrg(ctx, other); err != nil {
		t.Fatalf("rememberOrg: %v", err)
	}
	if err := f.srv.db.markOnboarded(ctx, defaultOrg); err != nil {
		t.Fatalf("markOnboarded: %v", err)
	}

	done, err := f.srv.db.onboarded(ctx, other)
	if err != nil {
		t.Fatalf("onboarded: %v", err)
	}
	if done {
		t.Errorf("%s answered and %s was marked with it", defaultOrg, other)
	}
}

// An organisation nobody has signed in to has no row, which is not an error:
// rememberOrg writes one on the first exchange.
func TestAnUnknownOrganisationHasNotOnboarded(t *testing.T) {
	f := newFixture(t, false)

	done, err := f.srv.db.onboarded(context.Background(), "nobody-has-been-here")
	if err != nil {
		t.Fatalf("onboarded: %v", err)
	}
	if done {
		t.Error("an organisation with no row reports having answered")
	}
}
