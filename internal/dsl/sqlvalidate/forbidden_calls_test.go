package sqlvalidate

import (
	"strings"
	"testing"
)

// The tenant discriminator is a run-time parameter, and PostgreSQL will not
// lock a custom one: `REVOKE SET ON PARAMETER "atlantis.tenant" FROM PUBLIC`
// does not create so much as a pg_parameter_acl row for a placeholder GUC,
// verified on 17.8. This validator is therefore the thing standing between a
// query body and another tenant's rows.
//
// Each case below hides the call somewhere different. That is the point: a
// parse-tree walk written by hand is a list of places to look, and anything it
// forgets is not a missing warning, it is a cross-tenant read. The walk in
// forbidden_calls.go visits the protobuf message graph, so it descends into all
// of these without knowing what any of them are.
func TestValidateCustomQuery_RejectsSetConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			// The vector that reached a live database. One SELECT, one declared
			// table, every existing check satisfied — and on 17.8 it returns the
			// other tenant's rows.
			name: "derived table in the FROM clause",
			sql: `SELECT o.id, o.consumer_id, o.name
			        FROM (SELECT set_config('atlantis.tenant','victim',true)) s,
			             consumer_saved_outfit o
			       WHERE o.consumer_id = $consumer_id`,
		},
		{
			name: "target list",
			sql: `SELECT set_config('atlantis.tenant','victim',true), id, consumer_id, name
			        FROM consumer_saved_outfit WHERE consumer_id = $consumer_id`,
		},
		{
			name: "WHERE clause",
			sql: `SELECT id, consumer_id, name FROM consumer_saved_outfit
			       WHERE set_config('atlantis.tenant','victim',true) IS NOT NULL
			         AND consumer_id = $consumer_id`,
		},
		{
			name: "CTE body",
			sql: `WITH s AS (SELECT set_config('atlantis.tenant','victim',true))
			      SELECT id, consumer_id, name FROM consumer_saved_outfit
			       WHERE consumer_id = $consumer_id`,
		},
		{
			name: "nested in another call's arguments",
			sql: `SELECT id, consumer_id,
			             coalesce(nullif(set_config('atlantis.tenant','victim',true),''),'x') AS name
			        FROM consumer_saved_outfit WHERE consumer_id = $consumer_id`,
		},
		{
			name: "CASE expression",
			sql: `SELECT id, consumer_id,
			             CASE WHEN true THEN set_config('atlantis.tenant','victim',true) END AS name
			        FROM consumer_saved_outfit WHERE consumer_id = $consumer_id`,
		},
		{
			name: "ORDER BY",
			sql: `SELECT id, consumer_id, name FROM consumer_saved_outfit
			       WHERE consumer_id = $consumer_id
			       ORDER BY set_config('atlantis.tenant','victim',true)`,
		},
		{
			// Matching only the qualified spelling would be defeated by the bare
			// one, and vice versa.
			name: "schema-qualified spelling",
			sql: `SELECT id, consumer_id, pg_catalog.set_config('atlantis.tenant','victim',true) AS name
			        FROM consumer_saved_outfit WHERE consumer_id = $consumer_id`,
		},
		{
			name: "arm of a UNION",
			sql: `SELECT id, consumer_id, name FROM consumer_saved_outfit
			       WHERE consumer_id = $consumer_id
			      UNION ALL
			      SELECT 1, 1, set_config('atlantis.tenant','victim',true)`,
		},
		{
			// The wrapper, on the surface its own documentation describes it
			// being attacked on. Every other case here uses set_config, so
			// deleting the set_partition entry from forbiddenFunctions left
			// this whole file green.
			name: "the setter itself, in a derived table",
			sql: `SELECT o.id, o.consumer_id, o.name
			        FROM (SELECT atlantis.set_partition('victim')) s,
			             consumer_saved_outfit o
			       WHERE o.consumer_id = $consumer_id`,
		},
		{
			name: "the setter itself, unqualified",
			sql: `SELECT id, consumer_id, name FROM consumer_saved_outfit
			       WHERE consumer_id = $consumer_id
			         AND set_partition('victim') IS NOT NULL`,
		},
		{
			name: "lateral join",
			sql: `SELECT o.id, o.consumer_id, o.name
			        FROM consumer_saved_outfit o,
			             LATERAL (SELECT set_config('atlantis.tenant','victim',true)) s
			       WHERE o.consumer_id = $consumer_id`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := lowerWithCustom(t, `
query Probe for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    `+tc.sql+`
  }
}
`)
			err := ValidateCustomQuery(ir, &ir.Queries[0])
			if err == nil {
				t.Fatalf("the validator accepted a query body that calls set_config. "+
					"Against PostgreSQL this rebinds atlantis.tenant mid-transaction, "+
					"and every partition-by policy then compares against the value "+
					"the body chose:\n%s", tc.sql)
			}
			if !strings.Contains(err.Error(), "set_config") &&
				!strings.Contains(err.Error(), "set_partition") {
				t.Errorf("rejected, but not for calling a forbidden function, so "+
					"the author cannot tell what to change: %v", err)
			}
		})
	}
}

// Procedure steps run caller SQL in the same transaction as the reads the
// policy constrains, so they need the same gate. They take a different path
// through this package — ValidateCustomProcedure, ModeProcedureStep — and a
// check wired into one and not the other is the shape of bug this whole
// session has been finding.
func TestValidateCustomProcedure_RejectsSetConfig(t *testing.T) {
	ir := lowerWithCustom(t, `
procedure Probe for SavedOutfit {
  input { id: bigint }
  steps {
    sql touches(SavedOutfit) {
      UPDATE consumer_saved_outfit
         SET name = set_config('atlantis.tenant','victim',true)
       WHERE id = $id
    }
  }
}
`)
	err := ValidateCustomProcedure(ir, &ir.Procedures[0])
	if err == nil {
		t.Fatal("the validator accepted a procedure step that calls set_config, " +
			"so a procedure can rebind the tenant its own statements are " +
			"constrained by")
	}
	if !strings.Contains(err.Error(), "set_config") {
		t.Errorf("rejected, but not for calling set_config: %v", err)
	}
}

// And the gate must not reject ordinary SQL, or it is a syntax restriction
// wearing a security argument. current_setting reads a parameter and does not
// change one; only the write is forbidden.
func TestValidateCustomQuery_AcceptsOrdinarySQL(t *testing.T) {
	for _, sql := range []string{
		`SELECT id, consumer_id, name FROM consumer_saved_outfit WHERE consumer_id = $consumer_id`,
		`SELECT id, consumer_id, coalesce(name, 'untitled') AS name FROM consumer_saved_outfit
		  WHERE consumer_id = $consumer_id`,
		`WITH t AS (SELECT id, consumer_id, name FROM consumer_saved_outfit WHERE consumer_id = $consumer_id)
		 SELECT id, consumer_id, name FROM t`,
		`SELECT id, consumer_id, name FROM consumer_saved_outfit
		  WHERE consumer_id = $consumer_id
		    AND current_setting('atlantis.tenant', true) IS NOT NULL`,
	} {
		ir := lowerWithCustom(t, `
query Probe for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    `+sql+`
  }
}
`)
		if err := ValidateCustomQuery(ir, &ir.Queries[0]); err != nil {
			t.Errorf("rejected an ordinary query body:\n%s\n  %v", sql, err)
		}
	}
}
