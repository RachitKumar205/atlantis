package dsl

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The grammar reference's reserved-word list must name only real keywords.
//
// It named three that do not exist: `composite_pk` (the parser spells composite
// primary keys `primary by`) and `no` / `action` (from an `on delete no action`
// form the parser rejects — it accepts only cascade, set null, restrict). All
// three were documented in the production rules and the prose as well, so a
// reader following the reference got a parse error with no hint that the
// document was wrong.
//
// This is cheap to check and the document is otherwise unverifiable, which is
// the combination that lets it rot.
func TestDocumentedReservedWordsExist(t *testing.T) {
	b, err := os.ReadFile("../../docs/reference/dsl-grammar.md")
	if err != nil {
		t.Fatalf("read grammar reference: %v", err)
	}
	text := string(b)

	const anchor = "entity, hypertable, query, procedure,"
	i := strings.Index(text, anchor)
	if i < 0 {
		t.Fatal("reserved-word list not found — this test is not reading what it " +
			"thinks it is. If the list moved, re-anchor it rather than deleting " +
			"this test.")
	}
	end := strings.Index(text[i:], "```")
	if end < 0 {
		t.Fatal("reserved-word list is not inside a fenced block")
	}
	block := text[i : i+end]

	words := regexp.MustCompile(`[a-z_][a-z0-9_]*`).FindAllString(block, -1)
	if len(words) < 20 {
		t.Fatalf("found only %d words in the reserved-word list; the parse is wrong",
			len(words))
	}
	var missing []string
	for _, w := range words {
		if _, ok := keywords[w]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("documented as reserved but absent from the lexer's keyword table: %v.\n"+
			"  Either the keyword was removed and the document was not updated, or "+
			"it was never implemented. Both mislead a reader who has no way to "+
			"check.", missing)
	}
}
