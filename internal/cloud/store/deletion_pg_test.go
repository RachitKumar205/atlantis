package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// deletable makes one ready organisation with an admin and a viewer.
//
// Ready, not pending: SoftDeleteOrg is guarded on 'ready', so a fixture that
// left the row pending would have every test here pass on the guard rather
// than on the logic under test.
func deletable(t *testing.T, db *Store, org string) (adminID, viewerID string) {
	t.Helper()
	ctx := context.Background()

	admin, err := db.CreateUser(ctx, org+"-admin@example.test", "Admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := db.CreateUser(ctx, org+"-viewer@example.test", "Viewer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrgWithOwner(ctx, org, org, admin.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx,
		`INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, $2, 'viewer')`,
		viewer.ID, org); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioned(ctx, org); err != nil {
		t.Fatal(err)
	}
	return admin.ID, viewer.ID
}

func stateOf(t *testing.T, db *Store, org string) ProvisioningState {
	t.Helper()
	p, err := db.ProvisioningFor(context.Background(), org)
	if err != nil {
		t.Fatalf("read %s: %v", org, err)
	}
	return p.State
}

func TestSoftDeleteNeedsAnAdminAndLeavesTheOrgIntact(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, viewerID := deletable(t, db, "delrole")

	// A viewer is refused, and told so — they already know it exists.
	err := db.SoftDeleteOrg(ctx, "delrole", viewerID, 30*24*time.Hour)
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("a viewer deleted the organisation, or got the wrong error: %v", err)
	}
	if got := stateOf(t, db, "delrole"); got != StateReady {
		t.Errorf("state is %q after a refused delete, want ready", got)
	}

	outsider, err := db.CreateUser(ctx, "outsider@example.test", "Out", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDeleteOrg(ctx, "delrole", outsider.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("a non-member got %v, want ErrNotFound — ErrNotPermitted would "+
			"confirm the organisation exists", err)
	}

	if err := db.SoftDeleteOrg(ctx, "delrole", adminID, 30*24*time.Hour); err != nil {
		t.Fatalf("an admin could not delete: %v", err)
	}
	if got := stateOf(t, db, "delrole"); got != StateDeleted {
		t.Fatalf("state is %q after delete, want deleted", got)
	}
}

// Without the state = 'ready' guard a retry would push purge_after further out
// and extend a window somebody is counting on.
func TestASecondDeleteDoesNotMoveTheWindow(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, _ := deletable(t, db, "deltwice")

	if err := db.SoftDeleteOrg(ctx, "deltwice", adminID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := db.pool.QueryRow(ctx,
		`SELECT purge_after FROM cloud.org_provisioning WHERE org = 'deltwice'`).
		Scan(&first); err != nil {
		t.Fatal(err)
	}

	// A much longer window, so a successful second call would be unmistakable.
	if err := db.SoftDeleteOrg(ctx, "deltwice", adminID, 900*time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an already-deleted organisation returned %v, want a refusal", err)
	}

	var second time.Time
	if err := db.pool.QueryRow(ctx,
		`SELECT purge_after FROM cloud.org_provisioning WHERE org = 'deltwice'`).
		Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first) {
		t.Errorf("purge_after moved from %s to %s: a repeated delete extended the window",
			first, second)
	}
}

func TestRestoreReturnsItToService(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, viewerID := deletable(t, db, "delrestore")

	if err := db.SoftDeleteOrg(ctx, "delrestore", adminID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.RestoreOrg(ctx, "delrestore", viewerID); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("a viewer restored the organisation: %v", err)
	}
	if err := db.RestoreOrg(ctx, "delrestore", adminID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := stateOf(t, db, "delrestore"); got != StateReady {
		t.Fatalf("state is %q after restore, want ready", got)
	}

	// purge_after is cleared. Left set, the reaper takes the organisation the
	// moment that date passes, weeks after somebody restored it.
	var after *time.Time
	if err := db.pool.QueryRow(ctx,
		`SELECT purge_after FROM cloud.org_provisioning WHERE org = 'delrestore'`).
		Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != nil {
		t.Errorf("purge_after is still %s after a restore: the reaper would take it later", *after)
	}
}

func TestThePurgeClaimWaitsForTheWindow(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, _ := deletable(t, db, "delwait")

	if err := db.SoftDeleteOrg(ctx, "delwait", adminID, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForPurge(ctx, "reaper", time.Minute); !errors.Is(err, ErrNothingToPurge) {
		t.Fatalf("an organisation deleted thirty days ago was claimed for destruction "+
			"immediately: %v", err)
	}

	// Bring the window forward the way PurgeNow does.
	if err := db.PurgeNow(ctx, "delwait"); err != nil {
		t.Fatal(err)
	}
	c, err := db.ClaimForPurge(ctx, "reaper", time.Minute)
	if err != nil {
		t.Fatalf("a due organisation was not claimed: %v", err)
	}
	if c.Org != "delwait" {
		t.Fatalf("claimed %q, want delwait", c.Org)
	}
	if got := stateOf(t, db, "delwait"); got != StatePurging {
		t.Fatalf("state is %q after a claim, want purging", got)
	}

	// A second reaper must not take the organisation while the first holds the
	// lease: two provisioners would tear down one namespace.
	if _, err := db.ClaimForPurge(ctx, "reaper-2", time.Minute); !errors.Is(err, ErrNothingToPurge) {
		t.Errorf("a second reaper claimed an organisation already being purged: %v", err)
	}
}

// A provisioner that died mid-teardown must not strand the organisation.
func TestAnExpiredPurgeLeaseIsReclaimed(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, _ := deletable(t, db, "delstale")

	if err := db.SoftDeleteOrg(ctx, "delstale", adminID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeNow(ctx, "delstale"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForPurge(ctx, "died", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Expire the lease without stopping the claimant.
	if _, err := db.pool.Exec(ctx,
		`UPDATE cloud.org_provisioning SET claimed_until = NOW() - interval '1 minute'
		  WHERE org = 'delstale'`); err != nil {
		t.Fatal(err)
	}

	c, err := db.ClaimForPurge(ctx, "took-over", time.Minute)
	if err != nil {
		t.Fatalf("an organisation left in purging by a dead provisioner was never "+
			"reclaimed, so it would sit half-destroyed for ever: %v", err)
	}
	if c.Org != "delstale" {
		t.Fatalf("claimed %q, want delstale", c.Org)
	}
}

func TestPurgeRemovesEveryTrace(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, _ := deletable(t, db, "delgone")

	if err := db.SoftDeleteOrg(ctx, "delgone", adminID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeNow(ctx, "delgone"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForPurge(ctx, "reaper", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPurged(ctx, "delgone"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, q := range []struct {
		what string
		sql  string
	}{
		{"provisioning row", `SELECT count(*) FROM cloud.org_provisioning WHERE org = 'delgone'`},
		{"memberships", `SELECT count(*) FROM cloud.memberships WHERE org = 'delgone'`},
		{"the organisation", `SELECT count(*) FROM cloud.orgs WHERE name = 'delgone'`},
	} {
		var n int
		if err := db.pool.QueryRow(ctx, q.sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s survived the purge (%d rows): a half-purged organisation "+
				"appears in somebody's list with nothing behind it", q.what, n)
		}
	}

	// The name is free again, which is what `cloud org purge` is for.
	if err := db.CreateOrgWithOwner(ctx, "delgone", "Again", adminID, identity.RoleAdmin); err != nil {
		t.Errorf("the name is still taken after a purge: %v", err)
	}
}

// Neither ReadyOrgs nor Requeue mentions deletion; both filter on
// state = 'ready', so a later change to either would remove the protection
// without looking like it touched deletion.
//
// The reconcile loop re-Ensures everything ReadyOrgs returns, so a deleted
// organisation appearing there has its namespace rebuilt while it waits to be
// destroyed.
func TestADeletedOrgIsInvisibleToReconcileAndRequeue(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	adminID, _ := deletable(t, db, "delhidden")

	ready, err := db.ReadyOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ready, "delhidden") {
		t.Fatal("the fixture is not ready, so this test would pass for the wrong reason")
	}

	if err := db.SoftDeleteOrg(ctx, "delhidden", adminID, time.Hour); err != nil {
		t.Fatal(err)
	}

	ready, err = db.ReadyOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if contains(ready, "delhidden") {
		t.Error("a deleted organisation is still in ReadyOrgs: the reconcile loop " +
			"would rebuild the namespace it is waiting to destroy")
	}

	if err := db.Requeue(ctx, "delhidden", "should not happen"); err == nil {
		if got := stateOf(t, db, "delhidden"); got != StateDeleted {
			t.Errorf("Requeue resurrected a deleted organisation to %q", got)
		}
	}
	if got := stateOf(t, db, "delhidden"); got != StateDeleted {
		t.Errorf("state is %q after Requeue, want deleted", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
