package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The emitted server names proto fields as strings; this holds the two
// emitters together.
//
// # What this guards
//
// A nullable ordering column's cursor value goes through
// runtime.PresentOrNil(ent, "score", ent.GetScore()), where "score" is a
// STRING that has to name a real field of the entity's proto message.
// PresentOrNil returns its input unchanged for a name it cannot resolve, which
// is the right failure mode at runtime — the alternative, returning nil, would
// make every row look NULL — but it means a misspelled or renamed field is
// silent: paging quietly goes back to the pre-fix behaviour of dropping every
// row past the first NULL, with nothing failing anywhere.
//
// Nothing else can catch this. The emitted server is only PARSED by these tests
// (#68), never type-checked, so a bad string is not a compile error here, and in
// the caller's repo it is not a compile error at all — it is a valid string
// literal. So the pairing has to be asserted directly, against the proto this
// same run emits.
//
// # Why both directions
//
// Checking only that emitted names resolve would pass vacuously if codegen
// stopped emitting PresentOrNil at all and went back to the bare getter. The
// second half asserts the split itself: nullable orderable fields go through
// PresentOrNil, NOT NULL ones do not.
func TestEmittedCursorFieldsExistInTheProto(t *testing.T) {
	ir := lower(t, `
entity Doc in library {
  id         bigint primary
  title      text not null
  score      real
  rank       int
  shelf      text
  created_at timestamptz not null
  retired_at timestamptz
}
`)
	AssignProtoNumbers(nil, ir)

	goFiles, err := EmitGoServer(ir)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	protoFiles, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}

	src := entityServerFile(t, goFiles)
	declared := protoFieldNames(t, protoFiles, "atlantis/library/v1/doc.proto", "Doc")

	// Direction 1: every name the server hands PresentOrNil is a field the
	// proto declares.
	emitted := map[string]bool{}
	for _, name := range presentOrNilFields(src) {
		emitted[name] = true
		if !declared[name] {
			t.Errorf("the emitted server calls runtime.PresentOrNil(ent, %q, ...), but "+
				"message Doc declares no field %q. PresentOrNil cannot resolve it, so it "+
				"returns the getter's value unchanged and a NULL in that column becomes a "+
				"zero in the cursor — paging silently stops short again.\n"+
				"declared: %v", name, name, declaredFieldList(declared))
		}
	}

	// Direction 2: the nullable/NOT NULL split is what decides whether the
	// call is emitted at all, so assert it per field rather than trusting
	// that direction 1 saw everything.
	e := ir.Entities[0]
	if e.Name != "Doc" {
		t.Fatalf("fixture drifted: first entity is %q", e.Name)
	}
	orderable := 0
	for _, f := range e.Fields {
		if !orderableType(f.Type) {
			continue
		}
		orderable++
		switch {
		case f.NotNull && emitted[f.Name]:
			t.Errorf("%q is NOT NULL but its cursor value goes through PresentOrNil. "+
				"That is not wrong at runtime, but it means nullability is no longer "+
				"what selects the call, and the nullable case could be lost without "+
				"this test noticing", f.Name)
		case !f.NotNull && !emitted[f.Name]:
			t.Errorf("%q is nullable and its cursor value does NOT go through "+
				"PresentOrNil, so a NULL in it reaches the token as %s's zero value. "+
				"This is the defect #71 fixed", f.Name, f.Type.Name)
		}
	}
	// A fixture that lowered to nothing orderable would pass every assertion
	// above without testing anything.
	if orderable < 4 {
		t.Fatalf("fixture has %d orderable fields; it needs several of each "+
			"nullability to prove anything", orderable)
	}
}

var presentOrNilRe = regexp.MustCompile(`runtime\.PresentOrNil\(ent, "([^"]+)"`)

func presentOrNilFields(src string) []string {
	var out []string
	for _, m := range presentOrNilRe.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// protoFieldNames parses the field names out of one message in an emitted
// .proto. This reads the emitted text rather than the dsl.Entity on purpose:
// the entity is what BOTH emitters were built from, so comparing the server
// against it would agree even if the proto emitter dropped or renamed a field.
func protoFieldNames(t *testing.T, files []ProtoFile, path, message string) map[string]bool {
	t.Helper()
	var content string
	for _, f := range files {
		if f.Path == path {
			content = f.Content
			break
		}
	}
	if content == "" {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = f.Path
		}
		t.Fatalf("no emitted proto at %s; got %v", path, paths)
	}
	decl := regexp.MustCompile(`^\s*(?:optional\s+|repeated\s+)?[\w.]+\s+(\w+)\s*=\s*\d+;`)
	out := map[string]bool{}
	inMsg := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, "message "+message+" {"):
			inMsg = true
			continue
		case inMsg && strings.HasPrefix(line, "}"):
			inMsg = false
		}
		if !inMsg {
			continue
		}
		if m := decl.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no fields out of message %s in %s:\n%s", message, path, content)
	}
	return out
}

func declaredFieldList(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprint(keys)
}

// A guard for the guard: the regex above has to match what the emitter
// actually writes. If cursorExtractorExpr's call shape changes — a different
// receiver name, a different argument order — presentOrNilFields silently
// returns nothing and the test above passes while checking nothing.
func TestPresentOrNilRegexMatchesTheEmitter(t *testing.T) {
	got := cursorExtractorExpr(dsl.Field{Name: "score", Type: dsl.FieldType{Name: "real"}}, nil)
	names := presentOrNilFields(got)
	if len(names) != 1 || names[0] != "score" {
		t.Fatalf("presentOrNilFields(%q) = %v; the regex no longer matches what "+
			"cursorExtractorExpr emits, so TestEmittedCursorFieldsExistInTheProto "+
			"is checking an empty list", got, names)
	}
}
