package query

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	commonv1 "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
)

// The float predicates, executed.
//
// The unit tests assert the SQL this package writes. This asserts the rows
// Postgres returns for it, which is the only thing that settles the question
// the ::real cast exists to answer.
//
// The fixture uses values that are NOT exactly representable in binary
// floating point on purpose. 0.1, 2.5 and 3.75 would make a lazy
// implementation pass: 2.5 and 3.75 are exact in both widths, so a missing
// cast never shows. 0.1 is exact in neither, and float4 0.1 promotes to
// 0.10000000149011612 while float8 0.1 is 0.1000000000000000055511151231257827
// — different numbers. That gap is the whole test.
func TestFloatPredicatesAgainstPostgres(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the float predicates")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pgfloat_probe`)
	}
	drop()
	t.Cleanup(drop)
	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.pgfloat_probe (
  id     bigint PRIMARY KEY,
  score  real,
  weight double precision
);
INSERT INTO atlantis.pgfloat_probe VALUES
  (1, 0.1,  0.1),
  (2, 2.5,  2.5),
  (3, 10.25, 10.25),
  (4, NULL, NULL)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	run := func(t *testing.T, field string, pred any) []int64 {
		t.Helper()
		f := newFilter(t)
		switch p := pred.(type) {
		case *commonv1.FloatPredicate:
			setPredicate(t, f, field, p)
		case *commonv1.DoublePredicate:
			setPredicate(t, f, field, p)
		default:
			t.Fatalf("unsupported predicate %T", pred)
		}
		where, args, _, terr := TranslateFilter(testSpec(), f.ProtoReflect(), 1)
		if terr != nil {
			t.Fatalf("TranslateFilter: %v", terr)
		}
		sql := `SELECT id FROM atlantis.pgfloat_probe`
		if where != "" {
			sql += " WHERE " + where
		}
		sql += " ORDER BY id"
		rows, qerr := pool.Query(ctx, sql, args...)
		if qerr != nil {
			t.Fatalf("%v\nSQL: %s\nargs: %v", qerr, sql, args)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if serr := rows.Scan(&id); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			ids = append(ids, id)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		return ids
	}

	t.Run("eq on a real column finds the row", func(t *testing.T) {
		// The case the cast was chosen for. Without `$1::real`, PG promotes the
		// COLUMN to float8 — 0.1 stored as float4 becomes 0.10000000149011612,
		// the parameter stays 0.1, and this returns nothing at all while the
		// row plainly reads back as 0.1.
		got := run(t, "score", &commonv1.FloatPredicate{
			Op: &commonv1.FloatPredicate_Eq{Eq: 0.1}})
		if fmt.Sprint(got) != "[1]" {
			t.Errorf("eq 0.1 on a real column returned %v, want [1]. A float4 "+
				"column compared against an un-cast float8 parameter matches "+
				"nothing, which is the defect the ::real cast removes", got)
		}
	})

	t.Run("eq on a double column finds the row", func(t *testing.T) {
		got := run(t, "weight", &commonv1.DoublePredicate{
			Op: &commonv1.DoublePredicate_Eq{Eq: 0.1}})
		if fmt.Sprint(got) != "[1]" {
			t.Errorf("eq 0.1 on a double column returned %v, want [1]", got)
		}
	})

	for _, tc := range []struct {
		name string
		pred *commonv1.FloatPredicate
		want string
	}{
		{"neq", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Neq{Neq: 0.1}}, "[2 3]"},
		{"lt", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Lt{Lt: 2.5}}, "[1]"},
		{"lte", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Lte{Lte: 2.5}}, "[1 2]"},
		{"gt", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Gt{Gt: 2.5}}, "[3]"},
		{"gte", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Gte{Gte: 2.5}}, "[2 3]"},
		{"in", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_In{
			In: &commonv1.FloatList{Values: []float32{0.1, 10.25}}}}, "[1 3]"},
		{"is_null", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_IsNull{IsNull: true}}, "[4]"},
		{"is_not_null", &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_IsNotNull{IsNotNull: true}}, "[1 2 3]"},
	} {
		t.Run("real/"+tc.name, func(t *testing.T) {
			if got := fmt.Sprint(run(t, "score", tc.pred)); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		pred *commonv1.DoublePredicate
		want string
	}{
		{"neq", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Neq{Neq: 0.1}}, "[2 3]"},
		{"lt", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Lt{Lt: 2.5}}, "[1]"},
		{"lte", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Lte{Lte: 2.5}}, "[1 2]"},
		{"gt", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Gt{Gt: 2.5}}, "[3]"},
		{"gte", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Gte{Gte: 2.5}}, "[2 3]"},
		{"in", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_In{
			In: &commonv1.DoubleList{Values: []float64{0.1, 10.25}}}}, "[1 3]"},
		{"is_null", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_IsNull{IsNull: true}}, "[4]"},
		{"is_not_null", &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_IsNotNull{IsNotNull: true}}, "[1 2 3]"},
	} {
		t.Run("double/"+tc.name, func(t *testing.T) {
			if got := fmt.Sprint(run(t, "weight", tc.pred)); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The hazard the ::real cast exists for, stated as Postgres behaviour.
//
// This asserts PG, not atlantis, and that is deliberate. Three separate things
// currently stop a float4 column being compared at float8 — the proto arm's
// width, the ::real cast in the emitted SQL, and pgx negotiating the parameter
// type — so removing any ONE of them breaks no other test. That redundancy is
// fine, but it leaves the REASON untested, and an untested reason is what gets
// removed by someone tidying up a cast that "does nothing".
//
// If this ever starts returning 1 for the uncast case, the cast really has
// become unnecessary and can go. Until then it is load-bearing under exactly
// the conditions that inference does not cover.
func TestFloat4ComparisonNeedsTheWidth(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise float4 comparison semantics")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pgfloat_width`)
	}
	drop()
	t.Cleanup(drop)
	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.pgfloat_width (score real);
INSERT INTO atlantis.pgfloat_width VALUES (0.1)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	count := func(where string) int {
		var n int
		if qerr := pool.QueryRow(ctx,
			`SELECT count(*) FROM atlantis.pgfloat_width WHERE `+where).Scan(&n); qerr != nil {
			t.Fatalf("%s: %v", where, qerr)
		}
		return n
	}

	if n := count("score = 0.1::float8"); n != 0 {
		t.Errorf("`score = 0.1::float8` matched %d rows, want 0. If PG has changed "+
			"how it resolves float4 against float8, the ::real cast in "+
			"translateFloatPredicate is no longer load-bearing and its comment is "+
			"now wrong", n)
	}
	if n := count("score = (0.1::float8)::real"); n != 1 {
		t.Errorf("`score = (0.1::float8)::real` matched %d rows, want 1 — casting "+
			"the value down to the column's width is what makes eq usable, and it "+
			"just stopped working", n)
	}
	// A bare decimal literal is `numeric` and resolves the same losing way.
	// Worth pinning because it is what a person types into psql when checking
	// whether the filter "really" works.
	if n := count("score = 0.1"); n != 0 {
		t.Errorf("`score = 0.1` matched %d rows, want 0", n)
	}
}

// Filtering and paging over the same float column must agree.
//
// They compare at different widths by construction: the filter casts its
// parameter down to float4, while ORDER BY and the keyset cursor compare the
// column after promotion to float8 (runtime.EncodePageToken widens a float32
// deliberately — see the comment there). Two comparisons at two widths over
// one column is exactly the setup where a row is skipped between pages, or
// returned on both.
//
// This is the one interaction between #67 and #71, and it is asserted rather
// than reasoned about.
func TestFloatFilterAndPagingAgree(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise float filter + paging")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pgfloat_page`)
	}
	drop()
	t.Cleanup(drop)
	// Values chosen so consecutive float4s are close enough that a float8
	// comparison could straddle them.
	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.pgfloat_page (id bigint PRIMARY KEY, score real);
INSERT INTO atlantis.pgfloat_page
SELECT g, 0.1 * g FROM generate_series(1, 9) g`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// Everything strictly above 0.2 — ids 3..9, seven rows.
	f := newFilter(t)
	setPredicate(t, f, "score", &commonv1.FloatPredicate{
		Op: &commonv1.FloatPredicate_Gt{Gt: 0.2}})
	where, args, next, terr := TranslateFilter(testSpec(), f.ProtoReflect(), 1)
	if terr != nil {
		t.Fatalf("TranslateFilter: %v", terr)
	}
	_ = next

	seen := map[int64]int{}
	var cursorID int64 = -1
	const limit = 3
	// One more iteration than pages needed, so a walk that never terminates
	// fails here rather than hanging.
	for page := 0; page < 6; page++ {
		sql := `SELECT id, score FROM atlantis.pgfloat_page WHERE ` + where
		pargs := append([]any{}, args...)
		if cursorID >= 0 {
			pargs = append(pargs, cursorID)
			sql += fmt.Sprintf(" AND id > $%d", len(pargs))
		}
		sql += fmt.Sprintf(` ORDER BY "score" ASC NULLS LAST, "id" ASC LIMIT %d`, limit)

		rows, qerr := pool.Query(ctx, sql, pargs...)
		if qerr != nil {
			t.Fatalf("page %d: %v\nSQL: %s", page, qerr, sql)
		}
		n := 0
		for rows.Next() {
			var id int64
			var score float32
			if serr := rows.Scan(&id, &score); serr != nil {
				rows.Close()
				t.Fatalf("scan: %v", serr)
			}
			seen[id]++
			cursorID = id
			n++
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("page %d rows: %v", page, rerr)
		}
		if n == 0 {
			break
		}
	}

	// The filter selects ids 3..9. Every one exactly once, and nothing else.
	for id := int64(3); id <= 9; id++ {
		if seen[id] != 1 {
			t.Errorf("row %d was returned %d times across the filtered walk, want 1. "+
				"The filter compares at float4 and the ordering at float8; a count "+
				"other than 1 is those two disagreeing", id, seen[id])
		}
	}
	for id := int64(1); id <= 2; id++ {
		if seen[id] != 0 {
			t.Errorf("row %d is excluded by `score > 0.2` but appeared %d times",
				id, seen[id])
		}
	}
}
