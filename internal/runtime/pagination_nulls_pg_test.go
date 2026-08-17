package runtime_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Paging over a nullable ordering column must reach every row.
//
// # The reported failure, verbatim
//
// A table `(id bigint primary key, score real)` holding
// (1,1.0) (2,2.0) (3,NULL) (4,NULL), ordered `score ASC, id ASC` at limit 2.
// Page 1 returned [1,2] and encoded the cursor (2.0, 2). Page 2 ran
// `WHERE ("score","id") > ($1,$2)`, which evaluates to NULL for ids 3 and 4 —
// so it returned ZERO rows. Being shorter than the limit, it emitted no next
// token, and rows 3 and 4 were unreachable with nothing to tell the caller.
//
// # Why this test runs the SQL rather than the handler
//
// The dynamic dispatcher only ever orders by the primary key
// (buildDefaultKeysetCols), so it cannot reach this at all — the bug belongs to
// the GENERATED server, which builds keyset columns from the request's
// order_by. Driving the generated server would mean generating a proto and
// compiling it, which nothing in this repo does yet (#68).
//
// What actually decides the outcome is the SQL that KeysetPredicate and
// OrderByClauseFromKeyset produce, so this supplies the columns and cursor the
// generated server would supply and executes the result. A unit test over the
// strings would assert the SQL I wrote; this asserts the answer Postgres gives.
func TestPagingReachesEveryRowWhenTheOrderColumnIsNullable(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise null-aware keyset pagination")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pgnull_doc`)
	}
	drop()
	t.Cleanup(drop)
	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.pgnull_doc (id bigint PRIMARY KEY, score real);
INSERT INTO atlantis.pgnull_doc VALUES (1,1.0),(2,2.0),(3,NULL),(4,NULL)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	for _, tc := range []struct {
		name  string
		desc  bool
		pages [][]int64
	}{
		{
			// NULLS LAST: the two real scores, then the two NULLs.
			name: "ascending", desc: false,
			pages: [][]int64{{1, 2}, {3, 4}},
		},
		{
			// NULLS FIRST: the NULLs lead, then descending scores. The
			// tiebreaker descends too, so the NULL pair comes back 4 then 3 —
			// the full order is 4,3,2,1.
			name: "descending", desc: true,
			pages: [][]int64{{4, 3}, {2, 1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols := []runtime.KeysetColumn{
				{QuotedIdent: `"score"`, Desc: tc.desc, Nullable: true},
				{QuotedIdent: `"id"`, Desc: tc.desc, Nullable: false},
			}
			orderBy := runtime.OrderByClauseFromKeyset(cols)

			const limit = 2
			var cursor []any
			var seen []int64

			// One extra iteration than there are expected pages, so a run that
			// keeps handing back rows forever fails here rather than hanging.
			for page := 0; page <= len(tc.pages); page++ {
				where, args, perr := runtime.KeysetPredicate(cols, cursor, 1)
				if perr != nil {
					t.Fatalf("page %d: KeysetPredicate: %v", page, perr)
				}
				sql := `SELECT id, score FROM atlantis.pgnull_doc`
				if where != "" {
					sql += " WHERE " + where
				}
				sql += orderBy + fmt.Sprintf(" LIMIT %d", limit)

				rows, qerr := pool.Query(ctx, sql, args...)
				if qerr != nil {
					t.Fatalf("page %d: %v\nSQL: %s", page, qerr, sql)
				}
				var ids []int64
				var last []any
				for rows.Next() {
					var id int64
					var score *float32
					if serr := rows.Scan(&id, &score); serr != nil {
						rows.Close()
						t.Fatalf("page %d scan: %v", page, serr)
					}
					ids = append(ids, id)
					// A NULL column becomes a nil cursor coordinate. This is
					// the step the defect skipped: the value arrived through a
					// proto getter as 0, which compares in range, so the cursor
					// named a position no row sat at.
					if score == nil {
						last = []any{nil, id}
					} else {
						last = []any{*score, id}
					}
				}
				rows.Close()
				if rerr := rows.Err(); rerr != nil {
					t.Fatalf("page %d rows: %v", page, rerr)
				}

				if page == len(tc.pages) {
					if len(ids) != 0 {
						t.Errorf("page %d returned %v; the walk should be exhausted", page, ids)
					}
					break
				}
				if fmt.Sprint(ids) != fmt.Sprint(tc.pages[page]) {
					t.Fatalf("page %d returned %v, want %v.\nSQL: %s\nargs: %v",
						page, ids, tc.pages[page], sql, args)
				}
				seen = append(seen, ids...)

				// Round-trip the cursor through the token, because that is the
				// path a real caller takes and it is where a NULL used to be
				// rejected outright.
				tok, terr := runtime.EncodePageToken("pgnull.Doc", last)
				if terr != nil {
					t.Fatalf("page %d: EncodePageToken(%v): %v", page, last, terr)
				}
				cursor, terr = runtime.DecodePageToken(tok, "pgnull.Doc")
				if terr != nil {
					t.Fatalf("page %d: DecodePageToken: %v", page, terr)
				}
			}

			// Every row exactly once. The original defect showed up as a short
			// walk; a predicate that is too permissive shows up as a repeat,
			// and both are failures of the same property.
			if len(seen) != 4 {
				t.Errorf("the walk saw %d rows (%v), want all 4 exactly once", len(seen), seen)
			}
			got := map[int64]int{}
			for _, id := range seen {
				got[id]++
			}
			for id := int64(1); id <= 4; id++ {
				if got[id] != 1 {
					t.Errorf("row %d was returned %d times across the walk, want 1", id, got[id])
				}
			}
		})
	}
}

// The ORDER BY has to state the NULLS placement the predicate assumes.
//
// Postgres already defaults to these, so this is not about changing plans — it
// is that KeysetPredicate's null arms are derived from a specific ordering, and
// an unstated default is a contract only in the sense that both halves happen
// to agree. Flip one and the walk above breaks; this says which one.
func TestOrderByStatesTheNullsPlacement(t *testing.T) {
	got := runtime.OrderByClauseFromKeyset([]runtime.KeysetColumn{
		{QuotedIdent: `"a"`, Desc: false, Nullable: true},
		{QuotedIdent: `"b"`, Desc: true, Nullable: true},
	})
	for _, want := range []string{`"a" ASC NULLS LAST`, `"b" DESC NULLS FIRST`} {
		if !strings.Contains(got, want) {
			t.Errorf("ORDER BY %q does not contain %q", got, want)
		}
	}
}
