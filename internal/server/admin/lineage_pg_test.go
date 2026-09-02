package admin

import (
	"context"
	"fmt"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Blame comes off the baseline an adopt writes, not off the diff it reports.
//
// The two describe different sets. A diff says what the declaration has and
// the database does not; filterToExistingEntities then drops exactly those
// from the checkpoint. Reading blame off the diff therefore files it against
// tables the checkpoint does not contain, and files nothing at all when the
// declaration and the database already agree — which is what an import is.
//
// Nothing in this package covered atlantis.entity_lineage before these.

const lineageSchema = `
entity Doc in lin {
  id    bigint primary
  title varchar(200) not null
  body  text
}
`

// A second caller, so ownership has something to tell apart.
const lineageOtherSchema = `
entity Tag in lin2 {
  id   bigint primary
  name varchar(64) not null
}
`

// declared but never built, so adopt sees it as drift and the checkpoint
// drops it
const lineageAbsentSchema = `
entity Ghost in lin {
  id   bigint primary
  name text
}
`

func createLineageTables(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS atlantis.lin_doc (
			id    BIGINT PRIMARY KEY,
			title VARCHAR(200) NOT NULL,
			body  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS atlantis.lin2_tag (
			id   BIGINT PRIMARY KEY,
			name VARCHAR(64) NOT NULL
		)`,
	} {
		if _, err := svc.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
}

// adoptTwoCallers baselines both tables as two separate callers, so that
// per-entity ownership is distinguishable from the event's own caller.
func adoptTwoCallers(t *testing.T, svc *Service, allowDrift bool, extra string) *adminpb.AdoptBaselineResponse {
	t.Helper()
	docFiles := depScopeFiles("doc.atl", lineageSchema+extra)
	resp, err := svc.AdoptBaseline(context.Background(), &adminpb.AdoptBaselineRequest{
		Submissions: []*adminpb.CallerSubmission{
			{Caller: "lin", Files: docFiles},
			{Caller: "lin2", Files: depScopeFiles("tag.atl", lineageOtherSchema)},
		},
		AllowDrift:     allowDrift,
		AdoptedBy:      "console:usr_lineage",
		AdoptedByEmail: "ada@example.com",
		AdoptedByName:  "Ada Lovelace",
	})
	if err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}
	return resp
}

// lineageRows reads the blame table back as entity_id -> field_name -> caller.
func lineageRows(t *testing.T, svc *Service) map[string]map[string]string {
	t.Helper()
	rows, err := svc.pool.Query(context.Background(),
		`SELECT entity_id, field_name, introduced_by FROM atlantis.entity_lineage WHERE removed_at IS NULL`)
	if err != nil {
		t.Fatalf("read lineage: %v", err)
	}
	defer rows.Close()

	out := map[string]map[string]string{}
	for rows.Next() {
		var entity, field, by string
		if err := rows.Scan(&entity, &field, &by); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if out[entity] == nil {
			out[entity] = map[string]string{}
		}
		out[entity][field] = by
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestAdoptWritesLineageForZeroDriftBaseline is the reported regression.
//
// An import whose declaration matches the database produces an empty diff, and
// blame read off that diff is nothing at all. The console then has no row for
// any entity and renders every one of them as unattributed — 97 of them, on
// the import this was found in.
func TestAdoptWritesLineageForZeroDriftBaseline(t *testing.T) {
	svc := depScopeService(t)
	createLineageTables(t, svc)

	resp := adoptTwoCallers(t, svc, false, "")
	if !resp.GetCheckpointWritten() {
		t.Fatalf("adopt refused to baseline; drift = %+v", resp.GetDrift())
	}

	got := lineageRows(t, svc)
	for _, want := range []string{"lin.Doc", "lin2.Tag"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s has no lineage row.\n"+
				"  A successful adopt recorded nothing for it, so the console has "+
				"nothing to attribute the table to.", want)
		}
	}

	// Per-field rows, not just the entity: blame is per column.
	for _, f := range []string{"", "id", "title", "body"} {
		if _, ok := got["lin.Doc"][f]; !ok {
			t.Errorf("lin.Doc has no row for field %q", f)
		}
	}

	// The event's caller is the string "adopt". The entity's is the caller
	// that declared it, which is the question the schema page asks.
	if by := got["lin.Doc"][""]; by != "lin" {
		t.Errorf("lin.Doc introduced_by = %q, want \"lin\"", by)
	}
	if by := got["lin2.Tag"][""]; by != "lin2" {
		t.Errorf("lin2.Tag introduced_by = %q, want \"lin2\"", by)
	}
}

// TestAdoptDoesNotAttributeUnbuiltEntities is the other half of the same
// mistake, and it fails in the opposite direction.
//
// A declared entity with no table is what the diff calls added, and it is
// exactly what filterToExistingEntities removes from the checkpoint. Blame
// read off the diff records a row the checkpoint cannot draw.
func TestAdoptDoesNotAttributeUnbuiltEntities(t *testing.T) {
	svc := depScopeService(t)
	createLineageTables(t, svc)

	// AllowDrift, because the absent table is drift by definition.
	adoptTwoCallers(t, svc, true, lineageAbsentSchema)

	got := lineageRows(t, svc)
	if _, ok := got["lin.Ghost"]; ok {
		t.Error("lin.Ghost has a lineage row.\n" +
			"  No such table exists and the checkpoint dropped the entity, so " +
			"the console would list a table it cannot open.")
	}
	if _, ok := got["lin.Doc"]; !ok {
		t.Error("lin.Doc lost its lineage row while filtering out the absent one")
	}
}

// TestAdoptLineageIsIdempotent guards the recovery path.
//
// Re-running an import is how an organisation gets attribution onto a schema
// baselined before this existed. If a second adopt relabelled the
// introduction, that recovery would destroy the history it was run to build.
func TestAdoptLineageIsIdempotent(t *testing.T) {
	svc := depScopeService(t)
	createLineageTables(t, svc)

	adoptTwoCallers(t, svc, false, "")
	first := introducedAt(t, svc, "lin.Doc")

	// A second adopt of the same declaration. The hash matches, so this is
	// the AlreadyAdopted path, and it must not disturb what is recorded.
	adoptTwoCallers(t, svc, false, "")
	if second := introducedAt(t, svc, "lin.Doc"); second != first {
		t.Errorf("introduced_at moved from version %d to %d on re-adopt.\n"+
			"  The introduction is when the table first appeared, not when it "+
			"was last confirmed.", first, second)
	}
}

func introducedAt(t *testing.T, svc *Service, entityID string) int64 {
	t.Helper()
	var v int64
	err := svc.pool.QueryRow(context.Background(),
		`SELECT introduced_at FROM atlantis.entity_lineage
		 WHERE entity_id = $1 AND field_name = ''`, entityID).Scan(&v)
	if err != nil {
		t.Fatalf("read introduced_at for %s: %v", entityID, err)
	}
	return v
}

// TestAdoptRecordsTheActorBesideTheCaller covers the provenance column.
//
// The caller is the mTLS identity the request arrived under and the server
// verified it. The actor is a string the request supplied, recorded as given.
// Both are stored, and the version row is where the human belongs — an
// entity_lineage copy could disagree with it.
func TestAdoptRecordsTheActorBesideTheCaller(t *testing.T) {
	svc := depScopeService(t)
	createLineageTables(t, svc)
	adoptTwoCallers(t, svc, false, "")

	var caller, actor, email, name string
	err := svc.pool.QueryRow(context.Background(),
		`SELECT caller, actor, actor_email, actor_name FROM atlantis.schema_versions
		 WHERE event_type = 'adopt' ORDER BY version DESC LIMIT 1`).
		Scan(&caller, &actor, &email, &name)
	if err != nil {
		t.Fatalf("read version row: %v", err)
	}
	if caller != "adopt" {
		t.Errorf("caller = %q, want \"adopt\" — the event, not the person", caller)
	}
	if actor != "console:usr_lineage" {
		t.Errorf("actor = %q, want \"console:usr_lineage\"", actor)
	}
	// All three, because the principal alone renders as the caller. An adopt
	// that stored the id and dropped the address and the name put
	// "atlantis-console" on the schema page under "last modified by".
	if email != "ada@example.com" {
		t.Errorf("actor_email = %q, want \"ada@example.com\"", email)
	}
	if name != "Ada Lovelace" {
		t.Errorf("actor_name = %q, want \"Ada Lovelace\"", name)
	}
}

// TestEntityOwnersCarriesBlame covers the read side of the version join.
//
// The lineage row names a version; the version is where the human and the
// timestamp live. Without the join the console has an entity and a caller and
// no answer to when, which is half of what blame is.
func TestEntityOwnersCarriesBlame(t *testing.T) {
	svc := depScopeService(t)
	createLineageTables(t, svc)
	adoptTwoCallers(t, svc, false, "")

	resp, err := svc.GetEntityOwners(context.Background(), &adminpb.GetEntityOwnersRequest{})
	if err != nil {
		t.Fatalf("GetEntityOwners: %v", err)
	}

	var found bool
	for _, o := range resp.GetOwners() {
		if o.GetEntityId() != "lin.Doc" {
			continue
		}
		found = true
		b := o.GetIntroduced()
		if b == nil {
			t.Fatal("introduced blame is nil; protojson omits a nil message " +
				"entirely, so the console cannot tell it from a missing field")
		}
		if b.GetCaller() != "lin" {
			t.Errorf("introduced.caller = %q, want \"lin\"", b.GetCaller())
		}
		if b.GetAt() == "" {
			t.Error("introduced.at is empty; the version join returned no timestamp")
		}
		if b.GetActor() != "console:usr_lineage" {
			t.Errorf("introduced.actor = %q, want the adopting human", b.GetActor())
		}
		if o.GetLastModified() == nil || o.GetLastModified().GetAt() == "" {
			t.Error("last_modified blame is empty; the query did not project " +
				"last_modified_by/at at all before this")
		}
	}
	if !found {
		t.Fatal("lin.Doc absent from GetEntityOwners")
	}
}

// TestGetSchemaHistoryFiltersByEntity covers the jsonpath.
//
// The filter walks every bucket rather than naming them. A Destructive change
// is the case that catches a filter written against a hand-listed set, because
// it is the bucket added last and the one such lists have missed before.
func TestGetSchemaHistoryFiltersByEntity(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()

	// Two versions, each naming a different entity, and each in a bucket a
	// hand-written filter would have to remember.
	for _, tc := range []struct{ entity, bucket string }{
		{"filt.Kept", "additive"},
		{"filt.Dropped", "destructive"},
	} {
		diff := fmt.Sprintf(`{"%s":[{"kind":1,"class":1,"entity_id":%q}]}`, tc.bucket, tc.entity)
		if _, err := svc.pool.Exec(ctx, `
INSERT INTO atlantis.schema_versions
    (caller, plan_class, diff, ir_snapshot, ir_hash, event_type)
VALUES ('filt', 'additive', $1::jsonb, '{}'::jsonb, 'h', 'apply')`, diff); err != nil {
			t.Fatalf("seed version for %s: %v", tc.entity, err)
		}
	}

	only := func(entityID string) []string {
		t.Helper()
		resp, err := svc.GetSchemaHistory(ctx, &adminpb.GetSchemaHistoryRequest{EntityId: entityID})
		if err != nil {
			t.Fatalf("GetSchemaHistory(%s): %v", entityID, err)
		}
		var callers []string
		for _, v := range resp.GetVersions() {
			callers = append(callers, v.GetCaller())
		}
		return callers
	}

	if got := only("filt.Dropped"); len(got) != 1 {
		t.Errorf("filtering on a destructive change returned %d versions, want 1", len(got))
	}
	if got := only("filt.Kept"); len(got) != 1 {
		t.Errorf("filtering on an additive change returned %d versions, want 1", len(got))
	}
	if got := only("filt.Absent"); len(got) != 0 {
		t.Errorf("filtering on an untouched entity returned %d versions, want 0", len(got))
	}

	// And unfiltered still returns both, so the predicate is genuinely optional.
	all, err := svc.GetSchemaHistory(ctx, &adminpb.GetSchemaHistoryRequest{})
	if err != nil {
		t.Fatalf("GetSchemaHistory: %v", err)
	}
	if len(all.GetVersions()) < 2 {
		t.Errorf("unfiltered history returned %d versions, want at least 2", len(all.GetVersions()))
	}
}
