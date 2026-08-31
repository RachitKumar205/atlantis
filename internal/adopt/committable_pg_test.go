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

	// Something for the foreign key below to point at. A UNIQUE satisfies
	// Postgres and does not satisfy atlantis, so this table is dropped and the
	// key that references it dangles — which is the case under test.
	`CREATE TABLE adopt_hz.no_pk_unique (a int NOT NULL UNIQUE, b text)`,

	// A column named for an .atl keyword. `identity double` parses as a
	// modifier and swallows the next field.
	`CREATE TABLE adopt_hz.reserved_col (
	    id bigint PRIMARY KEY,
	    identity double precision,
	    cols text[]
	 )`,

	// No primary key. Every entity must have one.
	`CREATE TABLE adopt_hz.no_pk (a int NOT NULL, b text)`,

	// A foreign key to a table that will be dropped for having no primary key.
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
func TestAReservedColumnNameSkipsTheTable(t *testing.T) {
	res := generateHazards(t)
	for _, e := range res.Entities {
		if strings.HasSuffix(e.Table, ".reserved_col") {
			t.Errorf("a declaration was generated for %s:\n%s", e.Table, e.Atl)
		}
	}
	assertSkipped(t, res, "reserved_col", "identity")
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
func TestAReferenceToASkippedTableIsDropped(t *testing.T) {
	res := generateHazards(t)
	atl := atlOf(t, res, "adopt_hz.points_at_no_pk")
	if strings.Contains(atl, "references") {
		t.Errorf("the declaration kept a reference to a table that was not declared:\n%s", atl)
	}
	assertSkipped(t, res, "points_at_no_pk", "references")
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
