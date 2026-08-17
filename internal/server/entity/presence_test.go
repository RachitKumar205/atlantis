package entity

import (
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// No descriptor builder may call protodesc.NewFile directly.
//
// materializeProto3Optional has to run on every FileDescriptorProto this
// package builds, and "remember to call it" is not a mechanism — there are
// three builders here and the failure when one forgets is silent: the field
// simply has no presence, Has() degrades to a zero comparison, and every test
// that leaves the field unset still passes. buildFileDescriptor is the single
// door, and this is what keeps it the only one.
//
// A grep rather than a call-graph check because the property is textual: the
// hazard is a NEW call site, and a new call site is a new occurrence of this
// string. A comment naming the call with its paren would be a false positive,
// which is the direction to err in.
func TestOnlyBuildFileDescriptorCallsNewFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "protodesc.NewFile(") {
				continue
			}
			// The one permitted call lives in buildFileDescriptor, which runs
			// the pass immediately before it.
			if name == "proto.go" && strings.Contains(line, "return protodesc.NewFile(file, resolver)") {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these call protodesc.NewFile directly and so skip "+
			"materializeProto3Optional — every nullable scalar in those messages "+
			"silently loses presence. Route them through buildFileDescriptor:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// Presence reaches the built descriptor, for both kinds of message.
//
// Setting Proto3Optional is the part that looks like the whole job and is not:
// presence for a proto3 scalar lives in a synthetic one-field oneof, which
// protoc writes when it compiles a .proto and which a descriptor assembled in
// Go has to supply itself. Asserting HasPresence() rather than
// HasOptionalKeyword() is the difference — the keyword reads the flag straight
// back, so it reported true throughout the whole period when no field on any
// dispatcher message had presence at all.
func TestBuiltDescriptorsCarryPresence(t *testing.T) {
	e := &dsl.Entity{
		Name: "Doc", Namespace: "library", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
			{Name: "title", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
			{Name: "note", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 3},
			{Name: "hits", Type: dsl.FieldType{Name: "bigint"}, ProtoNumber: 4},
		},
	}
	fd, err := buildProtoDescriptors(e)
	if err != nil {
		t.Fatalf("buildProtoDescriptors: %v", err)
	}
	msg := fd.Messages().ByName("Doc")
	if msg == nil {
		t.Fatal("no Doc message in the built descriptor")
	}

	for _, name := range []string{"note", "hits"} {
		f := msg.Fields().ByName(protoreflect.Name(name))
		if f == nil {
			t.Fatalf("no field %q", name)
		}
		if !f.HasPresence() {
			t.Errorf("nullable column %q has no presence. Has() then degrades to "+
				"a zero comparison: bindColumnValue writes SQL NULL for an "+
				"explicitly-sent zero, and a NULL read back is indistinguishable "+
				"from a real one", name)
		}
		if f.ContainingOneof() == nil {
			t.Errorf("nullable column %q is not in a synthetic oneof, which is "+
				"where proto3 scalar presence lives", name)
		}
	}

	// The response's own optional field, to prove the pass covers messages the
	// entity builder assembles by hand rather than from dsl.Fields.
	qresp := fd.Messages().ByName("QueryDocResponse")
	if qresp == nil {
		t.Fatal("no QueryDocResponse message")
	}
	if te := qresp.Fields().ByName("total_estimate"); te == nil {
		t.Error("QueryDocResponse has no total_estimate field")
	} else if !te.HasPresence() {
		t.Error("total_estimate is declared proto3-optional but has no presence, " +
			"so the pass is not reaching every message on the file")
	}
}
