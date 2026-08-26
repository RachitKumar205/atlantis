package sqlvalidate

import (
	"strings"
	"testing"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// The shapes the hand-written walk could not see.
//
// checkTouchesCoverage uses collectTableRefs to decide whether an author's
// `touches()` list is complete, and internal/server/entity derives from
// `touches()` whether a custom query runs with the caller's tenant bound. A
// table this walk misses is therefore not a stale cache generation — it is a
// query against a partitioned entity running with no tenant bound.
//
// Measured against pg_query_go v6.2.2 by restoring the hand-written walk and
// running these cases: it found the table in 2 and missed 14. All 16 are below.
func TestCollectTableRefsFindsEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"plain FROM", `SELECT id FROM orders`},
		{"IN subquery", `SELECT id FROM products WHERE id IN (SELECT product_id FROM orders)`},
		{"AND with IN subquery", `SELECT id FROM products WHERE active AND id IN (SELECT product_id FROM orders)`},
		{"OR", `SELECT id FROM products WHERE active OR id IN (SELECT product_id FROM orders)`},
		{"NOT", `SELECT id FROM products WHERE NOT (id IN (SELECT product_id FROM orders))`},
		{"scalar subquery in WHERE", `SELECT id FROM products WHERE id = (SELECT max(product_id) FROM orders)`},
		{"EXISTS behind AND", `SELECT id FROM products p WHERE p.active AND EXISTS (SELECT 1 FROM orders o WHERE o.product_id = p.id)`},
		{"inside a function call", `SELECT coalesce((SELECT count(*) FROM orders), 0) FROM products`},
		{"CASE expression", `SELECT CASE WHEN true THEN (SELECT count(*) FROM orders) ELSE 0 END FROM products`},
		{"ORDER BY", `SELECT id FROM products ORDER BY (SELECT count(*) FROM orders)`},
		{"GROUP BY", `SELECT count(*) FROM products GROUP BY (SELECT count(*) FROM orders)`},
		{"LIMIT", `SELECT id FROM products LIMIT (SELECT count(*) FROM orders)`},
		{"UPDATE SET subquery", `UPDATE products SET total = (SELECT count(*) FROM orders)`},
		{"INSERT VALUES subquery", `INSERT INTO products (total) VALUES ((SELECT count(*) FROM orders))`},
		{"DELETE behind AND", `DELETE FROM products WHERE active AND id IN (SELECT product_id FROM orders)`},
		{"HAVING", `SELECT count(*) FROM products HAVING count(*) > (SELECT count(*) FROM orders)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := pg.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var refs []string
			for _, raw := range tree.Stmts {
				refs = append(refs, collectTableRefs(raw.GetStmt())...)
			}
			found := false
			for _, r := range refs {
				if strings.Contains(r, "orders") {
					found = true
				}
			}
			if !found {
				t.Errorf("did not find \"orders\" in:\n  %s\nrefs: %v\n\n"+
					"A table this walk misses is a table missing from touches(), "+
					"and a custom query touching a partitioned entity then runs "+
					"with no tenant bound", tc.sql, refs)
			}
		})
	}
}

// Nodes that hold a RangeVar which is not a table reference.
//
// The exhaustive walk collected these when it first replaced the hand-written
// one, and `SELECT ... FOR UPDATE OF o` — the canonical read-modify-write
// procedure step — started failing with `unknown table "o"`. A schema that
// applied yesterday failed today, and the obvious fix was to delete the lock.
func TestCollectTableRefsSkipsAliasesAndIntoTargets(t *testing.T) {
	for _, tc := range []struct {
		name, sql, mustNot string
	}{
		{"FOR UPDATE OF alias", `SELECT id FROM orders o WHERE o.id = 1 FOR UPDATE OF o`, "o"},
		{"FOR SHARE OF alias", `SELECT id FROM orders o FOR SHARE OF o`, "o"},
		{"SELECT INTO target", `SELECT id INTO tmp_copy FROM orders`, "tmp_copy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := pg.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var refs []string
			for _, raw := range tree.Stmts {
				refs = append(refs, collectTableRefs(raw.GetStmt())...)
			}
			for _, r := range refs {
				if r == tc.mustNot || strings.HasSuffix(r, "."+tc.mustNot) {
					t.Errorf("collected %q from:\n  %s\nrefs: %v\n\nThat is an alias "+
						"or a CREATE target, not a table reference. Reporting it "+
						"fails a schema that is correct", r, tc.sql, refs)
				}
			}
			// And the real table is still found.
			found := false
			for _, r := range refs {
				if strings.Contains(r, "orders") {
					found = true
				}
			}
			if !found {
				t.Errorf("stopped finding the real table: %v", refs)
			}
		})
	}
}

// A CTE referenced from a position the old walk never reached.
//
// collectTableRefs became reflective and collectCTENames did not, so the
// reference was collected as a table while the name was not collected as a CTE.
// Validation then reported `unknown table "recent"` on SQL that applied before
// the change. Two walks over one tree have to descend equally far.
func TestCTENamesAreFoundWhereverTableRefsAre(t *testing.T) {
	const sql = `SELECT id FROM products WHERE active AND id IN
	  (WITH recent AS (SELECT product_id FROM orders) SELECT product_id FROM recent)`

	tree, err := pg.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stmt := tree.Stmts[0].GetStmt()

	refs := collectTableRefs(stmt)
	ctes := collectCTENames(stmt)

	sawRecent := false
	for _, r := range refs {
		if r == "recent" {
			sawRecent = true
		}
	}
	if sawRecent {
		if _, known := ctes["recent"]; !known {
			t.Errorf("collectTableRefs reports %q as a table and collectCTENames "+
				"does not know it is a CTE, so validation rejects legal SQL.\n"+
				"refs: %v\nctes: %v", "recent", refs, ctes)
		}
	}
}

// A quoted CTE name may contain a dot, and was able to occupy a qualified
// table's slot.
//
// PostgreSQL resolves a CTE by bare identifier, so `atlantis.jobs` always means
// the table. But the validator compared CTE names and table references in one
// flat string space, so naming a CTE "atlantis.jobs" made the reference to the
// real table look like a CTE reference and skipped it.
//
// The damage was not a missing name in an error. A skipped reference never
// reaches refTables, so touches() stops being required to name the entity, and
// internal/server/entity derives from touches() whether a custom query binds
// the caller's tenant. A review reached console.users, atlantis.jobs and the IR
// checkpoint from a query declaring none of them.
func TestQuotedCTENameCannotShadowAQualifiedTable(t *testing.T) {
	for _, tc := range []struct {
		name, sql, mustSee string
	}{
		{
			"top-level WITH shadowing a qualified table",
			`WITH "atlantis.orders" AS (SELECT 1) SELECT id FROM atlantis.orders`,
			"atlantis.orders",
		},
		{
			"nested WITH shadowing a qualified table",
			`SELECT id FROM products WHERE active AND id IN
			   (WITH "atlantis.orders" AS (SELECT 1) SELECT product_id FROM atlantis.orders)`,
			"atlantis.orders",
		},
		{
			"shadowing through DELETE",
			`WITH "atlantis.orders" AS (SELECT 1) DELETE FROM atlantis.orders`,
			"atlantis.orders",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := pg.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			stmt := tree.Stmts[0].GetStmt()

			// realTableRefs, not a copy of it. Re-implementing the guard here —
			// collectTableRefs, the same qualification test, the same skip —
			// asserts against the test's own copy, and reverting the production
			// code to the shape that leaks leaves the whole suite green.
			kept := realTableRefs(stmt)

			found := false
			for _, r := range kept {
				if r == tc.mustSee {
					found = true
				}
			}
			if !found {
				t.Errorf("the reference to %q was skipped as a CTE. It never reaches "+
					"refTables, so touches() need not declare the entity and a custom "+
					"query against it runs with no tenant bound:\n  %s\nkept: %v",
					tc.mustSee, tc.sql, kept)
			}
		})
	}

	// And an ordinary unqualified CTE is still skipped, or every legitimate
	// WITH becomes an unknown table.
	tree, err := pg.Parse(`WITH recent AS (SELECT id FROM orders) SELECT id FROM recent`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, ref := range realTableRefs(tree.Stmts[0].GetStmt()) {
		if ref == "recent" {
			t.Error("an ordinary CTE is no longer recognised, so every legitimate " +
				"WITH clause would be reported as an unknown table")
		}
	}
}

// Scope, which a flat set of CTE names cannot represent.
//
// PostgreSQL scopes a CTE name to the WITH that declares it, and forbids a CTE
// as a DML target. Both shapes below were executed against a live database as
// the atlantis role and both read or wrote the real table while the validator
// saw a CTE. Both passed ValidateCustomQuery and ValidateCustomProcedure.
//
// A skipped reference never reaches refTables, so checkTouchesCoverage stops
// requiring the entity in touches(), and internal/server/entity derives from
// touches() whether a custom query binds the caller's tenant — so the statement
// runs unbound, on the bare pool.
func TestCTEScopeDoesNotShadowOutsideItsWith(t *testing.T) {
	for _, tc := range []struct{ name, sql, mustSee string }{
		{
			// The CTE is declared inside an unrelated subquery and cannot
			// shadow the top-level reference. A flat name set did not know that.
			"sibling scope cannot shadow a top-level read",
			`SELECT ir FROM ir_checkpoint WHERE id = 1
			   AND EXISTS (WITH ir_checkpoint AS (SELECT 1 AS z) SELECT 1 FROM ir_checkpoint)`,
			"ir_checkpoint",
		},
		{
			"sibling scope cannot shadow a qualified read",
			`SELECT id FROM atlantis.jobs
			   WHERE EXISTS (WITH jobs AS (SELECT 1) SELECT 1 FROM jobs)`,
			"atlantis.jobs",
		},
		{
			// PostgreSQL will not resolve an UPDATE target to a CTE, so the
			// name means the real relation.
			"an UPDATE target is never a CTE",
			`WITH ir_checkpoint AS (SELECT 1) UPDATE ir_checkpoint SET content_hash = 'x'`,
			"ir_checkpoint",
		},
		{
			"a DELETE target is never a CTE",
			`WITH jobs AS (SELECT 1) DELETE FROM jobs WHERE id = 1`,
			"jobs",
		},
		{
			"an INSERT target is never a CTE",
			`WITH jobs AS (SELECT 1) INSERT INTO jobs (id) VALUES (1)`,
			"jobs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := pg.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			kept := realTableRefs(tree.Stmts[0].GetStmt())
			for _, r := range kept {
				if r == tc.mustSee {
					return
				}
			}
			t.Errorf("the reference to %q was skipped as a CTE, and it is not one. "+
				"touches() then need not declare the entity, and the statement runs "+
				"with no tenant bound:\n  %s\nkept: %v", tc.mustSee, tc.sql, kept)
		})
	}
}

// And the legitimate nested shapes must still be skipped, or ordinary SQL
// stops applying. A gate that reports every WITH as an unknown table refuses
// schemas that applied yesterday.
func TestCTEInScopeIsStillSkipped(t *testing.T) {
	for _, sql := range []string{
		`WITH recent AS (SELECT id FROM orders) SELECT id FROM recent`,
		`WITH a AS (SELECT 1), b AS (SELECT * FROM a) SELECT * FROM b`,
		`SELECT id FROM products WHERE id IN
		   (WITH recent AS (SELECT product_id FROM orders) SELECT product_id FROM recent)`,
		`WITH RECURSIVE t AS (SELECT 1 AS n UNION ALL SELECT n+1 FROM t WHERE n < 5) SELECT n FROM t`,
		`WITH upd AS (UPDATE orders SET status = 'x' RETURNING id) SELECT id FROM upd`,
	} {
		tree, err := pg.Parse(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		for _, ref := range realTableRefs(tree.Stmts[0].GetStmt()) {
			switch ref {
			case "recent", "b", "t", "upd":
				t.Errorf("%q: the in-scope CTE %q was reported as a table, so this "+
					"ordinary statement is now an unknown-table error", sql, ref)
			}
		}
	}
}
