package dsl

import "testing"

// The cache block's keywords may name a field, which is what the grammar
// reference calls contextual: "keywords only inside specific blocks and may
// otherwise be used as identifiers".
//
// They were refused everywhere, so a column called `tag` — ordinary in any
// schema that labels rows — took its table out of an import with the same
// message a genuinely unspellable name gets.
//
// None of them begins an entity member, so nothing has to tell them apart from
// one: unlike the field modifiers, position does not come into it.
func TestCacheKeywordsCanNameAField(t *testing.T) {
	for _, w := range []string{"read_through", "ttl", "tag"} {
		t.Run(w, func(t *testing.T) {
			src := "entity T in app {\n  id bigint primary\n  " + w + " text\n}\n"
			f := mustParse(t, src)
			e := f.Decls[0].(*EntityDecl)
			var got []string
			for _, m := range e.Members {
				if fd, ok := m.(*FieldDecl); ok {
					got = append(got, fd.Name)
				}
			}
			if len(got) != 2 || got[1] != w {
				t.Fatalf("fields = %v, want [id %s]", got, w)
			}
		})
	}
}

// And the cache block still reads them as its own.
func TestCacheKeywordsStillWorkInsideTheBlock(t *testing.T) {
	src := "entity T in app {\n" +
		"  id bigint primary\n" +
		"  tag text\n" +
		"  cache {\n" +
		"    read_through ttl=10m tag=\"t:{id}\"\n" +
		"  }\n" +
		"}\n"
	f := mustParse(t, src)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	e := &ir.Entities[0]
	if e.Cache == nil {
		t.Fatal("the cache block was not lowered")
	}
	if e.FindField("tag") == nil {
		t.Error("the field named tag was lost")
	}
}
