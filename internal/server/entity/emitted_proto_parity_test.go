package entity

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The dispatcher and the emitted .proto describe the same wire messages, and
// nothing but this test makes them agree.
//
// A caller's request is bytes on a wire; field numbers are the whole of what
// says which value is which. A number the two sides disagree on is not a
// version skew that fails a handshake — it decodes cleanly into the wrong
// field, or into no field at all, and the server answers a question the caller
// did not ask.
//
// Two live defects it was written after, both measured on
// id(1) / embedding(2) / title(3) / note(4):
//
//   - <E>Filter numbered its fields 1..n over the FILTERABLE columns, so a
//     vector column shifted every later predicate down one. The client's
//     `title` predicate is field 3, the dispatcher read field 3 as `note`,
//     and both are StringPredicate — it decoded without error and the query
//     filtered a column the caller never named.
//   - QueryXRequest declared filter/limit/page_token/cache_skip and not
//     order/fields/includes, so an ORDER BY arrived as an unknown field and
//     was discarded. Rows came back in primary-key order and the response
//     said nothing.
//
// The fixture is built to break both. Column numbering is only accidentally
// self-consistent on an entity whose columns are all filterable, all
// orderable, and numbered 1..n — which every fixture in this package was.
func TestDynamicDescriptorMatchesEmittedProto(t *testing.T) {
	ir := parityIR()
	files, err := codegen.EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	emitted := map[string]protoBlock{}
	for _, f := range files {
		for name, blk := range parseProtoBlocks(t, f.Content) {
			emitted[name] = blk
		}
	}

	includeRefs := schema.InboundRefs(ir)
	for i := range ir.Entities {
		e := &ir.Entities[i]
		fd, err := buildProtoDescriptors(e, includeRefs[e.ID()])
		if err != nil {
			t.Fatalf("%s: buildProtoDescriptors: %v", e.ID(), err)
		}

		msgs := fd.Messages()
		for j := range msgs.Len() {
			m := msgs.Get(j)
			compareBlock(t, string(m.Name()), emitted, messageBlock(m), allowedExtras(string(m.Name()), e))
		}
		enums := fd.Enums()
		for j := range enums.Len() {
			en := enums.Get(j)
			compareBlock(t, string(en.Name()), emitted, enumBlock(en), nil)
		}
	}
}

// Every documented column type must yield a descriptor that builds.
//
// The dispatcher's descriptors are built for the whole schema at once, so one
// column protodesc cannot resolve fails buildSnapshot and the server serves
// nothing — not the entity that declared it, not any other. `interval` did
// exactly that: setProtoType resolves it to atlantis.common.v1.Interval and
// nothing declared the import, so protodesc refused the file with "cannot
// resolve type".
//
// It survived because the two tests that build a descriptor per type both walk
// a narrower table: one skips any type whose nullable Go form is not a
// pointer, the other iterates a hand-written map of zero values that never
// listed interval. This one walks `samples`, which is held complete against
// the documented type list.
func TestEveryDocumentedTypeBuildsADescriptor(t *testing.T) {
	for name, ft := range samples {
		t.Run(name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Probe", Namespace: "probe", Kind: dsl.EntityKindRegular,
				Fields: []dsl.Field{
					{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
					{Name: "f", Type: ft, ProtoNumber: 2},
				},
			}
			if _, err := buildProtoDescriptors(e, nil); err != nil {
				t.Fatalf("a column of type %s cannot be served: %v", name, err)
			}
		})
	}
}

// The custom-query and custom-procedure descriptors have the same obligation
// as the entity one.
//
// There are three descriptor builders in this package and each carried its own
// answer to "which imports does this file need". Two of them still tested
// `Name == "timestamptz" || Name == "date"` — two of the three types coltype
// maps to a message, and none of the aliases — so a custom query taking an
// `interval` input failed to resolve and took its whole file with it.
func TestEveryDocumentedTypeBuildsACustomDescriptor(t *testing.T) {
	for name, ft := range samples {
		t.Run(name, func(t *testing.T) {
			cq := &dsl.CustomQuery{
				Name: "Probe", Owner: "probe.Thing",
				SQL:    "SELECT 1",
				Inputs: []dsl.QueryParam{{Name: "in", Type: ft}},
				Output: dsl.CustomOutput{Columns: []dsl.QueryParam{{Name: "out", Type: ft}}},
			}
			if _, err := buildCustomQueryDescs(cq, "probe", nil); err != nil {
				t.Errorf("a custom query over %s cannot be served: %v", name, err)
			}

			cp := &dsl.CustomProcedure{
				Name: "Probe", Owner: "probe.Thing",
				Inputs: []dsl.QueryParam{{Name: "in", Type: ft}},
			}
			if _, err := buildCustomProcedureDescs(cp, "probe", nil); err != nil {
				t.Errorf("a custom procedure taking %s cannot be served: %v", name, err)
			}
		})
	}
}

