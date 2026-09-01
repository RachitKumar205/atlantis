package admin

import (
	"context"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/entity"
)

// A checkpoint is read back by a server that has to build a protobuf
// descriptor per entity, and a descriptor needs a field number per column.
// Numbers are not derivable from the IR — they are assigned once and carried,
// so an unnumbered checkpoint is unserveable rather than merely incomplete.
//
// Nothing on the load path repairs one: DecodeJSONIR is a json.Unmarshal, and
// neither boot nor the LISTEN reload assigns numbers. The only place that can
// hold this is the write.
//
// The failure is silent at write time and total at read time. A server that
// loads a checkpoint whose fields all sit at 0 registers no entity at all,
// and reports it as a conflict between two columns that do not conflict.

// adoptNumbersSchema carries four fields because one is not enough to collide:
// protodesc rejects a lone field at 0 as an invalid number, and only a second
// field produces the "conflicting fields" report an operator actually sees.
const adoptNumbersSchema = `
entity Permission in adoptpn {
  id   bigint primary
  name varchar(255) not null
  code varchar(100) not null
  note text
}
`

// createAdoptNumbersTable builds the physical table adopt will introspect. An
// entity with no `table` override lives at atlantis.<namespace>_<name>.
func createAdoptNumbersTable(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	const stmt = `CREATE TABLE IF NOT EXISTS atlantis.adoptpn_permission (
		id   BIGINT PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		code VARCHAR(100) NOT NULL,
		note TEXT
	)`
	if _, err := svc.pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

// adoptNumbers baselines the fixture and returns the checkpoint that was
// written, failing the test if adopt refused.
func adoptNumbers(t *testing.T, svc *Service) *dsl.IR {
	t.Helper()
	ctx := context.Background()
	createAdoptNumbersTable(t, svc)

	resp, err := svc.AdoptBaseline(ctx, &adminpb.AdoptBaselineRequest{
		Caller:    "adoptpn",
		Files:     depScopeFiles("permission.atl", adoptNumbersSchema),
		AdoptedBy: "test",
	})
	if err != nil {
		t.Fatalf("AdoptBaseline: %v", err)
	}
	if !resp.GetCheckpointWritten() {
		t.Fatalf("adopt refused to baseline; drift = %+v", resp.GetDrift())
	}

	ir, err := svc.loadCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if ir == nil {
		t.Fatal("adopt reported a checkpoint was written and none is there")
	}
	return ir
}

// TestAdoptWritesACheckpointWithEveryFieldNumbered covers the write.
//
// adopt persists the introspected IR — the one describing the database rather
// than the declaration — and introspection sets no field numbers. Whatever
// numbers the declaration was given during the diff are on a different value.
func TestAdoptWritesACheckpointWithEveryFieldNumbered(t *testing.T) {
	svc := depScopeService(t)
	ir := adoptNumbers(t, svc)

	var checked int
	for i := range ir.Entities {
		e := &ir.Entities[i]
		seen := map[int]string{}
		for j := range e.Fields {
			f := &e.Fields[j]
			checked++
			if f.ProtoNumber == 0 {
				t.Errorf("%s.%s carries no proto number.\n"+
					"  Every field at 0 collides with every other field at 0, so this "+
					"checkpoint builds no descriptor and the server that loads it "+
					"serves none of its entities.", e.ID(), f.Name)
				continue
			}
			if prev, dup := seen[f.ProtoNumber]; dup {
				t.Errorf("%s: fields %q and %q share proto number %d",
					e.ID(), prev, f.Name, f.ProtoNumber)
			}
			seen[f.ProtoNumber] = f.Name
		}
	}
	if checked == 0 {
		t.Fatal("no fields were checked; the fixture entity is absent from the checkpoint")
	}
}

// TestTheAdoptedCheckpointRegistersItsEntities covers what the numbers are for.
//
// The number check above can pass against a rule that assigns something
// unbuildable. This drives the path a server takes on boot, so the assertion
// is the one the org needs: the checkpoint adopt just wrote can be served.
//
// nil dependencies are enough. Reload builds the descriptor snapshot and swaps
// it in; it reaches no pool, cache or outbox.
func TestTheAdoptedCheckpointRegistersItsEntities(t *testing.T) {
	svc := depScopeService(t)
	ir := adoptNumbers(t, svc)

	srv := entity.NewServer(nil, nil, nil, nil, nil)
	if err := srv.Reload(ir, ""); err != nil {
		t.Fatalf("the checkpoint adopt wrote cannot be registered: %v\n"+
			"  This is what the org's server does on boot, and it is where an "+
			"unnumbered checkpoint stops being a silent write and becomes a "+
			"crash loop.", err)
	}
}
