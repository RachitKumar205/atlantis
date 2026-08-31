package adopt

import (
	"strings"
	"testing"
)

// The generated .atl has to lower, not merely parse.
//
// Every case here was found by running the pipeline against a public 209-table
// research database, and every one of them made the whole namespace unusable:
// Lower reports the first failure and stops, so one bad table takes every good
// one in the file with it. A declaration that cannot be committed is worse than
// an absent one, because nothing says so until `tide plan` runs.

// hazards is one schema carrying every shape that broke the read, so a fix for
// one cannot regress another.
var hazards = []string{
	`CREATE SCHEMA adopt_hz`,

	// No PRIMARY KEY, and a NOT NULL UNIQUE column that addresses a row just
	// as well. The key is taken from it, so this table is declared and the
	// foreign key below survives.
	`CREATE TABLE adopt_hz.no_pk_unique (a int NOT NULL UNIQUE, b text)`,

	// The same shape with a nullable unique. Two rows may hold NULL there, so
	// it addresses nothing and no key is taken; Postgres still accepts a
	// foreign key to it, which is what makes a dangling reference reachable.
	`CREATE TABLE adopt_hz.no_pk_nullable (a int UNIQUE, b text)`,

	`CREATE TABLE adopt_hz.points_at_nullable (
	    id bigint PRIMARY KEY,
	    a  int REFERENCES adopt_hz.no_pk_nullable(a)
	 )`,

	// A column named for a keyword that also begins an entity member. `check`
	// at member indent is a table CHECK whichever way it is read, so it cannot
	// name a field and the table goes.
	`CREATE TABLE adopt_hz.reserved_col (
	    id bigint PRIMARY KEY,
	    "check" text,
	    cols text[]
	 )`,

	// A column named for a field modifier. Column tells the two apart: at
	// member indent it names a field, on a field's own line it modifies it.
	`CREATE TABLE adopt_hz.modifier_col (
	    id bigint PRIMARY KEY,
	    identity double precision,
	    "default" text
	 )`,

	// No primary key. Every entity must have one.
	`CREATE TABLE adopt_hz.no_pk (a int NOT NULL, b text)`,

	// A foreign key to the table whose key is taken from a unique constraint.
	`CREATE TABLE adopt_hz.points_at_no_pk (
	    id bigint PRIMARY KEY,
	    a  int REFERENCES adopt_hz.no_pk_unique(a)
	 )`,

	// A CHECK whose name Postgres allows and .atl cannot spell.
	`CREATE TABLE adopt_hz.dollar_check (
	    id bigint PRIMARY KEY,
	    flag text CONSTRAINT "ck_t$flag" CHECK (flag IN ('Y','N'))
	 )`,

	// A 32-bit sequence. .atl spells `serial` only on bigint.
	`CREATE TABLE adopt_hz.narrow_serial (id serial PRIMARY KEY, name text)`,

	// Two indexes over the same column, which is one `index by` clause.
	`CREATE TABLE adopt_hz.twin_index (id bigint PRIMARY KEY, owner_id bigint)`,
	`CREATE INDEX twin_a ON adopt_hz.twin_index (owner_id)`,
	`CREATE INDEX twin_b ON adopt_hz.twin_index (owner_id)`,
}

func generateHazards(t *testing.T) Result {
	t.Helper()
	return generateAllIn(t, hazards, "adopt_hz")
}

// The whole file lowers. The property everything else here serves.
func TestEveryGeneratedNamespaceLowers(t *testing.T) {
	res := generateHazards(t)
	if len(res.Entities) == 0 {
		t.Fatal("nothing was generated, so this proves nothing")
	}
	// This namespace only. GenerateAll reads every schema the test database
	// has, and the console's own tables are not what is under test here.
	var src strings.Builder
	var n int
	for _, e := range res.Entities {
		if e.Namespace != "adopt_hz" {
			continue
		}
		n++
		src.WriteString(e.Atl)
		src.WriteString("\n")
	}
	if n == 0 {
		t.Fatal("no adopt_hz entities, so this proves nothing")
	}
	mustLower(t, src.String())
}

// A column named for an .atl keyword takes its table out, not the file.
//
// `check` begins an entity member, so it cannot also name a field: both
// readings are available at member indent and nothing separates them.
func TestAReservedColumnNameSkipsTheTable(t *testing.T) {
	res := generateHazards(t)
	for _, e := range res.Entities {
		if strings.HasSuffix(e.Table, ".reserved_col") {
			t.Errorf("a declaration was generated for %s:\n%s", e.Table, e.Atl)
		}
	}
	assertSkipped(t, res, "reserved_col", "check")
}

// A column named for a field modifier is declared, because column separates
// the two readings.
//
// `identity` is a percent-identity measure on a public bioinformatics dataset,
// and the table carrying it is the target of four foreign keys — so skipping it
// dropped those too.
func TestAModifierColumnNameIsDeclared(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.modifier_col")
	for _, want := range []string{"identity", "default"} {
		if !strings.Contains(atl, want) {
			t.Errorf("the declaration lost the %q column:\n%s", want, atl)
		}
	}
	mustLower(t, "// generated\n"+atl)
}

