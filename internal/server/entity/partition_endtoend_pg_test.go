package entity

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The whole chain, against a real database, as a role row-level security
// applies to.
//
// # Why this exists
//
// Everything else testing `partition by` proves one link. A fake pool proves
// the dispatcher binds. A SQL test proves the policy filters. A source-level
// test proves a call site exists. An interceptor test proves a header becomes a
// context value. None of them cross a boundary.
//
// Seven defects were introduced into this feature while fixing other defects in
// it, and EVERY ONE sat at a boundary: the dispatcher bound a value nothing
// checked; the interceptor supplied a value nothing consumed; a validator
// walked a tree its pair did not; a read-back dropped a filter another rule
// depended on. Each link was tested. The chain was not.
//
// So this test asserts the only property that matters, in the terms a caller
// would state it: **a request carrying tenant A's header can read tenant A's
// rows and cannot read tenant B's.** It runs the real interceptor, the real
// dispatcher, the real emitted DDL, and a real restricted role.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/server/entity/ -run PartitionEndToEnd -v
func TestPartitionEndToEnd(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the full chain")
	}
	ctx := context.Background()

	admin, err := pg.New(ctx, pg.DefaultConfig(adminDSN))
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
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

	// Catalog writes go through pgcatalog, which serialises them on a
	// connection of its own.
	//
	// This test used to take the lock through the pgxpool above. A session-level
	// advisory lock belongs to the backend that took it, so lock and unlock
	// could land on different pooled connections — a review reproduced the lock
	// on backend 18987 and the unlock on 18990 returning false, leaving it held.
	// The next caller then blocks with no timeout and the run hangs.
	cleanup := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.e2e_doc CASCADE`)
		pgcatalog.Exec(t, adminDSN,
			`DROP OWNED BY e2e_tenant`,
			`DROP ROLE IF EXISTS e2e_tenant`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// The real emitted DDL, not a hand-written table: a policy this test wrote
	// itself would prove nothing about the policy atlantis emits.
	f, err := dsl.Parse("e2e.atl", []byte(`
entity Doc in e2e {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)

	scripts, err := codegen.EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("apply emitted DDL: %v\n%s", err, scripts.Up)
	}

	// Seed both tenants. FORCE applies to the owner too, so even this binds —
	// one transaction each, which is also the shape the server writes in.
	for _, seed := range []struct {
		id           int64
		tenant, body string
	}{
		{1, "acme", "acme-secret"},
		{2, "globex", "globex-secret"},
	} {
		tx, err := admin.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin seed %s: %v", seed.tenant, err)
		}
		if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, seed.tenant); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatalf("bind seed %s: %v", seed.tenant, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO atlantis.e2e_doc VALUES ($1, $2, $3)`,
			seed.id, seed.tenant, seed.body); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatalf("seed %s: %v", seed.tenant, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit seed %s: %v", seed.tenant, err)
		}
	}

	pgcatalog.Do(t, adminDSN, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE e2e_tenant LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO e2e_tenant;
GRANT SELECT, INSERT, UPDATE, DELETE ON atlantis.e2e_doc TO e2e_tenant;`)
		return err
	})

	// The connecting role OWNS the table, which is the production shape:
	// atlantis creates the tables it serves and connects as the role that owns
	// them. It also makes FORCE load-bearing — ENABLE alone exempts the owner,
	// so a fixture where some other role owns the table passes with FORCE
	// removed from the emitted DDL. Found by mutating the emitter and watching
	// this test stay green.
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.e2e_doc OWNER TO e2e_tenant`); err != nil {
		t.Fatalf("hand the table to the connecting role: %v", err)
	}

	// The server connects as the restricted role, which is the deployment
	// posture the whole design assumes. As a superuser this test would pass
	// with the policy deleted.
	tenantPool, err := pg.New(ctx, pg.DefaultConfig(
		strings.Replace(adminDSN, "//atlantis:atlantis@", "//e2e_tenant:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the tenant role: %v", err)
	}
	t.Cleanup(tenantPool.Close)

	srv := NewServer(tenantPool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "e2e"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["e2e.Doc"]
	if meta == nil || !meta.partitioned {
		t.Fatal("e2e.Doc is missing or not marked partitioned")
	}

	// One request: metadata in, rows out, through the real interceptor.
	query := func(t *testing.T, md metadata.MD) ([]string, error) {
		t.Helper()
		reqCtx := metadata.NewIncomingContext(context.Background(), md)

		var bodies []string
		_, err := interceptors.NewPartition()(reqCtx, nil,
			&grpc.UnaryServerInfo{FullMethod: "/e2e.DocService/QueryDoc"},
			func(ctx context.Context, _ any) (any, error) {
				req := dynamicpb.NewMessage(meta.queryRequestDesc)
				resp, err := srv.handleQuery(ctx, meta, func(dst any) error {
					proto.Reset(dst.(proto.Message))
					proto.Merge(dst.(proto.Message), req)
					return nil
				})
				if err != nil {
					return nil, err
				}
				list := resp.(*dynamicpb.Message).
					Get(meta.queryResponseDesc.Fields().ByName("entities")).List()
				bodyFD := meta.msgDesc.Fields().ByName("body")
				for i := 0; i < list.Len(); i++ {
					bodies = append(bodies, list.Get(i).Message().Get(bodyFD).String())
				}
				return nil, nil
			})
		return bodies, err
	}

	t.Run("a tenant reads its own rows and only its own", func(t *testing.T) {
		got, err := query(t, metadata.Pairs(interceptors.PartitionHeader, "acme"))
		if err != nil {
			t.Fatalf("query as acme: %v", err)
		}
		if len(got) != 1 || got[0] != "acme-secret" {
			t.Fatalf("acme read %v, want exactly [acme-secret]. Anything else is a "+
				"cross-tenant read through the real request path", got)
		}
	})

	t.Run("the other tenant sees only its own", func(t *testing.T) {
		got, err := query(t, metadata.Pairs(interceptors.PartitionHeader, "globex"))
		if err != nil {
			t.Fatalf("query as globex: %v", err)
		}
		if len(got) != 1 || got[0] != "globex-secret" {
			t.Fatalf("globex read %v, want exactly [globex-secret]", got)
		}
	})

	t.Run("no header reads nothing and is refused", func(t *testing.T) {
		got, err := query(t, metadata.MD{})
		if err == nil {
			t.Error("a request with no tenant header was served")
		}
		for _, b := range got {
			t.Errorf("an unbound request returned %q", b)
		}
	})

	t.Run("a tenant with no rows reads nothing", func(t *testing.T) {
		got, err := query(t, metadata.Pairs(interceptors.PartitionHeader, "nobody"))
		if err != nil {
			t.Fatalf("query as nobody: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a tenant with no rows read %v", got)
		}
	})

	// And the fixture has to be capable of failing. If the policy were absent
	// the reads above would return both rows, so confirm both rows are really
	// there when nothing constrains the reader.
	t.Run("both rows exist, so the reads above are constrained not empty", func(t *testing.T) {
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM atlantis.e2e_doc`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Fatalf("the table holds %d rows, want 2. With fewer, a scoped read "+
				"and a broken policy return the same thing", n)
		}
	})
}
