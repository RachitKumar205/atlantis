package dsl

import (
	"os"
	"regexp"
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
	text := string(b)

	// Both blocks: the reserved list and the contextual-keyword list that
	// follows it. Scanning only the first is how `partition_field` — a
	// hypertable clause that does not exist, the time column is named in the
	// header — survived in the contextual list and in the grammar production
	// beside it.
	const anchor = "entity, hypertable, query, procedure,"
	i := strings.Index(text, anchor)
	if i < 0 {
		t.Fatal("reserved-word list not found — this test is not reading what it " +
			"thinks it is. If the list moved, re-anchor it rather than deleting " +
			"this test.")
	}
	rest := text[i:]
	var block string
	for n := 0; n < 2; n++ {
		end := strings.Index(rest, "```")
		if end < 0 {
			break
		}
		block += rest[:end]
		rest = rest[end+3:]
		next := strings.Index(rest, "```")
		if next < 0 {
			break
		}
		rest = rest[next+3:]
	}
	if block == "" {
		t.Fatal("reserved-word list is not inside a fenced block")
	}

	// Strip trailing `// ...` notes; the contextual block annotates each line
	// with the construct it belongs to, and those words are prose.
	var kept []string
	for _, ln := range strings.Split(block, "\n") {
		if c := strings.Index(ln, "//"); c >= 0 {
			ln = ln[:c]
		}
		kept = append(kept, ln)
	}
	words := regexp.MustCompile(`[a-z_][a-z0-9_]*`).FindAllString(strings.Join(kept, "\n"), -1)
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
