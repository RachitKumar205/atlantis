package admin

import (
	"context"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// InspectSchema answers "how does this declaration differ from the database"
// and nothing else. The value of the command is that it is safe to point at
// production from CI, so "writes nothing" is the property under test — not a
// remark in a doc comment.

// dbFingerprint captures every table an inspect could plausibly touch.
//
// Compared before and after rather than asserting on one table, because the
// path inspect shares with adopt writes to three of them: persistCheckpoint
// touches ir_checkpoint and schema_versions, insertAdoptHistory touches
// adopt_history, and upsertCallerFiles touches caller_registrations. Watching
// only the checkpoint would miss two.
func dbFingerprint(t *testing.T, svc *Service) [4]string {
	t.Helper()
	var out [4]string
	queries := []string{
		`SELECT COALESCE(md5(ir::text) || content_hash, 'none') FROM atlantis.ir_checkpoint WHERE id = 1`,
		`SELECT COALESCE(count(*)::text, '0') FROM atlantis.adopt_history`,
		`SELECT COALESCE(count(*)::text, '0') FROM atlantis.caller_registrations`,
		`SELECT COALESCE(count(*)::text, '0') FROM atlantis.schema_versions`,
	}
	for i, q := range queries {
		var v string
		if err := svc.pool.QueryRow(context.Background(), q).Scan(&v); err != nil {
			// The checkpoint row may not exist yet on a fresh database.
			v = "absent"
		}
		out[i] = v
	}
	return out
}

func TestInspectSchemaWritesNothing(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createUnisolatedTable(t, svc)

	// Adopt first, so the tables inspect could damage hold rows. Against a
	// fresh database this test is close to vacuous: no checkpoint row to
	// update, no history to append to, and a probe write landing on nothing,
	// so a planted write survives it.
	if _, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller:    "adoptp",
		Files:     depScopeFiles("doc.atl", adoptPartitionSchema),
		AdoptedBy: "test",
	}); err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}

	before := dbFingerprint(t, svc)
	for i, v := range before {
		if v == "absent" || v == "0" {
			t.Fatalf("fingerprint[%d] is %q before inspect even runs — this test "+
				"cannot detect a write to a table that has no rows", i, v)
		}
	}

	resp, err := svc.InspectSchema(ctx, &adminpb.InspectSchemaRequest{
		Caller: "adoptp",
		Files:  depScopeFiles("doc.atl", adoptPartitionSchema),
	})
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	// The call has to have done real work, or "wrote nothing" is trivially
	// true and this test proves only that the function can return.
	if len(resp.GetDrift()) == 0 {
		t.Fatal("inspect reported no drift against a table with no policy, so it " +
			"either did not run or is not comparing anything")
	}

	if after := dbFingerprint(t, svc); after != before {
		t.Errorf("inspect changed the database.\n  before %v\n  after  %v\n"+
			"  It shares compareToLive with adopt, whose very next line is "+
			"persistCheckpoint.", before, after)
	}
}

// What the test above does and does not prove, established by mutation:
//
//   - A write that COMMITS is caught. That is the property that matters: the
//     database is unchanged after the call.
//   - A write that does not commit is NOT caught, and does not need to be —
//     InspectSchema only ever rolls back, so such a write changes nothing.
//   - It says nothing about whether the write was attempted. The guard against
//     that is the READ ONLY access mode, which Postgres enforces: a probe
//     UPDATE inside InspectSchema fails with SQLSTATE 25006 before it can run.
//
// Both matter and neither substitutes for the other. The access mode stops the
// path being reached; this test stops a change persisting if some future
// version reaches it another way — a second connection, say, which the access
// mode would not cover.

// TestInspectSchemaReportsTheSameDriftAdoptWould pins the two against each
// other.
//
// They share compareToLive precisely so an operator can trust that what
// inspect showed them is what adopt will act on. If the two ever disagree,
// inspect becomes advice about a different database.
func TestInspectSchemaReportsTheSameDriftAdoptWould(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createUnisolatedTable(t, svc)

	insp, err := svc.InspectSchema(ctx, &adminpb.InspectSchemaRequest{
		Caller: "adoptp",
		Files:  depScopeFiles("doc.atl", adoptPartitionSchema),
	})
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	adopt, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller:    "adoptp",
		Files:     depScopeFiles("doc.atl", adoptPartitionSchema),
		AdoptedBy: "test",
	})
	if err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}

	got := driftKinds(insp.GetDrift())
	want := driftKinds(adopt.GetDrift())
	if len(got) != len(want) {
		t.Fatalf("inspect reported %d findings, adopt reported %d — an operator "+
			"deciding from inspect is deciding about something else.\n  inspect %v\n  adopt   %v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("finding %d differs: inspect %q, adopt %q", i, got[i], want[i])
		}
	}
}

func driftKinds(items []*adminpb.AdoptDriftItem) []string {
	out := make([]string, 0, len(items))
	for _, d := range items {
		out = append(out, d.GetEntityId()+"/"+d.GetField()+"/"+d.GetKind()+"/"+d.GetSeverity())
	}
	return out
}

// TestInspectSchemaReportsInSyncOnlyWhenItIs covers the field a CLI turns into
// an exit code.
//
// in_sync is reported by the server rather than derived from an empty list,
// so a client cannot confuse "the database matches" with "this build produced
// no findings it knows how to name".
func TestInspectSchemaReportsInSyncOnlyWhenItIs(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createUnisolatedTable(t, svc)
	files := depScopeFiles("doc.atl", adoptPartitionSchema)

	drifted, err := svc.InspectSchema(ctx, &adminpb.InspectSchemaRequest{Caller: "adoptp", Files: files})
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if drifted.GetInSync() {
		t.Error("in_sync is true while the declaration asks for isolation the table " +
			"does not have. A CLI would exit 0 and CI would go green on it.")
	}

	// Adopt, then inspect again. The database now matches what was baselined,
	// so the only remaining difference is the isolation still to be applied.
	if _, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller: "adoptp", Files: files, AdoptedBy: "test",
	}); err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}
	after, err := svc.InspectSchema(ctx, &adminpb.InspectSchemaRequest{Caller: "adoptp", Files: files})
	if err != nil {
		t.Fatalf("InspectSchema after adopt: %v", err)
	}
	// Still not in sync — adopt recorded what the database has, and the
	// declaration still asks for a policy nobody has created. That is the
	// property 3f819ac established, checked here from the other side.
	if after.GetInSync() {
		t.Error("in_sync is true after adopting a table whose isolation was never " +
			"created. Adopt has recorded the work as done again.")
	}
}
