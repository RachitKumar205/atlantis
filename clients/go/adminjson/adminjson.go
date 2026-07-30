// Package adminjson is the one JSON encoding of admin API messages.
//
// `tide --format=json` and `tidectl --format=json` render admin API messages
// through this package. Two more surfaces will: the console BFF's HTTP
// responses, which still emit the older hand-marshalled shape, and the OpenAPI
// document, which does not exist yet. Both are tracked separately.
//
// The point of a shared encoder is that those surfaces cannot disagree once
// they are on it. A disagreement between them would be invisible until
// something downstream broke on a shape it had been told to expect.
//
// # Why protojson rather than encoding/json
//
// encoding/json over a generated struct is tempting: protoc-gen-go emits
// snake_case tags, so the output looks almost identical to the hand-rolled
// structs this replaced. It is wrong in two ways that matter. Enum fields
// serialize as integers, so `plan_class` becomes `2` and every consumer has to
// carry its own copy of the enum's numbering. And `omitempty` on generated
// fields means a field's presence in the output depends on its value, which is
// not a property any documented API should have.
//
// protojson implements the proto3 JSON mapping, which is a specification rather
// than a consequence of struct tags. Enums are their names, and the same
// specification is what an OpenAPI document generated from this proto will
// describe.
//
// # The int64 change
//
// The proto3 JSON mapping renders 64-bit integers as strings — `"version":
// "7"`, not `"version": 7`. This is not a protojson quirk: JSON numbers are
// IEEE-754 doubles, which silently lose precision above 2^53, and every 64-bit
// field here (schema versions, timestamps, row counts) is one an agent or a
// script may compare for equality. Quoting them is the standard's answer, and
// it is the correct one.
//
// It is also a break from what these commands used to print. Taken
// deliberately, once, while the installed base is small enough to absorb it.
//
// # Whitespace is not stable
//
// protojson randomizes its whitespace — an extra space after a comma in compact
// mode, after a key in indented mode — seeded from a hash of the running binary
// (see protobuf-go's internal/detrand). Two builds of the same tide release can
// differ. Byte-comparing this output, in a golden test or a diff, will fail
// eventually and for no reason; compare parsed values.
package adminjson

import (
	"encoding/base64"
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// marshaler is the settled dialect.
//
// UseProtoNames keeps snake_case (`plan_class`) rather than protojson's default
// lowerCamelCase (`planClass`). The proto field names are what the .atl files,
// the SQL columns, and the docs already use; a second casing convention
// appearing only in JSON output would be a thing every reader has to translate.
//
// EmitDefaultValues restores the empty-collection shapes that protobuf itself
// cannot distinguish from absent ones. Without it an empty list is an absent
// key, and a consumer that iterates it has to handle null where it expects an
// array — the failure lands in the consumer, at runtime, on the one deployment
// where the list happened to be empty. It deliberately does not emit null for
// presence-sensing fields, so a proto3 `optional` left unset stays absent and
// keeps meaning "not set" rather than "set to nothing".
var marshaler = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
}

// indented is marshaler with two-space indentation, for terminal output.
var indented = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
	Indent:            "  ",
}

// Marshal renders m as compact JSON.
func Marshal(m proto.Message) ([]byte, error) { return marshaler.Marshal(m) }

// MarshalIndent renders m as indented JSON, for a human reading it in a
// terminal.
func MarshalIndent(m proto.Message) ([]byte, error) { return indented.Marshal(m) }

// MarshalIndentInlining is MarshalIndent with the named `bytes` fields spliced
// in as JSON rather than base64, at any depth.
//
// The proto3 mapping renders `bytes` as base64, which is right for a field that
// holds arbitrary octets and wrong for these. Several admin responses carry a
// JSON document in a `bytes` field — the IR, a schema diff, a job's arguments —
// and they are `bytes` rather than google.protobuf.Struct for a specific
// reason: Struct reorders keys, and these documents are content-hash inputs, so
// reordering would change a hash for a schema nobody edited. Byte-exactness on
// the wire is the requirement; base64 in a terminal is the accident.
//
// Without this, `tide diff --format=json | jq '.diff.additive'` returns null,
// and that pipeline is the entire reason the command has a JSON mode.
//
// Matching is by field name at any depth, not by path. `args` names a JSON
// payload whether it appears on a single job or inside a list of dead ones, and
// the alternative — a path per call site — is how `tide job status` and `tide
// job dead` would end up rendering the same field two different ways.
//
// A named field that is absent, empty, or not valid JSON is left exactly as
// protojson rendered it. Silently emitting `null` for a payload that failed to
// parse would turn a server bug into a client one.
func MarshalIndentInlining(m proto.Message, fields ...string) ([]byte, error) {
	b, err := MarshalIndent(m)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return b, nil
	}
	// Round-trip through encoding/json rather than editing protojson's output
	// textually: a base64 payload that happens to contain the field name cannot
	// confuse a parsed tree, and it can a regex.
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return b, nil //nolint:nilerr // unparseable output is protojson's problem, not ours
	}
	want := make(map[string]bool, len(fields))
	for _, f := range fields {
		want[f] = true
	}
	inlined, changed := inlineBase64JSON(doc, want)
	if !changed {
		return b, nil
	}
	return json.MarshalIndent(inlined, "", "  ")
}

// inlineBase64JSON walks a decoded JSON tree, replacing any string value under
// a named key with the JSON it base64-decodes to.
func inlineBase64JSON(node any, want map[string]bool) (any, bool) {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		for key, child := range v {
			if want[key] {
				if s, ok := child.(string); ok && s != "" {
					if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && json.Valid(decoded) {
						v[key] = json.RawMessage(decoded)
						changed = true
						continue
					}
				}
			}
			if replaced, sub := inlineBase64JSON(child, want); sub {
				v[key] = replaced
				changed = true
			}
		}
		return v, changed
	case []any:
		changed := false
		for i, child := range v {
			if replaced, sub := inlineBase64JSON(child, want); sub {
				v[i] = replaced
				changed = true
			}
		}
		return v, changed
	default:
		return node, false
	}
}
