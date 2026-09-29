package admin

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

type refusingQuerier struct{ t *testing.T }

func (q refusingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Fatal("a diff that adds no field to an existing entity queried the database")
	return nil, nil
}

// Plans that add no field pay nothing for the parked-column read.
func TestDiffWithParkedQueriesNothingWithoutAnAddedField(t *testing.T) {
	lowered := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("s.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	prior := lowered(`entity A in x { id bigint primary  v text }`)
	next := lowered(`
entity A in x { id bigint primary  v varchar(20) not null }
entity B in x { id bigint primary  w text not null }
`)
	d, conflicts, err := diffWithParked(context.Background(), refusingQuerier{t}, refusingQuerier{t}, prior, next)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("err %v, conflicts %v", err, conflicts)
	}
	if d.Len() == 0 {
		t.Fatal("the fixture produced no changes, so the test proves nothing")
	}
}
