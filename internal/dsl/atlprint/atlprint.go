// Package atlprint performs surgical, formatting-preserving edits to .atl
// source text.
//
// The atlantis pipeline is one-way: .atl source → AST → validated IR →
// SQL/codegen. There is no IR→.atl printer, and re-emitting a whole file
// from the AST would discard comments, alignment, and blank lines that
// callers care about. The console's schema editor needs the opposite: take
// the caller's existing .atl verbatim, change exactly one declaration, and
// leave every other byte untouched so the resulting git diff is minimal and
// reviewable.
//
// The approach is a byte-level splice. The parser records a start byte
// (Position.Byte) and an end byte (EndByte) for entities and fields; an edit
// computes the target's [start, end) span and replaces only that slice. All
// surrounding text — comments, whitespace, other declarations — is copied
// through unchanged because it never enters the edited span.
//
// Every operation re-parses its result and fails if the splice produced
// syntactically invalid .atl, so a successful return is always parseable.
package atlprint

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// AddField inserts fieldText as a new field at the end of the named entity's
// body, just before the closing brace, indented to match existing members.
// fieldText is the field's own declaration (e.g. `nickname varchar(50)`);
// surrounding indentation and the trailing newline are supplied by atlprint.
func AddField(src []byte, namespace, entity, fieldText string) ([]byte, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	e := findEntity(f, namespace, entity)
	if e == nil {
		return nil, fmt.Errorf("entity %s.%s not found", namespace, entity)
	}
	if e.EndByte <= 0 {
		return nil, fmt.Errorf("entity %s.%s has no recorded end position (unterminated?)", namespace, entity)
	}

	closeBrace := e.EndByte - 1 // index of the `}`
	braceLine := lineStart(src, closeBrace)
	if braceLine <= e.Pos.Byte {
		// Opening `entity` keyword and closing `}` share a line: a
		// single-line entity. Line-oriented insertion would corrupt it.
		return nil, fmt.Errorf("entity %s.%s is single-line; AddField needs a multi-line body", namespace, entity)
	}

	indent := memberIndent(src, e, braceLine, closeBrace)
	insertion := indent + strings.TrimSpace(fieldText) + "\n"

	out := splice(src, braceLine, braceLine, insertion)
	return validate(out, namespace, entity)
}

// ReplaceField replaces the named field's declaration with newText, preserving
// the field's leading indentation and everything after it. newText is the
// field's own declaration text, without leading indentation or trailing
// newline.
func ReplaceField(src []byte, namespace, entity, field, newText string) ([]byte, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	e := findEntity(f, namespace, entity)
	if e == nil {
		return nil, fmt.Errorf("entity %s.%s not found", namespace, entity)
	}
	fd := findField(e, field)
	if fd == nil {
		return nil, fmt.Errorf("field %q not found in entity %s.%s", field, namespace, entity)
	}

	start := fd.Pos.Byte
	end := trimTrailingSpace(src, fd.EndByte)
	out := splice(src, start, end, strings.TrimSpace(newText))
	return validate(out, namespace, entity)
}

// RemoveField deletes the named field's declaration along with the source
// line(s) it occupies (leading indentation through the terminating newline).
func RemoveField(src []byte, namespace, entity, field string) ([]byte, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	e := findEntity(f, namespace, entity)
	if e == nil {
		return nil, fmt.Errorf("entity %s.%s not found", namespace, entity)
	}
	fd := findField(e, field)
	if fd == nil {
		return nil, fmt.Errorf("field %q not found in entity %s.%s", field, namespace, entity)
	}

	// Remove from the start of the field's first line through the end of
	// the line holding its last byte (including that line's newline).
	start := lineStart(src, fd.Pos.Byte)
	last := trimTrailingSpace(src, fd.EndByte)
	end := lineEnd(src, last)
	out := splice(src, start, end, "")
	return validate(out, namespace, entity)
}

// ---- internal helpers ----

func parse(src []byte) (*dsl.File, error) {
	f, err := dsl.Parse("edit.atl", src)
	if err != nil {
		return nil, fmt.Errorf("source does not parse cleanly; refusing to edit: %w", err)
	}
	return f, nil
}

// validate re-parses an edited result and returns it only if it is still
// valid .atl. A parse failure here means the splice produced broken syntax —
// a bug or a malformed caller-supplied field — and must not be returned.
func validate(out []byte, namespace, entity string) ([]byte, error) {
	if _, err := dsl.Parse("edit.atl", out); err != nil {
		return nil, fmt.Errorf("edit to %s.%s produced invalid .atl: %w", namespace, entity, err)
	}
	return out, nil
}

func findEntity(f *dsl.File, namespace, name string) *dsl.EntityDecl {
	for _, d := range f.Decls {
		if e, ok := d.(*dsl.EntityDecl); ok && e.Namespace == namespace && e.Name == name {
			return e
		}
	}
	return nil
}

func findField(e *dsl.EntityDecl, name string) *dsl.FieldDecl {
	for _, m := range e.Members {
		if fd, ok := m.(*dsl.FieldDecl); ok && fd.Name == name {
			return fd
		}
	}
	return nil
}

// memberIndent returns the indentation to apply to a newly inserted member:
// the leading whitespace of the last existing field, or — for an empty
// entity body — the closing brace's indentation plus two spaces.
func memberIndent(src []byte, e *dsl.EntityDecl, braceLine, closeBrace int) string {
	for i := len(e.Members) - 1; i >= 0; i-- {
		if fd, ok := e.Members[i].(*dsl.FieldDecl); ok {
			return leadingIndent(src, fd.Pos.Byte)
		}
	}
	return string(src[braceLine:closeBrace]) + "  "
}

// splice replaces src[start:end) with repl.
func splice(src []byte, start, end int, repl string) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(repl))
	out = append(out, src[:start]...)
	out = append(out, repl...)
	out = append(out, src[end:]...)
	return out
}

// lineStart returns the index of the first byte of the line containing pos
// (the byte after the preceding newline, or 0).
func lineStart(src []byte, pos int) int {
	if pos > len(src) {
		pos = len(src)
	}
	if i := bytes.LastIndexByte(src[:pos], '\n'); i >= 0 {
		return i + 1
	}
	return 0
}

// lineEnd returns the index just past the newline that terminates the line
// containing pos, or len(src) if pos is on the final unterminated line.
func lineEnd(src []byte, pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos >= len(src) {
		return len(src)
	}
	if i := bytes.IndexByte(src[pos:], '\n'); i >= 0 {
		return pos + i + 1
	}
	return len(src)
}

// trimTrailingSpace walks end backwards past ASCII whitespace, returning the
// offset just past the last non-whitespace byte before end.
func trimTrailingSpace(src []byte, end int) int {
	if end > len(src) {
		end = len(src)
	}
	for end > 0 && isASCIISpace(src[end-1]) {
		end--
	}
	return end
}

// leadingIndent returns the run of spaces/tabs at the start of the line
// containing pos (i.e. the indentation before the line's first token).
func leadingIndent(src []byte, pos int) string {
	ls := lineStart(src, pos)
	i := ls
	for i < pos && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[ls:i])
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
