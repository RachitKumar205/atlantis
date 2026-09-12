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

// A checkpoint whose entity has neither a key nor `keyless` is refused at
// decode, naming the entity.
//
// This is the production shape: the checkpoint was written by a binary
// predating the keyword, so the flag is absent from the JSON and false on the
// way back in. The server never calls Lower, so before DecodeJSONIR ran these
// checks the declaration reached the descriptor builders and one entity took
// the other 96 down at startup.
func TestACheckpointWithNoKeyAndNoKeylessIsRefused(t *testing.T) {
	ir := mustLower(t, "entity Summaries in app {\n  id bigint primary\n  notes text\n}\n")

	// Drop the key the way the older binary's output lacked one: no `primary`
	// on any field, no composite, no `keyless`.
	e := &ir.Entities[0]
	for i := range e.Fields {
		e.Fields[i].Primary = false
	}
	if e.Keyless {
		t.Fatal("fixture is declared keyless, so it does not exercise the unmarked path")
	}

	data, err := ir.EncodeJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeJSONIR(data)
	if err == nil {
		t.Fatal("DecodeJSONIR accepted a checkpoint with no key and no `keyless`")
	}
	if got != nil {
		t.Error("DecodeJSONIR returned an IR alongside its error")
	}
	if !strings.Contains(err.Error(), e.ID()) {
		t.Errorf("error does not name the entity %s: %v", e.ID(), err)
	}
	if !strings.Contains(err.Error(), "keyless") {
		t.Errorf("error does not name the declaration that would fix it: %v", err)
	}
}

// A checkpoint holding a legitimately keyless entity still decodes.
//
// The check above refuses a missing key; `keyless` is how a table with no key
// is declared, and a server that refused it would stop serving every schema
// adopted from a database with such a table.
func TestAKeylessCheckpointStillDecodes(t *testing.T) {
	ir := mustLower(t, keylessSrc)
	data, err := ir.EncodeJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeJSONIR(data)
	if err != nil {
		t.Fatalf("DecodeJSONIR refused a keyless checkpoint: %v", err)
	}
	if len(got.Entities) != 1 || !got.Entities[0].Keyless {
		t.Errorf("decoded IR lost the keyless declaration: %+v", got.Entities)
	}
}
