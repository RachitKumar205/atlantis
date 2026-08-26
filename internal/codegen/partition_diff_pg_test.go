package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// `partition by` added to an entity that already exists must actually isolate.
//
// A table is created without the clause and filled with two tenants' rows. The
// clause is then added and the emitted migration applied. After that, a caller
// bound to one tenant must read only its own rows.
//
// Every other check around this feature detects the absence of the policy —
// VerifyPartitionPolicies at boot, the reload hook, the backfill guard. All
// three exist for a differ that does not read `partition by`, where adding it
// to an existing entity produces an empty plan and the schema claims a
// partition the database has never heard of.
//
// It runs as a NOBYPASSRLS role that owns the table, which is the production
// posture and what makes FORCE do anything. As a superuser the policy is inert
// and this would pass with the whole migration deleted.
func TestPartitionByCanBeAddedToAnExistingEntity(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the partition differ")
	}
	ctx := context.Background()

	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	var haveFn bool
	if err := admin.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'atlantis' AND p.proname = 'current_partition')`).Scan(&haveFn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !haveFn {
		t.Skip("partition migrations not applied to this database")
	}

	cleanup := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pdiff_doc CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY pdiff_tenant`, `DROP ROLE IF EXISTS pdiff_tenant`)
	}
	cleanup()
	t.Cleanup(cleanup)

	const before = `
entity Doc in pdiff {
  id     bigint primary
  tenant varchar(16) not null
  body   text
}
`
	const after = `
entity Doc in pdiff {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`
	oldIR := lower(t, before)
	newIR := lower(t, after)
	AssignProtoNumbers(nil, oldIR)
	AssignProtoNumbers(oldIR, newIR)

	// Create the table WITHOUT the clause, and seed both tenants.
	initial, err := EmitInitial(oldIR)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v\n%s", err, initial.Up)
	}
	if _, err := admin.Exec(ctx, `
INSERT INTO atlantis.pdiff_doc VALUES (1, 'acme', 'acme-secret'), (2, 'globex', 'globex-secret')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The differ has to see the clause appear.
	d := ComputeDiff(oldIR, newIR)
	if d.IsEmpty() {
		t.Fatal("adding `partition by` to an existing entity produced an EMPTY plan. " +
			"No migration, no policy, no output — the schema claims a partition the " +
			"database has never heard of, which is the defect this task exists to fix")
	}
	// CrossCallerBreaking, not Destructive — and the reason is about what this
	// change MEANS, not about what the clients could render at the time.
	//
	// Adding tenant isolation does not destroy anything. It changes what every
	// existing reader can see, which is the definition of breaking, and the
	// rows stay exactly where they are. Destructive would be the wrong record
	// even if it rendered perfectly everywhere.
	//
	// Worth keeping the history, because it is why this line was ever in doubt:
	// when this was written ClassDestructive had no arm in translateClass, so it
	// reached the wire as PLAN_CLASS_UNPARSEABLE, and the CLI's diff and
	// rollback decoders read three buckets where the differ writes four. Both
	// are fixed now. Neither should decide a classification.
	if got := d.HighestClass(); got != ClassCrossCallerBreaking {
		t.Errorf("adding tenant isolation classified %v, want ClassCrossCallerBreaking. "+
			"It must not be auto-applied — after it applies, every request with no "+
			"tenant reads nothing from this table — and it must reach the CLI as a "+
			"class the CLI actually decodes", got)
	}

	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if !strings.Contains(scripts.Up, "CREATE POLICY") ||
		!strings.Contains(scripts.Up, "FORCE ROW LEVEL SECURITY") {
		t.Fatalf("the migration does not create the policy and force it:\n%s", scripts.Up)
	}
	if _, err := admin.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("apply the partition migration: %v\n%s", err, scripts.Up)
	}

	// A restricted role that owns the table, so FORCE applies to it.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE pdiff_tenant LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO pdiff_tenant;
GRANT SELECT, INSERT, UPDATE, DELETE ON atlantis.pdiff_doc TO pdiff_tenant;`)
		return err
	})
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.pdiff_doc OWNER TO pdiff_tenant`); err != nil {
		t.Fatalf("hand the table to the connecting role: %v", err)
	}

	tenantPool, err := pg.New(ctx, pg.DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//pdiff_tenant:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the tenant role: %v", err)
	}
	t.Cleanup(tenantPool.Close)

	readAs := func(t *testing.T, tenant string) []string {
		t.Helper()
		tx, err := tenantPool.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if tenant != "" {
			if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, tenant); err != nil {
				t.Fatalf("bind %s: %v", tenant, err)
			}
		}
		rows, err := tx.Query(ctx, `SELECT body FROM atlantis.pdiff_doc ORDER BY id`)
		if err != nil {
			t.Fatalf("read as %s: %v", tenant, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, b)
		}
		return out
	}

	t.Run("a bound caller reads only its own rows", func(t *testing.T) {
		if got := readAs(t, "acme"); len(got) != 1 || got[0] != "acme-secret" {
			t.Errorf("acme read %v, want exactly [acme-secret]. The migration applied "+
				"and the rows are still shared", got)
		}
		if got := readAs(t, "globex"); len(got) != 1 || got[0] != "globex-secret" {
			t.Errorf("globex read %v, want exactly [globex-secret]", got)
		}
	})

	t.Run("an unbound caller reads nothing", func(t *testing.T) {
		if got := readAs(t, ""); len(got) != 0 {
			t.Errorf("an unbound read returned %v", got)
		}
	})

	// And the boot check must now agree that this table is protected — the
	// check and the differ have to reach the same conclusion, or one of them is
	// describing a database that does not exist.
	t.Run("the boot check agrees the table is protected", func(t *testing.T) {
		problems, err := pg.VerifyPartitionPolicies(ctx, admin, []pg.PartitionedTable{{
			EntityID: "pdiff.Doc", Schema: "atlantis", Table: "pdiff_doc", Column: "tenant",
		}})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if len(problems) != 0 {
			t.Errorf("the differ emitted a policy the boot check rejects: %v", problems)
		}
	})

	// The down migration must restore the previous state exactly.
	t.Run("the down migration removes the isolation", func(t *testing.T) {
		if _, err := admin.Exec(ctx, scripts.Down); err != nil {
			t.Fatalf("apply down: %v\n%s", err, scripts.Down)
		}
		if got := readAs(t, ""); len(got) != 2 {
			t.Errorf("after the down migration an unbound read returned %v, want both "+
				"rows. A down migration that does not restore the previous state "+
				"leaves the table unreadable", got)
		}
	})
}

// The other two directions, which had no test at all.
//
// A review mutated the emitter and the differ ten ways and found every one of
// these survived: `partition_removed` emitting nothing, the differ filing no
// removal, `partition_changed` emitting nothing, and the explicit index drop
// deleted from either the up or the down path. One end-to-end test for one of
// three kinds is one third of a feature.
//
// Removal is the direction that matters most. The comment in the emitter calls
// an unemitted removal "the shape this whole task exists to remove", and
// nothing was checking it.
func TestPartitionRemovedAndChanged(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the partition differ")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	var haveFn bool
	if err := admin.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'atlantis' AND p.proname = 'current_partition')`).Scan(&haveFn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !haveFn {
		t.Skip("partition migrations not applied to this database")
	}

	drop := func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pmove_doc CASCADE`) }
	drop()
	t.Cleanup(drop)

	partitioned := lower(t, `
entity Doc in pmove {
  id     bigint primary
  tenant varchar(16) not null
  org    varchar(16) not null
  body   text
  partition by tenant
}
`)
	plain := lower(t, `
entity Doc in pmove {
  id     bigint primary
  tenant varchar(16) not null
  org    varchar(16) not null
  body   text
}
`)
	moved := lower(t, `
entity Doc in pmove {
  id     bigint primary
  tenant varchar(16) not null
  org    varchar(16) not null
  body   text
  partition by org
}
`)
	for _, ir := range []*dsl.IR{partitioned, plain, moved} {
		AssignProtoNumbers(nil, ir)
	}

	initial, err := EmitInitial(partitioned)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v", err)
	}

	// Where the policy and its index actually point, straight from the catalog.
	// Both flags, not just FORCE. Deleting the DISABLE line survived a test
	// that read only relforcerowsecurity: the table is left with row-level
	// security still ENABLED and no policy, which denies every statement for
	// any role that is not the owner.
	state := func(t *testing.T) (qual string, indexDef string, forced bool) {
		t.Helper()
		_ = admin.QueryRow(ctx, `
SELECT coalesce(max(pg_get_expr(p.polqual, p.polrelid)), '')
  FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'atlantis' AND c.relname = 'pmove_doc'
   AND NOT p.polpermissive`).Scan(&qual)
		_ = admin.QueryRow(ctx, `
SELECT coalesce(max(indexdef), '') FROM pg_indexes
 WHERE schemaname = 'atlantis' AND tablename = 'pmove_doc'
   AND indexname = 'pmove_doc_partition_idx'`).Scan(&indexDef)
		_ = admin.QueryRow(ctx, `
SELECT c.relrowsecurity OR c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'atlantis' AND c.relname = 'pmove_doc'`).Scan(&forced)
		return
	}

	t.Run("removing the clause drops the policy, FORCE and the index", func(t *testing.T) {
		d := ComputeDiff(partitioned, plain)
		if d.IsEmpty() {
			t.Fatal("removing `partition by` produced an EMPTY plan. The table keeps " +
				"enforcing a policy the schema no longer declares, and nothing says so")
		}
		if got := d.HighestClass(); got != ClassCrossCallerBreaking {
			t.Errorf("removing tenant isolation classified %v; after it applies every "+
				"caller reads every tenant's rows", got)
		}
		scripts, err := EmitSQL(partitioned, plain, d)
		if err != nil {
			t.Fatalf("EmitSQL: %v", err)
		}
		if _, err := admin.Exec(ctx, scripts.Up); err != nil {
			t.Fatalf("apply removal: %v\n%s", err, scripts.Up)
		}
		qual, idx, forced := state(t)
		if qual != "" {
			t.Errorf("the policy survived removal: %s", qual)
		}
		if idx != "" {
			t.Errorf("the policy's index survived removal: %s", idx)
		}
		if forced {
			t.Error("row-level security is still enabled or forced after removal. " +
				"With no policy left, that denies every statement for any role that " +
				"is not the table's owner, rather than scoping it")
		}
		// And the down path must put it all back.
		if _, err := admin.Exec(ctx, scripts.Down); err != nil {
			t.Fatalf("apply removal down: %v\n%s", err, scripts.Down)
		}
		qual, idx, forced = state(t)
		if !strings.Contains(qual, "tenant") || !strings.Contains(idx, "tenant") || !forced {
			t.Errorf("the down path did not restore isolation: qual=%q idx=%q forced=%v",
				qual, idx, forced)
		}
	})

	t.Run("moving the clause moves both the policy and the index", func(t *testing.T) {
		d := ComputeDiff(partitioned, moved)
		if d.IsEmpty() {
			t.Fatal("moving `partition by` to another column produced an EMPTY plan")
		}
		scripts, err := EmitSQL(partitioned, moved, d)
		if err != nil {
			t.Fatalf("EmitSQL: %v", err)
		}
		if _, err := admin.Exec(ctx, scripts.Up); err != nil {
			t.Fatalf("apply move: %v\n%s", err, scripts.Up)
		}
		qual, idx, _ := state(t)
		if !strings.Contains(qual, "org") {
			t.Errorf("the policy still compares the old column: %s", qual)
		}
		// The index is the assertion that matters. Both names derive from the
		// TABLE, so without an explicit drop `CREATE INDEX IF NOT EXISTS` finds
		// the old index and leaves it on the old column — every read a
		// sequential scan under a policy that looks correct.
		if !strings.Contains(idx, "org") {
			t.Errorf("the index still covers the old column, so every read under the "+
				"new policy is a sequential scan: %s", idx)
		}
		if _, err := admin.Exec(ctx, scripts.Down); err != nil {
			t.Fatalf("apply move down: %v\n%s", err, scripts.Down)
		}
		qual, idx, _ = state(t)
		if !strings.Contains(qual, "tenant") || !strings.Contains(idx, "tenant") {
			t.Errorf("the down path did not move both back: qual=%q idx=%q", qual, idx)
		}
	})
}

// The change's own Class, not just the bucket it lands in.
//
// Change.Class is persisted inside the diff JSON in atlantis.schema_versions
// and returned to the console and the CLI. Appending to a bucket by hand leaves
// it at its zero value, which is ClassAdditive — so a review read the stored
// plan back and found every partition change recording itself as additive while
// the plan_class column beside it said otherwise.
//
// The durable audit record is the thing someone reads months later to ask what
// changed and who decided it. It described the most consequential change in
// this grammar as routine.
func TestPartitionChangesRecordTheirOwnClass(t *testing.T) {
	base := `
entity Doc in pclass {
  id     bigint primary
  tenant varchar(16) not null
  org    varchar(16) not null
  body   text
`
	with := lower(t, base+"  partition by tenant\n}\n")
	without := lower(t, base+"}\n")
	movedTo := lower(t, base+"  partition by org\n}\n")
	for _, ir := range []*dsl.IR{with, without, movedTo} {
		AssignProtoNumbers(nil, ir)
	}

	for _, tc := range []struct {
		name      string
		from, to  *dsl.IR
		wantKind  ChangeKind
		wantField string
	}{
		{"added", without, with, KindPartitionAdded, "tenant"},
		{"removed", with, without, KindPartitionRemoved, "tenant"},
		{"changed", with, movedTo, KindPartitionChanged, "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ComputeDiff(tc.from, tc.to)
			var found *Change
			for i := range d.Breaking {
				if d.Breaking[i].Kind == tc.wantKind {
					found = &d.Breaking[i]
				}
			}
			if found == nil {
				t.Fatalf("no %s change in the breaking bucket: %+v", tc.wantKind, d)
			}
			if found.Class != ClassCrossCallerBreaking {
				t.Errorf("%s records Class=%v in the persisted diff; the bucket says "+
					"breaking and the change itself says %v. Whoever reads this record "+
					"later is told a tenant-isolation change was routine",
					tc.wantKind, found.Class, found.Class)
			}
			// The Field is rendered into the operator-facing summary as
			// "<entity>/<field>: <detail>". Empty leaves a dangling slash.
			if found.Field != tc.wantField {
				t.Errorf("%s records Field=%q, want %q — the plan summary renders "+
					"%q/%q and reads as a truncated line", tc.wantKind, found.Field,
					tc.wantField, found.EntityID, found.Field)
			}
		})
	}
}

// Changing the tenant column's TYPE must rebuild the policy, not wedge the schema.
//
// The emitted predicate casts atlantis.current_partition() to the column's
// type, so the policy text depends on the type. PostgreSQL refuses to alter a
// column a policy depends on:
//
//	ERROR: cannot alter type of a column used in a policy definition
//
// So without dropping the policy first, the apply rolls back on DDL nobody
// hand-wrote and NO further schema edit gets past it — the schema is wedged
// until someone drops the policy by hand. Found by executing it.
func TestPartitionColumnTypeChangeRebuildsThePolicy(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the partition differ")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.ptype_doc CASCADE`) }
	drop()
	t.Cleanup(drop)

	before := lower(t, `
entity Doc in ptype {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`)
	after := lower(t, `
entity Doc in ptype {
  id     bigint primary
  tenant text not null
  body   text
  partition by tenant
}
`)
	AssignProtoNumbers(nil, before)
	AssignProtoNumbers(before, after)

	initial, err := EmitInitial(before)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO atlantis.ptype_doc VALUES (1,'acme','a'), (2,'globex','g')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	d := ComputeDiff(before, after)
	if d.IsEmpty() {
		t.Fatal("changing the tenant column's type produced an empty plan")
	}
	// Across every bucket, not d.Breaking alone.
	//
	// This test is about whether the REBUILD is planned; the bucket the change
	// lands in is a separate question owned by
	// TestAWideningThatCannotMoveVisibilityIsNotBreaking. varchar(16) -> text
	// leaves the policy predicate byte-identical — both are text-shaped, so
	// neither takes a cast — so the change is additive, and scanning only
	// Breaking reported "the plan does not rebuild the policy" for a plan that
	// rebuilds it perfectly well.
	var sawRebuild bool
	for _, ch := range d.All() {
		if ch.Kind == KindPartitionChanged {
			sawRebuild = true
		}
	}
	if !sawRebuild {
		t.Errorf("the plan does not rebuild the policy. PostgreSQL refuses to alter "+
			"a column a policy depends on, so the apply rolls back and no further "+
			"schema edit gets past it:\n%+v", d)
	}

	scripts, err := EmitSQL(before, after, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	// The real assertion: PostgreSQL accepts it.
	if _, err := admin.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("the migration was rejected by PostgreSQL, which is the wedge this "+
			"exists to prevent: %v\n%s", err, scripts.Up)
	}

	// And isolation still works afterwards, on the new type.
	var qual string
	if err := admin.QueryRow(ctx, `
SELECT coalesce(max(pg_get_expr(p.polqual, p.polrelid)), '')
  FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'atlantis' AND c.relname = 'ptype_doc'
   AND NOT p.polpermissive`).Scan(&qual); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if !strings.Contains(qual, "current_partition") || !strings.Contains(qual, "tenant") {
		t.Errorf("the rebuilt policy does not scope the tenant column: %q", qual)
	}
}

