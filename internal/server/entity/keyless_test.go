package entity

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// testKeylessEntity returns an entity declared `keyless`, carrying a column
// called id that is not a key.
//
// The id column is what the SQL builders reach for when they have no key, and
// a discovered table often has one, so a fabricated clause naming it succeeds
// against this shape instead of failing.
func testKeylessEntity() *dsl.Entity {
	return &dsl.Entity{
		Name:      "Summaries",
		Namespace: "app",
		Kind:      dsl.EntityKindRegular,
		Keyless:   true,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, ProtoNumber: 1},
			{Name: "notes", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 2},
		},
	}
}

// testUnmarkedKeylessEntity is the production shape: no key and no `keyless`.
//
// Written by a binary predating the keyword, so the flag is false and every
// key-less code path is entered by an entity that has no key.
func testUnmarkedKeylessEntity() *dsl.Entity {
	e := testKeylessEntity()
	e.Keyless = false
	return e
}

// A keyless entity is absent from the snapshot and a keyed one is present.
//
// Both in one snapshot: an empty snapshot passes the first assertion on its
// own, and the failure being guarded against is a build that drops every
// entity, not one that drops the right entity.
func TestAKeylessEntityIsNotInTheSnapshot(t *testing.T) {
	keyless := testKeylessEntity()
	keyed := testAccount()
	ir := testIR(keyless, keyed)

	snap, err := buildSnapshot(ir, "hash")
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}

	if _, ok := snap.entities[keyless.ID()]; ok {
		t.Errorf("snapshot holds meta for the keyless entity %s", keyless.ID())
	}
	if _, ok := snap.entities[keyed.ID()]; !ok {
		t.Fatalf("snapshot holds no meta for the keyed entity %s, so this test proves nothing", keyed.ID())
	}

	for path, fd := range snap.files {
		if strings.Contains(path, "summaries") || strings.Contains(string(fd.Package()), "summaries") {
			t.Errorf("snapshot holds descriptor file %s for the keyless entity", path)
		}
	}
}

// An entity with no key and no `keyless` is refused, naming the declaration.
//
// The panic this replaces was `index out of range [0] with length 0` inside
// buildBatchGetRequest, which named neither the entity nor the declaration and
// took every other entity in the schema down with it. buildSnapshot supplies
// the entity name by wrapping, which the test below holds.
func TestAnUnmarkedKeylessEntityIsRefusedNotIndexed(t *testing.T) {
	e := testUnmarkedKeylessEntity()

	_, err := buildProtoDescriptors(e, nil)
	if err == nil {
		t.Fatal("buildProtoDescriptors accepted an entity with no key columns")
	}
	if !strings.Contains(err.Error(), "no key columns") {
		t.Errorf("error does not say what is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "keyless") {
		t.Errorf("error does not name the declaration that would fix it: %v", err)
	}
}

// The refusal reaches buildSnapshot, so one such entity fails the load rather
// than the process.
//
// A keyed entity sits beside it to prove the error is the guard's and not an
// empty-IR artefact.
func TestAnUnmarkedKeylessEntityFailsTheSnapshot(t *testing.T) {
	e := testUnmarkedKeylessEntity()
	ir := testIR(testAccount(), e)

	snap, err := buildSnapshot(ir, "hash")
	if err == nil {
		t.Fatal("buildSnapshot accepted an entity with no key columns")
	}
	if snap != nil {
		t.Errorf("buildSnapshot returned a snapshot alongside its error")
	}
	if !strings.Contains(err.Error(), e.ID()) {
		t.Errorf("error does not name the entity %s: %v", e.ID(), err)
	}
}

// No generated statement addresses a row by a column that is not a key.
//
// `"id"` was the fallback, and the id column on this fixture is not part of any
// key: a statement naming it reads and writes rows the request did not
// identify. Every builder that takes a key is asked, because the fallback was
// in two of them and the callers of both are spread over five call sites.
func TestNoStatementAddressesAKeylessEntityByAnUnkeyedColumn(t *testing.T) {
	e := testUnmarkedKeylessEntity()
	ir := testIR(e)

	if cols := schema.PKColumns(e); len(cols) != 0 {
		t.Fatalf("fixture resolves %d key columns, so it does not exercise the no-key path", len(cols))
	}

	stmts := map[string]string{
		"get":       buildGetSQL(e),
		"writeBack": buildWriteBackSQL(e),
		"batchGet":  buildBatchGetSQL(e),
		"update":    buildUpdateSQL(e, nil),
		"delete":    buildDeleteSQL(e, nil),
		"inbound":   buildSelectInboundSQL(e, []string{"notes"}),
	}
	// pkWhereClause returns the predicate alone, so it carries no WHERE for the
	// loop below to cut on. Prefixed here to be checked with the rest.
	stmts["pkWhere"] = "WHERE " + pkWhereClause(e, 1)
	// buildEntityMeta runs before buildProtoDescriptors reports the refusal, so
	// these strings are rendered for an entity the snapshot then rejects.
	meta := buildEntityMeta(e, ir, nil, nil)
	stmts["metaGet"] = meta.sqlGet
	stmts["metaBatchGet"] = meta.sqlBatchGet

	for what, sql := range stmts {
		// The row predicate only. UPDATE assigns to "id" in its SET list, which
		// is the column being written and not the column addressing the row.
		_, where, found := strings.Cut(sql, "WHERE ")
		if !found {
			t.Errorf("%s has no WHERE clause, so its predicate went unchecked: %s", what, sql)
			continue
		}
		// The sentinel by name, written out rather than read from noKeyColumn,
		// which would compare the implementation with itself.
		//
		// A check that only refused `"id"` passed on any other name a column
		// could hold: `"notes"` is a real column on this fixture, and `true`
		// makes the delete unbounded. Both address rows the request did not
		// identify, which is what the predicate has to be unable to do.
		if !strings.Contains(where, `"atlantis_no_key_column_for_Summaries"`) {
			t.Errorf("%s addresses the keyless entity by something other than the "+
				"no-key sentinel: %s", what, sql)
		}
	}
}
