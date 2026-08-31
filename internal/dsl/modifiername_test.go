package dsl

import "testing"

// A column may be named after a field modifier, and indentation says which one
// is meant.
//
// parseFieldModifiers consumes greedily, so `identity` on its own line was
// taken as a modifier of the field above and the type after it was left to
// start a member it cannot start. The TokCheck arm already resolves the same
// collision by column; these are the modifiers that need the same treatment.
//
// Lookahead cannot do it. In `id bigint identity` followed by `name text`,
// reading `identity name` as a field of type `name` is equally consistent, and
// `name` is a registered type.
func TestAModifierKeywordCanNameAField(t *testing.T) {
	for _, tc := range []struct{ name, word, typ string }{
		{"identity", "identity", "double"},
		{"serial", "serial", "text"},
		{"default", "default", "text"},
		{"references", "references", "text"},
		{"backfill", "backfill", "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "entity T in app {\n" +
				"  id bigint primary\n" +
				"  " + tc.word + " " + tc.typ + "\n" +
				"}\n"
			f := mustParse(t, src)
			e := f.Decls[0].(*EntityDecl)
			var names []string
			for _, m := range e.Members {
				if fd, ok := m.(*FieldDecl); ok {
					names = append(names, fd.Name)
				}
			}
			if len(names) != 2 || names[1] != tc.word {
				t.Fatalf("fields = %v, want [id %s] — the keyword was swallowed as "+
					"a modifier of the field above", names, tc.word)
			}
		})
	}
}

// The same keywords are still modifiers where they always were. A field's own
// line, and a continuation indented under it, both sit right of the field's
// column.
func TestAModifierKeywordIsStillAModifier(t *testing.T) {
	t.Run("on the field's own line", func(t *testing.T) {
		f := mustParse(t, "entity T in app {\n  id bigint identity\n  name text\n}\n")
		e := f.Decls[0].(*EntityDecl)
		fd := e.Members[0].(*FieldDecl)
		if len(fd.Modifiers) != 1 {
			t.Fatalf("id has %d modifiers, want the identity one", len(fd.Modifiers))
		}
		if _, ok := fd.Modifiers[0].(*ModIdentityDecl); !ok {
			t.Errorf("modifier = %T, want *ModIdentityDecl", fd.Modifiers[0])
		}
		if n := len(e.Members); n != 2 {
			t.Errorf("entity has %d members, want 2 — `name text` was consumed", n)
		}
	})

	t.Run("indented under the field", func(t *testing.T) {
		f := mustParse(t, "entity T in app {\n  id bigint\n      identity\n  name text\n}\n")
		e := f.Decls[0].(*EntityDecl)
		fd := e.Members[0].(*FieldDecl)
		if len(fd.Modifiers) != 1 {
			t.Fatalf("id has %d modifiers, want the indented identity", len(fd.Modifiers))
		}
		if _, ok := fd.Modifiers[0].(*ModIdentityDecl); !ok {
			t.Errorf("modifier = %T, want *ModIdentityDecl", fd.Modifiers[0])
		}
	})

	t.Run("default with its value on the field's line", func(t *testing.T) {
		f := mustParse(t, "entity T in app {\n  id bigint primary\n  s text default \"x\"\n}\n")
		e := f.Decls[0].(*EntityDecl)
		fd := e.Members[1].(*FieldDecl)
		if len(fd.Modifiers) != 1 {
			t.Fatalf("s has %d modifiers, want the default", len(fd.Modifiers))
		}
	})
}

// Two shapes this must not change.
//
// `not` stays greedy: `not null` is two tokens, `not` is not a plausible column
// name, and a bare `not null` at member indent has always bound to the field
// above. `check` keeps its own arm, which already reads the column.
func TestTheModifiersThatDoNotMove(t *testing.T) {
	t.Run("a bare not null at member indent binds to the field above", func(t *testing.T) {
		f := mustParse(t, "entity T in app {\n  id bigint\n  not null\n}\n")
		e := f.Decls[0].(*EntityDecl)
		if n := len(e.Members); n != 1 {
			t.Fatalf("entity has %d members, want 1 — `not null` became its own member", n)
		}
		fd := e.Members[0].(*FieldDecl)
		if len(fd.Modifiers) != 1 {
			t.Errorf("id has %d modifiers, want the not-null", len(fd.Modifiers))
		}
	})

	t.Run("a check at member indent is a table check", func(t *testing.T) {
		f := mustParse(t, "entity T in app {\n  id bigint primary\n  check \"id > 0\" as t_id_check\n}\n")
		e := f.Decls[0].(*EntityDecl)
		var checks int
		for _, m := range e.Members {
			if _, ok := m.(*TableCheckDecl); ok {
				checks++
			}
		}
		if checks != 1 {
			t.Errorf("got %d table checks, want 1 — it was taken as the field's modifier", checks)
		}
	})
}
