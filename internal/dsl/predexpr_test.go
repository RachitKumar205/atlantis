package dsl

import "testing"

// dslToSQL is pure Go and lives outside the cgo build tag, so it is exercised in
// both cgo and cgo-free builds. The predicate tests that need Postgres's parser
// are in predicate_test.go, which is cgo-only.
func TestDslToSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{`status = "active"`, `status = 'active'`},
		{`note = "it's"`, `note = 'it''s'`},
		{`x = "a\"b"`, `x = 'a"b'`},
		{`a   is    null`, `a is null`},
		{`lower(sku) like "a%" /* note */ and tier > 1`, `lower(sku) like 'a%' and tier > 1`},
		{`tag = "}" and deleted_at is null`, `tag = '}' and deleted_at is null`},
	}
	for _, tc := range cases {
		if got := dslToSQL(tc.in); got != tc.want {
			t.Errorf("dslToSQL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
