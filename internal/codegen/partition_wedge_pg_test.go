package codegen

import (
	"context"
	"os"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// Nothing may touch the old partition column while its policy stands.
//
// PostgreSQL refuses to alter a column any policy depends on. The bracket was
// first written for the one shape that had been reproduced — a type change on
// an unchanged partition column — and a review executed two more that fall
// outside it and produce the identical SQLSTATE 0A000:
//
//	remove `partition by` + widen the same column
//	move `partition by` to another column + widen the OLD one
//
// Selecting on the kind of partition change was picking the symptom. The
// invariant is about the column.
func TestOldPartitionColumnCanAlwaysBeAltered(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the rebuild bracket")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	const head = `
entity Doc in pwedge {
  id     bigint primary
  tenant %s not null
  org    varchar(16) not null
  body   text
`
	for _, tc := range []struct {
		name, from, to, why string
	}{
		{
			name: "remove the clause and widen the same column",
			from: `  partition by tenant
}`,
			to: `}`,
			why: "the policy is dropped by the removal, but the ALTER lands in an " +
				"earlier class group than the drop",
		},
		{
			name: "move the clause and widen the OLD column",
			from: `  partition by tenant
}`,
			to: `  partition by org
}`,
			why: "the move's own drop lands in BREAKING, after the ALTER",
		},
		{
			name: "keep the clause and widen the column",
			from: `  partition by tenant
}`,
			to: `  partition by tenant
}`,
			why: "the case the bracket was originally written for; must not regress",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drop := func() {
				_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pwedge_doc CASCADE`)
			}
			drop()
			t.Cleanup(drop)

			before := lower(t, sprintfSchema(head, "varchar(16)")+tc.from)
			after := lower(t, sprintfSchema(head, "text")+tc.to)
			for _, ir := range []*dsl.IR{before, after} {
				AssignProtoNumbers(nil, ir)
			}

			initial, err := EmitInitial(before)
			if err != nil {
				t.Fatalf("EmitInitial: %v", err)
			}
			if _, err := admin.Exec(ctx, initial.Up); err != nil {
				t.Fatalf("apply initial: %v", err)
			}
			scripts, err := EmitSQL(before, after, ComputeDiff(before, after))
			if err != nil {
				t.Fatalf("EmitSQL: %v", err)
			}
			if _, err := admin.Exec(ctx, scripts.Up); err != nil {
				t.Fatalf("PostgreSQL rejected the migration — the schema is wedged "+
					"until someone drops the policy by hand (%s): %v\n%s",
					tc.why, err, scripts.Up)
			}
			if _, err := admin.Exec(ctx, scripts.Down); err != nil {
				t.Fatalf("the down script was rejected: %v\n%s", err, scripts.Down)
			}
		})
	}
}

func sprintfSchema(tmpl, typ string) string {
	out := ""
	for i := 0; i < len(tmpl); i++ {
		if i+1 < len(tmpl) && tmpl[i] == '%' && tmpl[i+1] == 's' {
			out += typ
			i++
			continue
		}
		out += string(tmpl[i])
	}
	return out
}
