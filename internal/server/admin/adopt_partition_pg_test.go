package admin

import (
	"context"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Adopt baselines an already-populated database: introspect the live schema,
// diff it against the declaration, and record a checkpoint without running DDL.
//
// The property these cover is that the checkpoint describes the DATABASE, not
// the file. Adopt used to baseline the declaration, which is the same thing
// only when the two already agree — and adopt exists precisely for the case
// where they might not.

// adoptPartitionSchema declares tenant isolation. The physical table the test
// creates deliberately has no policy, so the declaration is a statement of
// intent that the live database has not yet satisfied.
const adoptPartitionSchema = `
entity Doc in adoptp {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`

// createUnisolatedTable builds the physical table by hand: the right columns,
// and no row-level security at all. This is what a legacy table looks like on
// the day somebody points atlantis at it and declares how they want it scoped.
func createUnisolatedTable(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	// An entity with no `table` override lives at atlantis.<namespace>_<name>
	// — see introspect.physical. Getting this wrong makes the table invisible
	// to introspection, which reports every column as an addition and looks
	// like a different bug entirely.
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS atlantis.adoptp_doc (
			id     BIGINT PRIMARY KEY,
			tenant VARCHAR(16) NOT NULL,
			body   TEXT
		)`,
	} {
		if _, err := svc.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
}

// TestAdoptDoesNotBaselineIsolationTheDatabaseDoesNotHave is the regression.
//
// The table has no policy. The declaration says `partition by tenant`. Adopt
// reports that as drift — correctly, and it did so before this fix too — and
// then used to write the DECLARED entity into the checkpoint, recording the
// isolation as already present.
//
// What that cost: the next `tide plan` compared the declaration against a
// checkpoint saying the same thing, found nothing to do, and the policy was
// never created. The dispatcher reads that checkpoint, so it believed the
// table was partitioned; every caller read every tenant's rows while the one
// signal an operator can check — omit the tenant, get refused — kept reporting
// healthy. The operator had been shown the drift and told it was outstanding
// work, and the same transaction recorded it as done.
func TestAdoptDoesNotBaselineIsolationTheDatabaseDoesNotHave(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createUnisolatedTable(t, svc)

	resp, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller:    "adoptp",
		Files:     depScopeFiles("doc.atl", adoptPartitionSchema),
		AdoptedBy: "test",
	})
	if err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}
	if !resp.GetCheckpointWritten() {
		t.Fatalf("adopt refused to baseline; drift = %+v", resp.GetDrift())
	}

	// The drift is reported either way. Asserted so a future change that
	// silences it fails here rather than looking like an improvement.
	var sawPartitionDrift bool
	for _, d := range resp.GetDrift() {
		if d.GetKind() == "partition_added" {
			sawPartitionDrift = true
		}
	}
	if !sawPartitionDrift {
		t.Error("adopt did not report partition_added. The operator is not being told " +
			"the table lacks the isolation its declaration claims.")
	}

	// The checkpoint must describe the database.
	ir, err := svc.loadCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	var found bool
	for _, e := range ir.Entities {
		if e.ID() != "adoptp.Doc" {
			continue
		}
		found = true
		if e.PartitionField != "" {
			t.Errorf("checkpoint records partition_field = %q on a table with no policy.\n"+
				"  Nothing will ever create that policy: the next plan compares the "+
				"declaration against this and sees no change, and the dispatcher trusts "+
				"this to decide the table is tenant-scoped.", e.PartitionField)
		}
	}
	if !found {
		t.Fatal("adoptp.Doc is absent from the checkpoint; the table exists, so it should " +
			"have been baselined")
	}
}

// TestAdoptLeavesTheIsolationChangeStillToDo is the other half, and the one an
// operator actually experiences.
//
// Reporting the drift is not enough — the change has to remain DOABLE. This
// drives the real plan path after adopt and requires it to be non-empty, which
// is what "outstanding work" has to mean.
func TestAdoptLeavesTheIsolationChangeStillToDo(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	createUnisolatedTable(t, svc)

	if _, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller:    "adoptp",
		Files:     depScopeFiles("doc.atl", adoptPartitionSchema),
		AdoptedBy: "test",
	}); err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}

	plan, err := svc.PlanSchema(ctx, &adminpb.PlanSchemaRequest{
		Caller: "adoptp",
		Files:  depScopeFiles("doc.atl", adoptPartitionSchema),
	})
	if err != nil {
		t.Fatalf("PlanSchema after adopt: %v", err)
	}
	// Asserted on the emitted SQL rather than on a count, so this cannot pass
	// on some unrelated difference: the plan has to be the isolation change
	// specifically, which means a CREATE POLICY.
	sql := plan.GetUpSql()
	if sql == "" {
		t.Fatal("the plan after adopt is empty, so the isolation adopt reported as " +
			"outstanding can never be applied. An operator who adopts and then applies " +
			"is told everything is up to date, on a table serving every tenant to " +
			"every caller.")
	}
	if !strings.Contains(strings.ToUpper(sql), "CREATE POLICY") {
		t.Errorf("the plan after adopt emits no CREATE POLICY, so whatever it is "+
			"planning, it is not the tenant isolation the declaration asks for.\n%s", sql)
	}
}