// A table with no primary key is skipped, and still gets the suggestion that
// would let it be read next time.
func TestATableWithNoPrimaryKeyIsSkippedAndSuggested(t *testing.T) {
	res := generateHazards(t)
	for _, e := range res.Entities {
		if strings.HasSuffix(e.Table, ".no_pk") {
			t.Errorf("a declaration was generated for %s, which has no primary key", e.Table)
		}
	}
	assertSkipped(t, res, "no_pk", "primary key")

	// Computed before the drop. Without that the only table the advice applies
	// to is the one it is never shown for.
	var suggested bool
	for _, s := range res.Suggestions {
		if strings.HasSuffix(s.Table, ".no_pk") && s.Kind == SuggestNoPrimaryKey {
			suggested = true
		}
	}
	if !suggested {
		t.Errorf("no no-primary-key suggestion for adopt_hz.no_pk: %+v", res.Suggestions)
	}
}

// A foreign key to a skipped table is dropped, and the table that carries it
// survives.
//
// Lower resolves references against the file, so leaving one dangling fails the
// whole namespace — which is how a single PK-less table took 61 good ones down.
//
// The target has a nullable unique constraint: enough for Postgres to accept
// the foreign key, not enough to address a row, so the table is skipped and the
// reference has nowhere to resolve.
func TestAReferenceToASkippedTableIsDropped(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.points_at_nullable")
	if strings.Contains(atl, "references") {
		t.Errorf("the declaration kept a reference to a table that was not declared:\n%s", atl)
	}
	assertSkipped(t, res, "points_at_nullable", "references")
}

// A table whose key comes from a UNIQUE constraint is declared, and the
// foreign keys pointing at it survive with it.
//
// Postgres refuses a foreign key to a table with no unique constraint, so every
// table a key points at has a candidate. Requiring a declared PRIMARY KEY
// dropped those tables and every reference to them: five such references on one
// public dataset.
func TestAPromotedKeyKeepsItsInboundReference(t *testing.T) {
	res := generateHazards(t)

	target := atlOf(t, res, "adopt_hz.no_pk_unique")
	if !strings.Contains(target, "primary") {
		t.Errorf("the target has a NOT NULL UNIQUE column and no key was taken from it:\n%s", target)
	}

	atl := atlOf(t, res, "adopt_hz.points_at_no_pk")
	if !strings.Contains(atl, "references") {
		t.Errorf("the reference was dropped although its target is declared:\n%s", atl)
	}
}

// A CHECK whose name does not lex is left out, with the table kept.
func TestACheckNameThatDoesNotLexIsNotDeclared(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.dollar_check")
	if strings.Contains(atl, "$") {
		t.Errorf("the declaration carries a name .atl cannot spell:\n%s", atl)
	}
	assertWarned(t, res, "ck_t$flag")
}

// A 32-bit sequence keeps both its type and `serial`.
//
// `serial` renders SMALLSERIAL, SERIAL or BIGSERIAL from the column width, so
// the declaration describes the sequence the database has rather than dropping
// it. Nothing is warned about, because nothing was lost.
func TestANarrowSerialKeepsItsTypeAndItsSerial(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.narrow_serial")

	// The id line, not the whole file: the table is called narrow_serial, so
	// searching the file for "serial" matches its own `table "..."` line.
	var idLine string
	for _, l := range strings.Split(atl, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "id" {
			idLine = strings.TrimSpace(l)
		}
	}
	if idLine == "" {
		t.Fatalf("no id column in the declaration:\n%s", atl)
	}
	if !strings.Contains(idLine, "serial") {
		t.Errorf("id = %q; the sequence was dropped, so a plan would propose removing it", idLine)
	}
	if !strings.Contains(idLine, "int") {
		t.Errorf("id = %q; the column lost its type", idLine)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "narrow_serial") {
			t.Errorf("a narrow sequence is declarable, so it should raise no warning: %q", w)
		}
	}
}

// Two indexes over the same columns produce one clause.
func TestDuplicateIndexesProduceOneClause(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.twin_index")
	if n := strings.Count(atl, "index by owner_id"); n != 1 {
		t.Errorf("`index by owner_id` appears %d times, want 1:\n%s", n, atl)
	}
	assertWarned(t, res, "same columns")
}

func assertSkipped(t *testing.T, res Result, table, reason string) {
	t.Helper()
	for _, s := range res.Skipped {
		if strings.Contains(s, table) && strings.Contains(s, reason) {
			return
		}
	}
	t.Errorf("no skip naming %q and %q: %v", table, reason, res.Skipped)
}

func assertWarned(t *testing.T, res Result, want string) {
	t.Helper()
	for _, w := range res.Warnings {
		if strings.Contains(w, want) {
			return
		}
	}
	t.Errorf("no warning naming %q: %v", want, res.Warnings)
}
