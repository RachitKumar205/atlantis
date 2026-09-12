package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// A keyless entity gets a table and no API.
//
// The emitters are asked one by one rather than through a single clean run:
// an artifact naming the entity is a service whose SQL addresses a row it
// cannot find, and a new emitter that forgets servesAPI fails here rather than
// at the customer's first request.
func TestNoAPIArtifactNamesAKeylessEntity(t *testing.T) {
	src := "entity Summaries in app {\n" +
		"  keyless\n" +
		"  upi   varchar(26)\n" +
		"  notes text\n" +
		"}\n" +
		"entity Keyed in app {\n" +
		"  id  bigint primary\n" +
		"  label text\n" +
		"}\n"
	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	AssignProtoNumbers(nil, ir)
	cfg := GenConfig{ModulePrefix: InRepoModulePrefix}

	// Every generator that produces a caller-facing artifact.
	protos, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	servers, err := EmitGoServer(ir, cfg)
	if err != nil {
		t.Fatalf("EmitGoServer: %v", err)
	}
	clients, err := EmitGoClient(ir, cfg)
	if err != nil {
		t.Fatalf("EmitGoClient: %v", err)
	}
	keys, err := EmitGoCacheKeys(ir)
	if err != nil {
		t.Fatalf("EmitGoCacheKeys: %v", err)
	}

	type artifact struct{ what, path, body string }
	var all []artifact
	for _, p := range protos {
		all = append(all, artifact{"proto", p.Path, p.Content})
	}
	for _, g := range servers {
		all = append(all, artifact{"server", g.Path, g.Content})
	}
	for _, g := range clients {
		all = append(all, artifact{"client", g.Path, g.Content})
	}
	for _, g := range keys {
		all = append(all, artifact{"cache keys", g.Path, g.Content})
	}

	var sawKeyed bool
	for _, a := range all {
		if strings.Contains(a.path, "summaries") || strings.Contains(a.body, "Summaries") {
			t.Errorf("%s artifact %s names the keyless entity:\n%s", a.what, a.path, a.body)
		}
		if strings.Contains(a.body, "Keyed") {
			sawKeyed = true
		}
	}
	if !sawKeyed {
		t.Fatal("no artifact named the keyed entity either, so this test proves nothing")
	}
}

// The table is still atlantis's to own: DDL, and therefore plan, apply and
// drift.
func TestAKeylessEntityStillGetsItsTable(t *testing.T) {
	src := "entity Summaries in app {\n  keyless\n  upi varchar(26)\n  notes text\n}\n"
	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if !strings.Contains(scripts.Up, `"atlantis"."app_summaries"`) {
		t.Errorf("no table was created for the keyless entity:\n%s", scripts.Up)
	}
	if strings.Contains(scripts.Up, "PRIMARY KEY") {
		t.Errorf("a primary key was emitted for a keyless entity:\n%s", scripts.Up)
	}
	if !strings.Contains(scripts.Up, `"upi"`) || !strings.Contains(scripts.Up, `"notes"`) {
		t.Errorf("the columns were not emitted:\n%s", scripts.Up)
	}
}

// Adding a keyless entity is a change the differ reports, so a plan does not
// say "0 changes" for a table it is about to create.
func TestAKeylessEntityIsDiffed(t *testing.T) {
	lower := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("t.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	empty := lower("entity Keyed in app {\n  id bigint primary\n}\n")
	withKeyless := lower("entity Keyed in app {\n  id bigint primary\n}\n" +
		"entity Summaries in app {\n  keyless\n  upi varchar(26)\n}\n")

	d := ComputeDiff(empty, withKeyless)
	var added bool
	for _, c := range d.All() {
		if c.Kind == KindEntityAdded && c.EntityID == "app.Summaries" {
			added = true
		}
	}
	if !added {
		t.Errorf("adding a keyless entity produced no change: %v", d.All())
	}
}

// An entity with no key and no `keyless` is refused by the proto emitter.
//
// servesAPI is true for it, so the six RPCs are emitted and every one addresses
// a row by key. An `id` column was substituted for the missing key: a
// discovered table often has one that is not part of its key, so the emitted
// contract compiled and addressed rows by a column no caller named, and where
// no such column existed the generated server failed at its first request.
//
// Hand-built rather than parsed, because Lower refuses this declaration — which
// is why it reaches the emitter only from a checkpoint.
func TestTheProtoEmitterRefusesAnEntityWithNoKey(t *testing.T) {
	ir := &dsl.IR{Version: 1, Entities: []dsl.Entity{{
		Name: "Summaries", Namespace: "app", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, ProtoNumber: 1},
			{Name: "notes", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 2},
		},
	}}}

	out, err := EmitProto(ir)
	if err == nil {
		t.Fatalf("EmitProto accepted an entity with no key columns and emitted %d file(s)", len(out))
	}
	if !strings.Contains(err.Error(), "app.Summaries") {
		t.Errorf("error does not name the entity: %v", err)
	}
	if !strings.Contains(err.Error(), "keyless") {
		t.Errorf("error does not name the declaration that would fix it: %v", err)
	}
	// EmitProto returns nil on the first entity that fails, so there is nothing
	// to inspect for a fabricated key: the refusal is the whole assertion.
	if out != nil {
		t.Errorf("EmitProto returned %d file(s) alongside its error", len(out))
	}
}
