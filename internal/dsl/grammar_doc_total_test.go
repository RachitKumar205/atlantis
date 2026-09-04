package dsl

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every keyword the lexer knows is accounted for by the reference, and the
// account is true.
//
// TestDocumentedReservedWordsExist checks one direction: a word in the document
// exists in the lexer. Three wrong claims survived that in one day — `identity`
// and `backfill` absent from the reserved list entirely, five words listed as
// never-identifiers after indentation began separating them, and
// `chunk_time_interval` listed as contextual when it begins a member and cannot
// name a field. Each was found by hitting it, not by reading.
//
// So this asks the other direction, and asks the lexer rather than the prose:
// a word documented as usable for a field name must parse as one, and a word
// documented as reserved must not.
func TestEveryKeywordIsAccountedForByTheReference(t *testing.T) {
	b, err := os.ReadFile("../../web/docs/src/content/docs/reference/dsl-grammar.mdx")
	if err != nil {
		t.Fatalf("read grammar reference: %v", err)
	}
	text := string(b)

	// The three fenced lists under "Reserved words", in document order:
	// reserved, then the field modifiers that may also name a field, then the
	// cache block's contextual keywords.
	// Word lists only. A fenced block holding a declaration is an example, and
	// its identifiers are not keywords.
	var blocks []string
	for _, b := range fencedBlocksAfter(t, text, "## Reserved words") {
		if strings.ContainsAny(b, "{}\"") {
			continue
		}
		blocks = append(blocks, b)
	}
	if len(blocks) != 3 {
		t.Fatalf("found %d fenced word lists under Reserved words, want 3 "+
			"(reserved, modifiers, contextual) — re-anchor this test rather than "+
			"deleting it", len(blocks))
	}
	reserved := wordsIn(blocks[0])
	nameable := append(wordsIn(blocks[1]), wordsIn(blocks[2])...)

	documented := map[string]bool{}
	for _, w := range append(append([]string{}, reserved...), nameable...) {
		documented[w] = true
	}

	// Every keyword appears somewhere. A keyword in neither list is one a
	// reader has no way to learn about except by having a declaration refused.
	var undocumented []string
	for w := range keywords {
		if !documented[w] {
			undocumented = append(undocumented, w)
		}
	}
	if len(undocumented) > 0 {
		t.Errorf("in the lexer's keyword table and in no list under "+
			"\"Reserved words\": %v.\n  Add each to the reserved list, or to the "+
			"list of words that may name a field if it parses as one.", undocumented)
	}

	// And the claims hold, asked of the parser.
	for _, w := range nameable {
		if !parsesAsFieldName(w) {
			t.Errorf("%q is documented as usable for a field name and the parser refuses it", w)
		}
	}
	for _, w := range reserved {
		if parsesAsFieldName(w) {
			t.Errorf("%q is documented as reserved everywhere and the parser accepts it "+
				"as a field name", w)
		}
	}
}

// parsesAsFieldName reports whether the word names a field at member indent.
func parsesAsFieldName(w string) bool {
	src := "entity T in app {\n  id bigint primary\n  " + w + " text\n}\n"
	f, err := Parse("t.atl", []byte(src))
	if err != nil {
		return false
	}
	e, ok := f.Decls[0].(*EntityDecl)
	if !ok {
		return false
	}
	for _, m := range e.Members {
		if fd, ok := m.(*FieldDecl); ok && fd.Name == w {
			return true
		}
	}
	return false
}

// fencedBlocksAfter returns the contents of every ``` block following heading.
func fencedBlocksAfter(t *testing.T, text, heading string) []string {
	t.Helper()
	i := strings.Index(text, heading)
	if i < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	rest := text[i:]
	if end := strings.Index(rest, "\n## "); end > 0 {
		rest = rest[:end]
	}
	var out []string
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			return out
		}
		rest = rest[start+3:]
		// Drop the opening fence's info string. It sits between the
		// backticks and the first newline, and a tagged fence would
		// otherwise contribute its language as a documented word.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		end := strings.Index(rest, "```")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+3:]
	}
}

var wordRe = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// wordsIn extracts the keywords from one fenced list, dropping `// ...` notes.
func wordsIn(block string) []string {
	var kept []string
	for _, ln := range strings.Split(block, "\n") {
		if c := strings.Index(ln, "//"); c >= 0 {
			ln = ln[:c]
		}
		kept = append(kept, ln)
	}
	return wordRe.FindAllString(strings.Join(kept, "\n"), -1)
}
