package dsl

import (
	"os"
	"strings"
	"testing"
)

// The grammar reference's reserved-word list must name only real keywords.
//
// Three names it has carried do not exist: `composite_pk`, where the parser
// spells composite primary keys `primary by`, and `no` / `action`, from an
// `on delete no action` form the parser rejects — it takes cascade, set null
// and restrict only.
//
// Nothing else compiles the document, so a name in it that the lexer does not
// know produces a parse error and no hint that the reference is wrong.
func TestDocumentedReservedWordsExist(t *testing.T) {
	b, err := os.ReadFile("../../docs/reference/dsl-grammar.md")
	if err != nil {
		t.Fatalf("read grammar reference: %v", err)
	}

	// Every word list under the heading, not just the first. Scanning one is
	// how `partition_field` — a hypertable clause that does not exist, the
	// time column is named in the header — survived in a later list and in the
	// grammar production beside it.
	var words []string
	for _, blk := range fencedBlocksAfter(t, string(b), "## Reserved words") {
		if strings.ContainsAny(blk, "{}\"") {
			continue // a declaration example, not a word list
		}
		words = append(words, wordsIn(blk)...)
	}
	if len(words) < 20 {
		t.Fatalf("found only %d words under \"Reserved words\"; the parse is wrong "+
			"and this test is not reading what it thinks it is", len(words))
	}

	var missing []string
	for _, w := range words {
		if _, ok := keywords[w]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("documented under \"Reserved words\" but absent from the lexer's "+
			"keyword table: %v.\n  Either the keyword was removed and the document "+
			"was not updated, or it was never implemented. Both mislead a reader "+
			"who has no way to check.", missing)
	}
}
