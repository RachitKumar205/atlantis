package atlprint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

const sampleSrc = `// User accounts for the auth namespace.
entity User in auth {
  id          bigint primary serial
  email       varchar(255) not null unique
  created_at  timestamptz not null default now()

  index by email
}
`

// fieldNames returns the ordered field names of the named entity in src.
func fieldNames(t *testing.T, src []byte, namespace, entity string) []string {
	t.Helper()
	f, err := dsl.Parse("t.atl", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := findEntity(f, namespace, entity)
	if e == nil {
		t.Fatalf("entity %s.%s not found in result", namespace, entity)
		return nil
	}
	var names []string
	for _, m := range e.Members {
		if fd, ok := m.(*dsl.FieldDecl); ok {
			names = append(names, fd.Name)
		}
	}
	return names
}

func fieldByName(t *testing.T, src []byte, namespace, entity, field string) *dsl.FieldDecl {
	t.Helper()
	f, err := dsl.Parse("t.atl", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return findField(findEntity(f, namespace, entity), field)
}

func TestAddField(t *testing.T) {
	out, err := AddField([]byte(sampleSrc), "auth", "User", "nickname varchar(50)")
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	got := fieldNames(t, out, "auth", "User")
	want := []string{"id", "email", "created_at", "nickname"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	// Untouched content survives verbatim.
	for _, frag := range []string{"// User accounts", "index by email", "primary serial"} {
		if !bytes.Contains(out, []byte(frag)) {
			t.Errorf("missing preserved fragment %q in:\n%s", frag, out)
		}
	}
	// Inserted with two-space indent, before the closing brace.
	if !bytes.Contains(out, []byte("\n  nickname varchar(50)\n")) {
		t.Errorf("nickname not indented as expected:\n%s", out)
	}
}

func TestReplaceField(t *testing.T) {
	out, err := ReplaceField([]byte(sampleSrc), "auth", "User", "email", "email text not null")
	if err != nil {
		t.Fatalf("ReplaceField: %v", err)
	}
	fd := fieldByName(t, out, "auth", "User", "email")
	if fd == nil || fd.Type.Name != "text" {
		t.Fatalf("email type = %+v, want text", fd)
	}
	// Field set unchanged in name and order.
	if got := strings.Join(fieldNames(t, out, "auth", "User"), ","); got != "id,email,created_at" {
		t.Fatalf("fields = %s", got)
	}
	// Only the email line changed; neighbors are byte-identical.
	for _, frag := range []string{"  id          bigint primary serial\n", "  created_at  timestamptz not null default now()\n"} {
		if !bytes.Contains(out, []byte(frag)) {
			t.Errorf("neighbor line changed; missing %q in:\n%s", frag, out)
		}
	}
	if bytes.Contains(out, []byte("varchar(255)")) {
		t.Errorf("old email type still present:\n%s", out)
	}
}

func TestRemoveField(t *testing.T) {
	out, err := RemoveField([]byte(sampleSrc), "auth", "User", "created_at")
	if err != nil {
		t.Fatalf("RemoveField: %v", err)
	}
	if got := strings.Join(fieldNames(t, out, "auth", "User"), ","); got != "id,email" {
		t.Fatalf("fields = %s, want id,email", got)
	}
	if bytes.Contains(out, []byte("created_at")) {
		t.Errorf("created_at line not removed:\n%s", out)
	}
	if !bytes.Contains(out, []byte("index by email")) {
		t.Errorf("trailing member removed by mistake:\n%s", out)
	}
}

// A field whose modifiers wrap onto a second line (a `check` continuation,
// as seen in real cart/order schemas) must be treated as one span.
func TestMultiLineField(t *testing.T) {
	src := []byte(`entity Cart in consumer {
  id      varchar(9) primary
  status  varchar(20) not null default "active"
          check "status IN ('active','done')"
  note    text
}
`)
	t.Run("remove", func(t *testing.T) {
		out, err := RemoveField(src, "consumer", "Cart", "status")
		if err != nil {
			t.Fatalf("RemoveField: %v", err)
		}
		if got := strings.Join(fieldNames(t, out, "consumer", "Cart"), ","); got != "id,note" {
			t.Fatalf("fields = %s, want id,note", got)
		}
		if bytes.Contains(out, []byte("check")) {
			t.Errorf("check continuation line orphaned:\n%s", out)
		}
	})
	t.Run("replace", func(t *testing.T) {
		out, err := ReplaceField(src, "consumer", "Cart", "status", "status varchar(20) not null")
		if err != nil {
			t.Fatalf("ReplaceField: %v", err)
		}
		if bytes.Contains(out, []byte("check")) {
			t.Errorf("check continuation not replaced:\n%s", out)
		}
		if got := strings.Join(fieldNames(t, out, "consumer", "Cart"), ","); got != "id,status,note" {
			t.Fatalf("fields = %s", got)
		}
	})
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"entity not found", func() ([]byte, error) { return AddField([]byte(sampleSrc), "auth", "Ghost", "x int") }},
		{"field not found", func() ([]byte, error) { return RemoveField([]byte(sampleSrc), "auth", "User", "ghost") }},
		{"invalid field text", func() ([]byte, error) { return AddField([]byte(sampleSrc), "auth", "User", "this is !!! not valid") }},
		{"unparseable source", func() ([]byte, error) { return AddField([]byte("entity Broken in {"), "auth", "Broken", "x int") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.fn(); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// TestCorpus_RemoveEveryField walks every entity field in the real .atl
// fixtures and removes it, asserting the result re-parses and drops exactly
// that one field. This is the load-bearing check on EndByte correctness: too
// large a span swallows a neighbor (caught by the name-set diff); too small
// leaves a fragment (caught by the re-parse inside RemoveField).
func TestCorpus_RemoveEveryField(t *testing.T) {
	files := corpusFiles(t)
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := dsl.Parse(path, src)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			e, ok := d.(*dsl.EntityDecl)
			if !ok {
				continue
			}
			before := fieldNames(t, src, e.Namespace, e.Name)
			for _, target := range before {
				out, err := RemoveField(src, e.Namespace, e.Name, target)
				if err != nil {
					t.Fatalf("%s: RemoveField(%s.%s, %s): %v", path, e.Namespace, e.Name, target, err)
				}
				after := fieldNames(t, out, e.Namespace, e.Name)
				if want := remove(before, target); strings.Join(after, ",") != strings.Join(want, ",") {
					t.Fatalf("%s: removing %s.%s.%s\n got fields %v\nwant fields %v",
						path, e.Namespace, e.Name, target, after, want)
				}
			}
		}
	}
}

func corpusFiles(t *testing.T) []string {
	t.Helper()
	var all []string
	for _, pat := range []string{
		"../../../schema/consumer/internal/*/schema.atl",
		"../../../schema/vendor/internal/*/schema.atl",
	} {
		m, _ := filepath.Glob(pat)
		all = append(all, m...)
	}
	if len(all) == 0 {
		t.Skip("no .atl fixtures found")
	}
	return all
}

func remove(xs []string, x string) []string {
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
