package entity

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Three functions describe the same column and must agree:
//
//	makeScanTargets        allocates the scan destination for a RETURNING clause
//	readScanTargets        dereferences it back into a Go value
//	goValueFromProtoReflect derives the same Go value from the proto message
//
// A cache id is built from BOTH the read side and the write side — the write
// path scans the row it just inserted, the read path reflects over the message
// it is about to return — so if the two produce different Go types for one
// column, runtime.CompositeID renders two different ids for the same row. The
// outbox then bumps a key nobody reads and the cached row stays stale until
// TTL, with nothing logged.
//
// They did not agree. makeScanTargets listed four types and sent everything
// else to `new(string)`. timestamptz, date, boolean and bytea cannot be scanned
// into *string at all, so Create died at the RETURNING clause; float4 and
// float8 CAN be, which was worse — a double holding 1e6 came back as the string
// "1000000" on the write path and as float64(1e6), rendered "1e+06", on the
// read path.
//
// This pins the agreement rather than any one table's contents, because the
// agreement is the property the cache id depends on.

// pkCapableTypes are the types checkPKPredicates (internal/codegen/server.go)
// names as valid primary keys, plus the two float types and the shapes that
// reach readScanTargets through inbound cache invalidation. Every one of these
// must round-trip; the list is here rather than derived so that widening
// checkPKPredicates without teaching the scan tables about the new type is a
// visible, failing change.
var pkCapableTypes = []dsl.FieldType{
	{Name: "bigint"},
	{Name: "int"},
	{Name: "smallint"},
	{Name: "text"},
	{Name: "varchar", Len: 64},
	{Name: "citext"},
	{Name: "uuid"},
	{Name: "boolean"},
	{Name: "timestamptz"},
	{Name: "date"},
	{Name: "real"},
	{Name: "double"},
	{Name: "numeric", NumP: 12, NumS: 2, HasNumP: true},
	{Name: "jsonb"},
	{Name: "bytea"},
}

func TestScanTargetsMatchGoValueFromProto(t *testing.T) {
	for _, ft := range pkCapableTypes {
		t.Run(ft.Name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Probe", Namespace: "probe", Kind: dsl.EntityKindRegular,
				Fields: []dsl.Field{
					{Name: "k", Type: ft, Primary: true, NotNull: true, ProtoNumber: 1},
					{Name: "v", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 2},
				},
			}
			meta := entityMetaFor(e, &dsl.IR{Version: 1})
			fd, err := buildProtoDescriptors(e)
			if err != nil {
				t.Fatalf("buildProtoDescriptors: %v", err)
			}
			resolveProtoDescriptors(meta, fd)

			if len(meta.pkCols) != 1 {
				t.Fatalf("expected one PK column, got %d — this case proves nothing",
					len(meta.pkCols))
			}
			cm := meta.pkCols[0]

			// Write path: allocate, then dereference as the handler does after
			// row.Scan fills it in.
			//
			// Recovered, because readScanTargets asserts the pointer type it
			// expects. If the two tables disagree the assertion panics — which
			// is how production behaves too, so it is a real signal rather
			// than a test artefact. Catching it here turns "the whole test
			// binary died on the first bad type" into one named failure per
			// type, so a single mismatch cannot hide the others.
			scanned, panicked := readTargetsRecovering(meta.pkCols)
			if panicked != nil {
				t.Fatalf("makeScanTargets and readScanTargets disagree about %s: %v — "+
					"in production this panics inside the Create handler",
					ft.Name, panicked)
			}
			if len(scanned) != 1 {
				t.Fatalf("readScanTargets returned %d values", len(scanned))
			}

			// Read path: the same column, derived from the message.
			msg := dynamicpb.NewMessage(meta.msgDesc)
			protoFD := meta.msgDesc.Fields().ByNumber(cm.protoNum)
			if protoFD == nil {
				t.Fatal("primary key has no proto field")
			}
			derived := goValueFromProtoReflect(msg, protoFD, cm)

			wantT := reflect.TypeOf(derived)
			gotT := reflect.TypeOf(scanned[0])
			if gotT != wantT {
				t.Errorf("scan path yields %v, proto path yields %v — the same row "+
					"produces two different cache ids, so invalidation bumps a key "+
					"nobody reads", gotT, wantT)
			}
		})
	}
}

// readTargetsRecovering runs the allocate-then-dereference pair and returns
// whatever panic the type assertion raised instead of letting it escape.
func readTargetsRecovering(cols []columnMeta) (out []any, recovered any) {
	defer func() { recovered = recover() }()
	return readScanTargets(cols, makeScanTargets(cols)), nil
}

// TestScanTargetsAreScannableTypes catches the other half: a target that is the
// wrong SHAPE for the column fails inside pgx at Scan, with a driver error that
// names neither the column nor the schema. Asserting the Go type here is what
// makes that failure impossible rather than merely unlikely.
func TestScanTargetsAreScannableTypes(t *testing.T) {
	want := map[string]string{
		"bigint":      "*int64",
		"int":         "*int32",
		"smallint":    "*int32",
		"text":        "*string",
		"varchar":     "*string",
		"citext":      "*string",
		"uuid":        "*string",
		"numeric":     "*string",
		"boolean":     "*bool",
		"timestamptz": "*time.Time",
		"date":        "*time.Time",
		"real":        "*float32",
		"double":      "*float64",
		"jsonb":       "*[]uint8",
		"bytea":       "*[]uint8",
	}
	for _, ft := range pkCapableTypes {
		cols := []columnMeta{{field: &dsl.Field{Name: "k", Type: ft}}}
		got := reflect.TypeOf(makeScanTargets(cols)[0]).String()
		if w, ok := want[ft.Name]; ok && got != w {
			t.Errorf("%s scans into %s, want %s", ft.Name, got, w)
		}
	}
}
