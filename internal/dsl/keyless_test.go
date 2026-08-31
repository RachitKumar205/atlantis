package dsl

import (
	"strings"
	"testing"
)

const keylessSrc = "entity Summaries in app {\n" +
	"  keyless\n" +
	"  table \"app.old_summaries\"\n" +
	"  upi   varchar(26)\n" +
	"  notes text\n" +
	"}\n"

// A table with no key is declarable, and says so.
func TestKeylessLowers(t *testing.T) {
	f := mustParse(t, keylessSrc)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	e := &ir.Entities[0]
	if !e.Keyless {
		t.Error("the entity did not carry Keyless")
	}
	if e.PrimaryField() != nil {
		t.Error("a keyless entity reported a primary field")
	}
}

// Without the keyword it is still an error, so a forgotten key fails loudly
// rather than turning an entity into a keyless one.
func TestAMissingKeyIsStillAnError(t *testing.T) {
	f := mustParse(t, "entity T in app {\n  a bigint\n  b text\n}\n")
	_, err := Lower([]*File{f})
	if err == nil {
		t.Fatal("an entity with no key and no `keyless` lowered")
	}
	if !strings.Contains(err.Error(), "keyless") {
		t.Errorf("the error does not name the way out: %v", err)
	}
}

// The two are alternatives, so a declaration cannot claim both.
func TestKeylessAndAPrimaryKeyAreRefused(t *testing.T) {
	for name, src := range map[string]string{
		"field primary": "entity T in app {\n  keyless\n  id bigint primary\n}\n",
		"primary by":    "entity T in app {\n  keyless\n  a bigint\n  b bigint\n  primary by a, b\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := mustParse(t, src)
			if _, err := Lower([]*File{f}); err == nil {
				t.Error("lowered with both a key and `keyless`")
			}
		})
	}
}

// A cached row is addressed by its key. runtime.CompositeID builds the id from
// key values, so every row of a keyless entity would cache under one id.
func TestKeylessRefusesACache(t *testing.T) {
	src := "entity T in app {\n" +
		"  keyless\n" +
		"  a bigint\n" +
		"  cache {\n" +
		"    read_through ttl=10m\n" +
		"  }\n" +
		"}\n"
	f := mustParse(t, src)
	_, err := Lower([]*File{f})
	if err == nil {
		t.Fatal("a keyless entity lowered with a cache")
	}
	if !strings.Contains(err.Error(), "cache") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// A foreign key to a keyless entity's unique column is legal, and Postgres
// agrees: it needs a unique constraint on the target, not a primary key. The
// existing primary-or-unique rule already says this, so keyless does not
// change it.
func TestAReferenceToAKeylessUniqueColumnIsAllowed(t *testing.T) {
	src := "entity Provider in app {\n" +
		"  keyless\n" +
		"  code text unique\n" +
		"}\n" +
		"entity Feature in app {\n" +
		"  id bigint primary\n" +
		"  provider text references app.Provider.code\n" +
		"}\n"
	f := mustParse(t, src)
	if _, err := Lower([]*File{f}); err != nil {
		t.Errorf("a reference to a unique column of a keyless entity was refused: %v", err)
	}
}

// And to a column that is neither, it is refused — as Postgres refuses it.
func TestAReferenceToAKeylessNonUniqueColumnIsRefused(t *testing.T) {
	src := "entity Provider in app {\n" +
		"  keyless\n" +
		"  code text\n" +
		"}\n" +
		"entity Feature in app {\n" +
		"  id bigint primary\n" +
		"  provider text references app.Provider.code\n" +
		"}\n"
	f := mustParse(t, src)
	if _, err := Lower([]*File{f}); err == nil {
		t.Error("a reference to a non-unique column lowered; Postgres refuses that key")
	}
}
