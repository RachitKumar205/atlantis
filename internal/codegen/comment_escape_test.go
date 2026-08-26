package codegen

import (
	"strings"
	"testing"
)

// Author text must never be able to end the comment line it is written into.
//
// A `--` comment ends at the first newline, and the DSL lexer turns `\n` in a
// string literal into a real newline. On PostgreSQL 17.8,
//
//	total int check "total > 0 /*\n; DROP POLICY shop_doc_partition ON shop.doc; --*/"
//
// passes parse, lower, every validator in internal/dsl/sqlvalidate and the
// emitter: to a parser the expression is an ordinary CHECK, since the newline
// is whitespace and the block comment is stripped. In the emitted migration the
// second line is top-level SQL, and applying it drops the row-level security
// policy off the table.
//
// Asserted at the builder, because that is where every channel converges.
// CHECK, backfill, index expression and index predicate all reach the same
// function through Change.Detail, and a Kind added later inherits the
// guarantee rather than having to remember it.
func TestCommentfCannotBeEscaped(t *testing.T) {
	for _, payload := range []string{
		"total > 0 /*\n; DROP POLICY p ON atlantis.doc; --*/",
		"a\r\nDROP TABLE atlantis.jobs;",
		"a\rDROP TABLE atlantis.jobs;",
		"a\vDROP TABLE atlantis.jobs;",
		"a\fDROP TABLE atlantis.jobs;",
		"aDROP TABLE atlantis.jobs;",
	} {
		var b sqlBuilder
		b.commentf("check_added: %s", payload)
		got := b.String()

		if n := strings.Count(strings.TrimSuffix(got, "\n"), "\n"); n != 0 {
			t.Errorf("payload %q produced %d extra line(s); everything after the "+
				"first is top-level SQL:\n%s", payload, n, got)
		}
		if !strings.HasPrefix(got, "-- ") {
			t.Errorf("payload %q did not stay a comment: %q", payload, got)
		}
		// The text must survive, minus the control characters. A sanitizer that
		// dropped the detail entirely would satisfy the checks above while
		// making every generated migration unreadable.
		if !strings.Contains(got, "DROP") {
			t.Errorf("payload %q lost its text; the comment is meant to stay "+
				"readable: %q", payload, got)
		}
	}
}

// Ordinary details must round-trip unchanged.
//
// The counterpart to the test above: a gate that mangles legitimate text moves
// the cost onto every reader of every migration.
func TestCommentfKeepsOrdinaryText(t *testing.T) {
	var b sqlBuilder
	b.commentf("check_added: %s", "total > 0 AND status IN ('a', 'b')")
	if got, want := b.String(), "-- check_added: total > 0 AND status IN ('a', 'b')\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
