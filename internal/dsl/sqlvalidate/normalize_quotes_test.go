package sqlvalidate

import "testing"

// Declarations PostgreSQL accepts, which the boot audit must not refuse.
//
// The audit runs over the ALREADY-STORED IR and is fatal under the isolation
// flag, so a false rejection here stops a server that started yesterday from
// starting today — over a constraint the database has been enforcing all along.
func TestNormalizeNamedParams_LeavesQuotesAndCommentsAlone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`position($tag$@$tag$ in email) > 0`, `position($tag$@$tag$ in email) > 0`},
		{`$$a$$ <> ''`, `$$a$$ <> ''`},
		{`$q$ $notaparam $q$ = x`, `$q$ $notaparam $q$ = x`},
		{"x > 0 -- don't allow zero", "x > 0 -- don't allow zero"},
		{"x > 0 /* don't */ AND y > 0", "x > 0 /* don't */ AND y > 0"},
		{"a /* outer /* inner */ still */ b", "a /* outer /* inner */ still */ b"},
		// And the rewrite it exists for still happens.
		{`email = $caller`, `email = $1`},
		{`id = $1`, `id = $1`},
		{`'$notaparam' = x`, `'$notaparam' = x`},
	} {
		if got := normalizeNamedParams(tc.in); got != tc.want {
			t.Errorf("normalizeNamedParams(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}