// allowedExtras names the members the emitted .proto declares and the
// dispatcher does not.
//
// Only the include slots on the entity message qualify. They are numbered from
// 1001, clear of any column, and the dispatcher serves no include — a response
// simply omits them. Every other absence is a field the caller can set and the
// server will not read.
func allowedExtras(msgName string, e *dsl.Entity) map[string]bool {
	if msgName != e.Name {
		return nil
	}
	out := map[string]bool{}
	for _, f := range emittedIncludeSlots(e) {
		out[f] = true
	}
	return out
}

// emittedIncludeSlots names the entity message's include slots by the same
// rule codegen builds them from: `included_<source entity>_by_<fk column>`.
func emittedIncludeSlots(e *dsl.Entity) []string {
	var out []string
	for _, refs := range schema.InboundRefs(&dsl.IR{Version: 1, Entities: parityEntities()}) {
		for _, ref := range refs {
			ns, ent, ok := strings.Cut(ref.FromEntityID, ".")
			if !ok || ns != e.Namespace {
				continue
			}
			out = append(out, "included_"+schema.SnakeCase(ent)+"_by_"+ref.FromField)
		}
	}
	return out
}

// protoBlock is one message's fields or one enum's values, by name.
type protoBlock map[string]protoMember

type protoMember struct {
	number   int32
	repeated bool
}

func (m protoMember) String() string {
	if m.repeated {
		return fmt.Sprintf("repeated =%d", m.number)
	}
	return fmt.Sprintf("=%d", m.number)
}

func messageBlock(m protoreflect.MessageDescriptor) protoBlock {
	out := protoBlock{}
	fields := m.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		out[string(f.Name())] = protoMember{number: int32(f.Number()), repeated: f.IsList()}
	}
	return out
}

func enumBlock(e protoreflect.EnumDescriptor) protoBlock {
	out := protoBlock{}
	vals := e.Values()
	for i := range vals.Len() {
		v := vals.Get(i)
		out[string(v.Name())] = protoMember{number: int32(v.Number())}
	}
	return out
}

func compareBlock(t *testing.T, name string, emitted map[string]protoBlock, got protoBlock, extras map[string]bool) {
	t.Helper()
	want, ok := emitted[name]
	if !ok {
		t.Errorf("%s: the dispatcher builds this but the emitted .proto has no such message or enum, "+
			"so no generated client can address it", name)
		return
	}
	for member, g := range got {
		w, ok := want[member]
		if !ok {
			t.Errorf("%s.%s: declared by the dispatcher (%s) and absent from the emitted .proto", name, member, g)
			continue
		}
		if g != w {
			t.Errorf("%s.%s: dispatcher has %s, emitted .proto has %s. A client's bytes for this "+
				"member land somewhere else in the dispatcher's message", name, member, g, w)
		}
	}
	for member, w := range want {
		if _, ok := got[member]; ok || extras[member] {
			continue
		}
		t.Errorf("%s.%s: declared by the emitted .proto (%s) and absent from the dispatcher, so a "+
			"client that sets it is silently ignored", name, member, w)
	}
}

