package codegen

import (
	"regexp"
	"testing"
)

// An emitted .proto that names a well-known type it does not import is source
// protoc rejects, and nothing here compiles what the emitters produce — no
// .atl fixture is committed and gen/ is untracked, so `EmitProto` returning a
// nil error only means the strings were built.
//
// That gap shipped: `interval` maps to google.protobuf.Duration, emitProtoEntity
// imports timestamp.proto and nothing else, and duration.proto appears nowhere
// in this package. The generated file died at `tide generate` with
// `"google.protobuf.Duration" is not defined` — after `tide apply` had already
// migrated the database.
//
// This checks the property protoc would check, without needing protoc: every
// google.protobuf.X the emitted text references must have a matching import
// line in the same file. It generalises past Duration, so the next well-known
// type added to a mapping table cannot repeat this.

var (
	wellKnownRef    = regexp.MustCompile(`google\.protobuf\.([A-Za-z]+)`)
	wellKnownImport = regexp.MustCompile(`import\s+"google/protobuf/([a-z_]+)\.proto"`)
)

// wellKnownFile maps a message name to the file that defines it.
var wellKnownFile = map[string]string{
	"Timestamp": "timestamp",
	"Duration":  "duration",
	"FieldMask": "field_mask",
	"Any":       "any",
	"Struct":    "struct",
	"Empty":     "empty",
}

func assertImportsCoverReferences(t *testing.T, path, content string) {
	t.Helper()

	imported := map[string]bool{}
	for _, m := range wellKnownImport.FindAllStringSubmatch(content, -1) {
		imported[m[1]] = true
	}

	seen := map[string]bool{}
	for _, m := range wellKnownRef.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true

		file, known := wellKnownFile[name]
		if !known {
			t.Errorf("%s references google.protobuf.%s, which this test has no "+
				"defining file for — add it to wellKnownFile rather than letting "+
				"an unimported well-known type through", path, name)
			continue
		}
		if !imported[file] {
			t.Errorf("%s references google.protobuf.%s but does not import "+
				"google/protobuf/%s.proto — protoc rejects this file with "+
				"%q is not defined", path, name, file, "google.protobuf."+name)
		}
	}
}

func TestEmittedProtoImportsEveryWellKnownTypeItUses(t *testing.T) {
	// One entity per shape that reaches a well-known type. `interval` is
	// included because it is documented and it parses.
	for _, tc := range []struct{ name, src string }{
		{"timestamptz", `entity A in app { id bigint primary  at timestamptz }`},
		{"date", `entity A in app { id bigint primary  d date }`},
		{"interval", `entity A in app { id bigint primary  dur interval }`},
		{"mixed", `entity A in app { id bigint primary  at timestamptz  dur interval }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := lower(t, tc.src)
			AssignProtoNumbers(nil, ir)
			files, err := EmitProto(ir)
			if err != nil {
				t.Fatalf("EmitProto: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("EmitProto produced no files, so this case checks nothing")
			}
			for _, f := range files {
				assertImportsCoverReferences(t, f.Path, f.Content)
			}
		})
	}
}

// TestEmittedCustomQueryProtoImportsEveryWellKnownType covers the SECOND
// emitter. EmitProto writes entity files; EmitCustomProto writes the
// per-namespace custom-query file, and it keeps its own import list — it had
// the same missing-duration gap, found only because this test was pointed at
// both rather than at the one that happened to be under review.
func TestEmittedCustomQueryProtoImportsEveryWellKnownType(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"interval output", `
entity Span in timing { id bigint primary  dur interval }
query LongSpans for Span {
  input { min: bigint }
  output { id: bigint, dur: interval }
  sql touches(Span) { SELECT id, dur FROM timing_span WHERE id >= $min }
}`},
		{"interval input", `
entity Span in timing { id bigint primary  dur interval }
query SpansOver for Span {
  input { floor: interval }
  output { id: bigint }
  sql touches(Span) { SELECT id FROM timing_span WHERE dur >= $floor }
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := lower(t, tc.src)
			AssignProtoNumbers(nil, ir)
			files, err := EmitCustomProto(ir)
			if err != nil {
				t.Fatalf("EmitCustomProto: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("EmitCustomProto produced no files, so this case checks nothing")
			}
			for _, f := range files {
				assertImportsCoverReferences(t, f.Path, f.Content)
			}
		})
	}
}

// TestEmittedProtoImportCheckerCatchesAMissingImport drives the checker above
// to a failure, so a checker that always passes does not read as coverage.
func TestEmittedProtoImportCheckerCatchesAMissingImport(t *testing.T) {
	const bad = `syntax = "proto3";
import "google/protobuf/timestamp.proto";
message M {
  google.protobuf.Timestamp at = 1;
  google.protobuf.Duration  dur = 2;
}
`
	var fake testing.T
	assertImportsCoverReferences(&fake, "bad.proto", bad)
	if !fake.Failed() {
		t.Error("the import checker passed a file that references Duration without " +
			"importing duration.proto — it cannot catch the defect it exists for")
	}
}
