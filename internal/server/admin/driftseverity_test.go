package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// classifyDriftSeverity matches codegen.ChangeKind values as bare strings, so
// nothing links the two: a kind added in codegen falls to the default and is
// reported as "mismatch". That default is the safer assumption but it is
// silent, and check_added / check_removed were misreported for exactly as long
// as it took someone to notice.
//
// This reads the kinds out of codegen's source and holds the switch to the rule
// its own doc comment states — "added"/"removed" kinds are additions and
// removals, everything else is a modification. A new kind in codegen now fails
// here rather than being quietly misfiled.
func TestEveryChangeKindIsClassified(t *testing.T) {
	kinds := changeKindsFromCodegen(t)
	if len(kinds) < 20 {
		t.Fatalf("found only %d ChangeKind constants; this test is not reading "+
			"what it thinks it is", len(kinds))
	}

	// The assertion is that every kind is an explicit decision, not that the
	// decision matches any particular rule.
	//
	// Deriving the expectation from the name suffix would have mandated the
	// suffix rule — a future kind ending in _added that is really a tightening
	// of an existing column would then be *required* to classify as an
	// addition, which is the opposite of asking a person. Requiring the literal
	// string to appear in the switch is what forces the decision without
	// dictating it.
	// The mention requirement applies to add/remove kinds only, because those
	// are the ones the default gets wrong. Every other kind — "changed",
	// "modified", "tightened", "loosened" — is a modification by construction,
	// which is what the default already says, so enumerating them would be
	// duplication that drifts.
	//
	// A future kind named _added that is really a tightening still lands here:
	// the test demands it be mentioned, and whoever mentions it chooses the arm.
	// That is the whole point — an expectation derived from the suffix would
	// have made "addition" mandatory and removed the choice.
	src := adoptSource(t)
	for _, k := range kinds {
		if !strings.HasSuffix(k, "_added") && !strings.HasSuffix(k, "_removed") {
			if got := classifyDriftSeverity(k); got != "mismatch" {
				t.Errorf("classifyDriftSeverity(%q) = %q; a kind that is neither an "+
					"addition nor a removal is a modification", k, got)
			}
			continue
		}
		if !strings.Contains(src, strconv.Quote(k)) {
			t.Errorf("codegen declares ChangeKind %q and classifyDriftSeverity does "+
				"not mention it, so it falls to the default and is reported as a "+
				"modification.\n"+
				"  Add it to whichever arm is right. That is a judgement — "+
				"index_added and composite_unique_added were additions misfiled as "+
				"modifications for as long as this list was maintained by hand, and "+
				"an expectation derived from the name would not have let anyone "+
				"say otherwise.", k)
		}
	}

	// Spot-check the arms actually work, so "mentioned somewhere" cannot be
	// satisfied by a comment.
	for k, want := range map[string]string{
		"check_added":            "addition",
		"check_removed":          "removal",
		"check_changed":          "mismatch",
		"entity_added":           "addition",
		"composite_unique_added": "addition",
	} {
		if got := classifyDriftSeverity(k); got != want {
			t.Errorf("classifyDriftSeverity(%q) = %q, want %q", k, got, want)
		}
	}
}

// adoptSource returns the text of the file classifyDriftSeverity lives in.
func adoptSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own path")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "adopt.go"))
	if err != nil {
		t.Fatalf("read adopt.go: %v", err)
	}
	return string(b)
}

// changeKindsFromCodegen extracts the string values of the ChangeKind constants
// declared in internal/codegen, by parsing the source rather than importing —
// the values are what cross the wire, and a compile-time reference would not
// enumerate them.
func changeKindsFromCodegen(t *testing.T) []string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own path")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	src := filepath.Join(repoRoot, "internal", "codegen", "diff.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only constants explicitly typed ChangeKind.
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "ChangeKind" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out = append(out, s)
			}
		}
	}
	return out
}