// The class of a tenant-column type change follows the POLICY PREDICATE, not
// the column type.
//
// 0026 seeds PLAN_CLASS_CROSS_CALLER_BREAKING with require_approval=true, so
// the class decides whether an apply stops and waits for a human. Widening
// varchar(16) to varchar(32) on a tenant column rebuilds a policy that is
// byte-identical before and after, because both types are text-shaped and
// neither takes a cast: nothing about what callers can read has moved.
//
// The rebuild happens either way — PostgreSQL refuses to alter a column a
// policy depends on. Only the class moves.
//
// Both directions. Asserting only that a widening is additive would pass
// against a differ that classified everything additive, sending a genuine
// change to what the policy matches through unattended. The uuid case is what
// makes the additive case mean something.
func TestAWideningThatCannotMoveVisibilityIsNotBreaking(t *testing.T) {
	partitioned := func(tenantDecl string) string {
		return `
entity Doc in pclass {
  id     bigint primary
  tenant ` + tenantDecl + ` not null
  body   text
  partition by tenant
}
`
	}

	for _, tc := range []struct {
		name      string
		from, to  string
		wantClass ChangeClass
		why       string
	}{
		{
			name: "varchar widened", from: "varchar(16)", to: "varchar(32)",
			wantClass: ClassAdditive,
			why: "both are text-shaped so neither takes a cast; the policy " +
				"predicate is byte-identical and no caller can observe the change",
		},
		{
			name: "varchar to text", from: "varchar(16)", to: "text",
			wantClass: ClassAdditive,
			why:       "same reason — text is text-shaped and takes no cast either",
		},
		{
			name: "varchar to uuid", from: "varchar(36)", to: "uuid",
			wantClass: ClassCrossCallerBreaking,
			why: "the discriminator gains a ::uuid cast, so the policy compares on " +
				"different terms and the class is earned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := lower(t, partitioned(tc.from))
			after := lower(t, partitioned(tc.to))
			AssignProtoNumbers(nil, before)
			AssignProtoNumbers(before, after)

			var got ChangeClass
			var found bool
			for _, ch := range ComputeDiff(before, after).All() {
				if ch.Kind == KindPartitionChanged {
					got, found = ch.Class, true
				}
			}
			if !found {
				t.Fatalf("no partition change emitted for %s -> %s, so the rebuild is "+
					"not planned at all and PostgreSQL will refuse the ALTER", tc.from, tc.to)
			}
			if got != tc.wantClass {
				t.Errorf("%s -> %s classified %v, want %v: %s",
					tc.from, tc.to, got, tc.wantClass, tc.why)
			}
		})
	}
}