var (
	protoFieldRE = regexp.MustCompile(`^\s*(repeated\s+|optional\s+)?([A-Za-z0-9_.]+)\s+([a-z][a-z0-9_]*)\s*=\s*(\d+)\s*;`)
	protoValueRE = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)\s*=\s*(\d+)\s*;`)
	protoOpenRE  = regexp.MustCompile(`^(message|enum)\s+([A-Za-z0-9_]+)\s*\{(.*)$`)
)

// parseProtoBlocks reads the emitted .proto text into one protoBlock per
// top-level message and enum.
//
// A hand-rolled reader rather than a proto compiler: the input is machine
// written with a fixed line shape, and pulling in a parser to read it would
// add a dependency this repository does not otherwise carry.
//
// Every message the emitter writes is top-level, so no nesting is handled. A
// message with a nested type would end its block early here; the test would
// then report the outer message as missing members, which fails loudly.
func parseProtoBlocks(t *testing.T, content string) map[string]protoBlock {
	t.Helper()
	out := map[string]protoBlock{}
	var cur protoBlock
	var curName, curKind string

	add := func(line string) {
		if curKind == "enum" {
			if m := protoValueRE.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[2])
				cur[m[1]] = protoMember{number: int32(n)}
			}
			return
		}
		if m := protoFieldRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[4])
			cur[m[3]] = protoMember{number: int32(n), repeated: strings.TrimSpace(m[1]) == "repeated"}
		}
	}

	for _, line := range strings.Split(content, "\n") {
		if cur != nil {
			if strings.HasPrefix(line, "}") {
				out[curName] = cur
				cur, curName, curKind = nil, "", ""
				continue
			}
			add(line)
			continue
		}
		m := protoOpenRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		curKind, curName, cur = m[1], m[2], protoBlock{}
		// `message GetDocResponse { Doc entity = 1; }` — opened and closed on
		// one line, which the emitter writes for every single-field wrapper.
		if rest := strings.TrimSpace(m[3]); rest != "" {
			body, closed := strings.CutSuffix(rest, "}")
			for _, stmt := range strings.Split(body, ";") {
				if strings.TrimSpace(stmt) != "" {
					add(stmt + ";")
				}
			}
			if closed {
				out[curName] = cur
				cur, curName, curKind = nil, "", ""
			}
		}
	}
	if cur != nil {
		t.Fatalf("unterminated %s %s in the emitted .proto", curKind, curName)
	}
	return out
}

// parityEntities is the fixture, built so that column numbering cannot be
// self-consistent by accident.
//
// Doc carries a vector (neither filterable nor orderable), an array (not
// filterable), an interval (orderable, not filterable) and a retired number,
// so the filter fields, the order variants and the columns each run out of
// step with the others. Comment gives Doc an inbound foreign key, which is
// what the DocInclude enum is numbered from. Pair has a composite key.
func parityEntities() []dsl.Entity {
	return []dsl.Entity{
		{
			Name: "Doc", Namespace: "library", Kind: dsl.EntityKindRegular,
			RetiredProtoNumbers: []int{5},
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
				{Name: "embedding", Type: dsl.FieldType{Name: "vector", VecDim: 3}, ProtoNumber: 2},
				{Name: "title", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 3},
				{Name: "tags", Type: dsl.FieldType{Name: "text", Array: true, Elem: &dsl.FieldType{Name: "text"}}, ProtoNumber: 4},
				{Name: "note", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 6},
				{Name: "created_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true, ProtoNumber: 7},
				{Name: "lifespan", Type: dsl.FieldType{Name: "interval"}, ProtoNumber: 8},
			},
		},
		{
			Name: "Comment", Namespace: "library", Kind: dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
				{Name: "doc_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, ProtoNumber: 2,
					Ref: &dsl.Ref{TargetID: "library.Doc", TargetField: "id"}},
			},
		},
		{
			Name: "Pair", Namespace: "library", Kind: dsl.EntityKindRegular,
			CompositePK: []string{"a", "b"},
			Fields: []dsl.Field{
				{Name: "a", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 1},
				{Name: "b", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
				{Name: "v", Type: dsl.FieldType{Name: "int"}, ProtoNumber: 3},
			},
		},
	}
}

func parityIR() *dsl.IR {
	return &dsl.IR{Version: 1, Entities: parityEntities()}
}

// The fixture only tests what it contains. Each of these shapes is one the
// numbering ran out of step on, so a fixture that lost one would pass while
// the defect it was written for came back.
func TestParityFixtureCoversTheShapesThatBreakNumbering(t *testing.T) {
	doc := parityEntities()[0]

	var hasUnfilterable, hasUnorderable, hasGap bool
	for _, f := range doc.Fields {
		if _, ok := predicateMessageForField(f.Type); !ok {
			hasUnfilterable = true
		}
		if !coltype.Orderable(f.Type) {
			hasUnorderable = true
		}
	}
	for i := 1; i < len(doc.Fields); i++ {
		if doc.Fields[i].ProtoNumber != doc.Fields[i-1].ProtoNumber+1 {
			hasGap = true
		}
	}

	if !hasUnfilterable {
		t.Error("no column in the fixture is unfilterable, so the filter message's numbering " +
			"is 1..n either way and cannot come apart")
	}
	if !hasUnorderable {
		t.Error("every column in the fixture is orderable, so the OrderField enum's numbering " +
			"is 1..n either way and cannot come apart")
	}
	if !hasGap {
		t.Error("the fixture's proto numbers are contiguous, so numbering by position and " +
			"numbering by proto number give the same answer")
	}
	if len(parityIR().Entities) < 2 {
		t.Error("no second entity, so no inbound foreign key and the Include enum is empty")
	}
}
