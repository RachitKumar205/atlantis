// Package adminjson is the one JSON encoding of admin API messages.
//
// `tide --format=json` and `tidectl --format=json` render admin API messages
// through this package. The console BFF still emits its own hand-marshalled
// shape, so those two surfaces can disagree until it moves here.
//
// protojson, not encoding/json. protoc-gen-go emits snake_case tags, so
// encoding/json over a generated struct looks almost right, and is wrong twice:
// an enum field serializes as its integer, making `plan_class` a `2` that every
// consumer must map itself, and `omitempty` makes a field's presence depend on
// its value. protojson implements the proto3 JSON mapping — a specification,
// and the one an OpenAPI document generated from this proto describes.
//
// That mapping renders 64-bit integers as strings: `"version": "7"`, not
// `"version": 7`. JSON numbers are IEEE-754 doubles and lose precision above
// 2^53, and every 64-bit field here — schema versions, timestamps, row counts —
// is one a script may compare for equality.
//
// Whitespace is not stable. protojson randomizes it — an extra space after a
// comma in compact mode, after a key in indented mode — seeded from a hash of
// the running binary (protobuf-go's internal/detrand), so two builds of one
// tide release differ. Compare parsed values, not bytes.
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
// EmitDefaultValues restores the empty-collection shapes protobuf cannot
// distinguish from absent ones. Without it an empty list is an absent key, and
// a consumer iterating it meets null where it expects an array — at runtime, on
// the one deployment where that list is empty.
//
// It emits no null for presence-sensing fields, so a proto3 `optional` left
// unset stays absent and keeps meaning "not set" rather than "set to nothing".
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
// reordering changes a hash for an unedited schema. Byte-exactness on the wire
// is the requirement, and base64 in a terminal is what it costs.
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
