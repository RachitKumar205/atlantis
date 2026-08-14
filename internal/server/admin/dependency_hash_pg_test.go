package admin

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The end-to-end half of the per-caller staleness token.
//
// dependency_hash_test.go pins what the token covers. These pin that the token
// is what the RPCs actually consult, which is a separate claim and was very
// nearly wrong: ApplyMigration compares a plan id BEFORE it compares the
// checkpoint token, and the plan id used to be hashed over the entire prior
// checkpoint. Narrowing only the token would have left every unrelated apply
// refused exactly as before, by the earlier of the two checks, with the new
// scoping working perfectly and doing nothing. A unit test on the hash cannot
// see that; these can.
//
// Env-gated like the other live-PG tests here:
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/server/admin/ -run Plan -v

// depScopeService builds a Service over a private, freshly migrated database.
//
// Private because these tests drive real applies: they write atlantis.
// ir_checkpoint, which is one row for the whole deployment, and they run the
// DDL their own schemas emit. Sharing the developer's database would leave the
// checkpoint holding test entities, and every later `tide plan` against it
// would propose dropping them.
func depScopeService(t *testing.T) *Service {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise plan staleness across callers")
	}

	const dbName = "atlantis_depscope"
	drop := func() {
		pgcatalog.Exec(t, adminDSN,
			// Terminate first: DROP DATABASE refuses while any connection to it
			// remains, and a pool closed a moment ago may still be draining.
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '`+dbName+`'`,
			`DROP DATABASE IF EXISTS `+dbName)
	}
	drop()
	t.Cleanup(drop)
	pgcatalog.Exec(t, adminDSN, `CREATE DATABASE `+dbName)

	dsn := adminDSN
	i := strings.LastIndex(dsn, "/atlantis?")
	if i < 0 {
		t.Fatalf("cannot derive a DSN for %s from %q — expected it to end in /atlantis?...", dbName, adminDSN)
	}
	dsn = dsn[:i] + "/" + dbName + dsn[i+len("/atlantis"):]

	// Extensions are per-database. A fresh one has none, and the migrations
	// reference all three.
	pgcatalog.Exec(t, dsn,
		`CREATE EXTENSION IF NOT EXISTS citext`,
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.Run(dsn, "../../../migrations", quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	t.Cleanup(pool.Close)

	// AllowApplyMutation, or requireMutablePlane refuses every apply below with
	// a message about tidectl that has nothing to do with what is under test.
	return New(pool, Config{AllowApplyMutation: true})
}

func depScopeFiles(path, src string) []*adminpb.SubmittedFile {
	return []*adminpb.SubmittedFile{{Path: path, Content: []byte(src)}}
}

// plan runs PlanSchema and fails the test on a plan that cannot be applied,
// so a broken fixture reports itself rather than surfacing later as a
// confusing staleness result.
func depScopePlan(t *testing.T, svc *Service, caller, path, src string) *adminpb.PlanSchemaResponse {
	t.Helper()
	resp, err := svc.PlanSchema(context.Background(), &adminpb.PlanSchemaRequest{
		Caller: caller, Files: depScopeFiles(path, src),
	})
	if err != nil {
		t.Fatalf("PlanSchema(%s): %v", caller, err)
	}
	if len(resp.GetParseErrors()) > 0 || len(resp.GetCustomSqlErrors()) > 0 {
		t.Fatalf("PlanSchema(%s) rejected the fixture: parse=%v custom=%v",
			caller, resp.GetParseErrors(), resp.GetCustomSqlErrors())
	}
	return resp
}

func depScopeApply(t *testing.T, svc *Service, caller, path, src string, plan *adminpb.PlanSchemaResponse) error {
	t.Helper()
	_, err := svc.ApplyMigration(context.Background(), &adminpb.ApplyMigrationRequest{
		Caller:         caller,
		PlanId:         plan.GetPlanId(),
		Files:          depScopeFiles(path, src),
		CheckpointHash: plan.GetCheckpointHash(),
	})
	return err
}

// planAndApply is the ordinary path: plan, then immediately apply that plan.
func depScopePlanAndApply(t *testing.T, svc *Service, caller, path, src string) {
	t.Helper()
	p := depScopePlan(t, svc, caller, path, src)
	if err := depScopeApply(t, svc, caller, path, src, p); err != nil {
		t.Fatalf("ApplyMigration(%s): %v", caller, err)
	}
}

const (
	depScopeAlphaV1 = `
entity Order in dsalpha {
  id    bigint primary
  total numeric(10, 2) not null
}
`
	depScopeAlphaV2 = `
entity Order in dsalpha {
  id    bigint primary
  total numeric(10, 2) not null
  note  text
}
`
	depScopeBetaV1 = `
entity Gadget in dsbeta {
  id   bigint primary
  name text
}
`
	depScopeBetaV2 = `
entity Gadget in dsbeta {
  id     bigint primary
  name   text
  colour text
}
`
)

// The headline property, and the one that failed before this change.
func TestUnrelatedCallerApplyDoesNotInvalidateAPlan(t *testing.T) {
	svc := depScopeService(t)

	depScopePlanAndApply(t, svc, "dsalpha", "order.atl", depScopeAlphaV1)
	depScopePlanAndApply(t, svc, "dsbeta", "gadget.atl", depScopeBetaV1)

	// dsalpha plans a column of its own and holds the plan.
	held := depScopePlan(t, svc, "dsalpha", "order.atl", depScopeAlphaV2)

	// dsbeta deploys something entirely unrelated in the meantime.
	depScopePlanAndApply(t, svc, "dsbeta", "gadget.atl", depScopeBetaV2)

	if err := depScopeApply(t, svc, "dsalpha", "order.atl", depScopeAlphaV2, held); err != nil {
		t.Fatalf("dsalpha's plan was invalidated by dsbeta's unrelated apply: %v\n\n"+
			"The two schemas share nothing. Before the staleness token was scoped "+
			"per caller this refused, and every team on a shared atlantis re-planned "+
			"whenever any other team deployed.", err)
	}
}

// The counterweight, at the RPC level: scoping must not have made the token so
// narrow that a plan built on a stale picture of a dependency still applies.
func TestApplyIsRejectedWhenADependencyMoves(t *testing.T) {
	svc := depScopeService(t)

	const betaThingV1 = `
entity Thing in dsbeta {
  id    varchar(8) primary
  label text
}
`
	const betaThingV2 = `
entity Thing in dsbeta {
  id     varchar(8) primary
  label  text
  weight int
}
`
	const alphaRefsThingV1 = `
entity Order in dsalpha {
  id       bigint primary
  thing_id varchar(8) not null references dsbeta.Thing.id
}
`
	const alphaRefsThingV2 = `
entity Order in dsalpha {
  id       bigint primary
  thing_id varchar(8) not null references dsbeta.Thing.id
  note     text
}
`
	depScopePlanAndApply(t, svc, "dsbeta", "thing.atl", betaThingV1)
	depScopePlanAndApply(t, svc, "dsalpha", "order.atl", alphaRefsThingV1)

	held := depScopePlan(t, svc, "dsalpha", "order.atl", alphaRefsThingV2)

	// dsbeta changes the very entity dsalpha holds a foreign key into.
	depScopePlanAndApply(t, svc, "dsbeta", "thing.atl", betaThingV2)

	err := depScopeApply(t, svc, "dsalpha", "order.atl", alphaRefsThingV2, held)
	if err == nil {
		t.Fatal("dsalpha applied a plan made before dsbeta changed the entity it " +
			"references. A token narrow enough to permit this protects nothing.")
	}
	if !strings.Contains(err.Error(), "stale") && !strings.Contains(err.Error(), "has moved") {
		t.Errorf("the apply was refused, but by something other than the staleness "+
			"guards, so this proves nothing about them: %v", err)
	}
}

// The ordinary compare-and-swap case, which scoping must leave intact: two
// deploys of the same caller racing.
func TestOwnChangeInvalidatesTheCallersOwnPlan(t *testing.T) {
	svc := depScopeService(t)

	depScopePlanAndApply(t, svc, "dsalpha", "order.atl", depScopeAlphaV1)

	// One deploy plans a column.
	held := depScopePlan(t, svc, "dsalpha", "order.atl", depScopeAlphaV2)

	// A second deploy of the same caller lands a different column first. Same
	// file path, so the plan id can only move because the token did.
	const alphaOther = `
entity Order in dsalpha {
  id     bigint primary
  total  numeric(10, 2) not null
  status text
}
`
	depScopePlanAndApply(t, svc, "dsalpha", "order.atl", alphaOther)

	if err := depScopeApply(t, svc, "dsalpha", "order.atl", depScopeAlphaV2, held); err == nil {
		t.Fatal("the first deploy's plan still applied after the second deploy " +
			"landed. It was computed against an Order that no longer exists, and " +
			"applying it drops the column the second deploy added.")
	}
}
