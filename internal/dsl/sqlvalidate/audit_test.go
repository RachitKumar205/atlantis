package sqlvalidate

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The audit's whole reason for existing is content the gate never saw.
//
// Every fixture below goes through dsl.Parse and dsl.Lower rather than being
// hand-built, because the gate has to hold against the shape the parser
// actually produces. A hand-built dsl.Entity proves the walk works on a struct
// somebody wrote to match the walk; it does not prove `check "..."` lands
// where the walk looks.
func lowerSchema(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("audit.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}

func TestAuditForbiddenCalls_FindsStoredSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{
			// The vector an adversarial review used to defeat the whole design:
			// a CHECK is emitted verbatim into DDL and PostgreSQL does not
			// require it to be IMMUTABLE, so it fires on every write.
			name: "entity-level check",
			src: `
entity Doc in shop {
  id     bigint primary
  tenant text not null
  body   text
  check "set_config('atlantis.tenant','victim',true) IS NOT NULL OR body IS NOT NULL"
  partition by tenant
}`,
		},
		{
			name: "field-level check",
			src: `
entity Doc in shop {
  id     bigint primary
  tenant text not null
  body   text check "set_config('atlantis.tenant','victim',true) IS NOT NULL"
  partition by tenant
}`,
		},
		{
			// The wrapper, not the primitive. It is GRANTed to PUBLIC because
			// the server binds as the same role that runs caller SQL.
			name: "the setter itself",
			src: `
entity Doc in shop {
  id     bigint primary
  tenant text not null
  body   text
  check "atlantis.set_partition('victim') IS NOT NULL OR body IS NOT NULL"
  partition by tenant
}`,
		},
		{
			name: "stored query body",
			src: `
entity Doc in shop {
  id     bigint primary
  tenant text not null
  body   text
}

query Probe for Doc {
  input { id: bigint }
  output as Doc
  sql touches(Doc) {
    SELECT d.id, d.tenant, d.body
      FROM (SELECT set_config('atlantis.tenant','victim',true)) s, shop_doc d
     WHERE d.id = $id
  }
}`,
		},
		{
			name: "stored procedure step",
			src: `
entity Doc in shop {
  id     bigint primary
  tenant text not null
  body   text
}

procedure Probe for Doc {
  input { id: bigint }
  steps {
    sql touches(Doc) {
      UPDATE shop_doc SET body = set_config('atlantis.tenant','victim',true) WHERE id = $id
    }
  }
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := lowerSchema(t, tc.src)
			findings := AuditForbiddenCalls(ir)
			if len(findings) == 0 {
				t.Fatalf("the audit found nothing in a stored schema that can rebind "+
					"the tenant. A server booting on this checkpoint serves it:\n%s", tc.src)
			}
			joined := findings[0].Error()
			if !strings.Contains(joined, "set_config") && !strings.Contains(joined, "set_partition") {
				t.Errorf("reported something other than the forbidden call: %v", joined)
			}
		})
	}
}

// And an ordinary schema must audit clean, or the server refuses to boot on
// every deployment that has done nothing wrong.
func TestAuditForbiddenCalls_QuietOnOrdinarySchemas(t *testing.T) {
	ir := lowerSchema(t, `
entity Doc in shop {
  id       bigint primary
  tenant   text not null
  status   text not null check "status IN ('draft','active')"
  quantity int not null
  check "quantity > 0"
  partition by tenant
}

query ByStatus for Doc {
  input { status: text }
  output as Doc
  sql touches(Doc) {
    SELECT id, tenant, status, quantity FROM shop_doc WHERE status = $status
  }
}

procedure Archive for Doc {
  input { id: bigint }
  steps {
    sql touches(Doc) {
      UPDATE shop_doc SET status = 'draft' WHERE id = $id
    }
  }
}`)
	if findings := AuditForbiddenCalls(ir); len(findings) > 0 {
		t.Errorf("an ordinary schema failed the audit, which would refuse a boot: %v", findings)
	}
	if findings := AuditForbiddenCalls(nil); findings != nil {
		t.Errorf("a nil IR produced findings: %v", findings)
	}
	if findings := AuditForbiddenCalls(&dsl.IR{}); len(findings) > 0 {
		t.Errorf("an empty IR produced findings: %v", findings)
	}
}