// A bracketed partition change must never reach a class group, whatever its
// class.
//
// emitClass's KindPartitionChanged arm carries `if oldE.PartitionField ==
// newE.PartitionField { break }`, because a same-column rebuild is owned by the
// prologue/epilogue bracket. Emitting it in the group as well creates the policy
// twice, and on the DOWN path the group's copy recreates it before the column is
// reverted, which is the SQLSTATE 0A000 wedge the bracket exists to remove.
//
// Two mechanisms prevent this for a same-column change: the group filter, which
// withoutBracketedPartitionChanges runs on every class group, and the arm guard.
// Removing either alone leaves this test green; it fails only when both go.
//
// They are not duplicates. The filter is class-independent and is the only
// thing covering KindPartitionAdded and KindPartitionRemoved on a bracketed
// entity, which the guard's same-column condition cannot see. The guard is the
// backstop inside the arm. What is asserted here is the outcome, because a test
// tied to either one would pass while the other did the work.
func TestABracketedPartitionChangeReachesNoClassGroup(t *testing.T) {
	before := lower(t, `
entity Doc in pbrk {
  id     bigint primary
  tenant varchar(16) not null
  partition by tenant
}
`)
	after := lower(t, `
entity Doc in pbrk {
  id     bigint primary
  tenant varchar(32) not null
  partition by tenant
}
`)
	AssignProtoNumbers(nil, before)
	AssignProtoNumbers(before, after)
	d := ComputeDiff(before, after)

	if len(partitionRebuilds(d, indexByID(after), indexByID(before))) == 0 {
		t.Fatal("a same-column type change did not produce a bracket, so this test " +
			"is asserting nothing about bracketed changes")
	}

	scripts, err := EmitSQL(before, after, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}

	// Counted in the EMITTED SCRIPT, not by calling
	// withoutBracketedPartitionChanges here.
	//
	// The first version of this test called that helper itself and asserted on
	// what came back. It passed with the ADDITIVE group left unfiltered in
	// EmitSQL — because it was checking that the helper works, which was never
	// in doubt, rather than that EmitSQL uses it on every group. A test that
	// re-implements the call it is meant to be verifying cannot fail for the
	// reason it exists.
	create := "CREATE POLICY " + quoteIdent(partitionPolicyName(&before.Entities[0]))
	for _, s := range []struct {
		name   string
		script string
	}{{"up", scripts.Up}, {"down", scripts.Down}} {
		if n := strings.Count(s.script, create); n != 1 {
			t.Errorf("the %s script creates the boundary policy %d times, want 1. The "+
				"bracket already drops and recreates it; a second copy from a class "+
				"group lands before the column change on the down path — SQLSTATE "+
				"0A000, the wedge the bracket exists to remove:\n%s", s.name, n, s.script)
		}
	}
}
