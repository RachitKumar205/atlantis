package adminjson

import (
	"encoding/json"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The dialect is a published contract: `tide --format=json` output is what CI
// pipelines and agents parse, and the whole premise of this package is that the
// choice is made once. These assertions are the choice, written down. Changing
// one should require editing this file and noticing that you did.
//
// Assertions are on parsed values, never on bytes — protojson randomizes its
// whitespace per binary, so a golden-string test here would fail on an
// unrelated rebuild.

func TestSixtyFourBitIntegersAreQuoted(t *testing.T) {
	b, err := Marshal(&adminpb.SchemaVersionSummary{Version: 7, ChangeCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, b)
	}
	// int64 -> string, because JSON numbers lose precision above 2^53 and these
	// are compared for equality.
	if got, ok := doc["version"].(string); !ok || got != "7" {
		t.Errorf(`version = %#v, want the string "7"`, doc["version"])
	}
	// int32 stays a number: it fits, so quoting it would be noise.
	if got, ok := doc["change_count"].(float64); !ok || got != 3 {
		t.Errorf("change_count = %#v, want the number 3", doc["change_count"])
	}
}

func TestEnumsRenderAsNames(t *testing.T) {
	b, err := Marshal(&adminpb.PlanSchemaResponse{Class: adminpb.PlanClass_PLAN_CLASS_ADDITIVE})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["class"] != "PLAN_CLASS_ADDITIVE" {
		t.Errorf(`class = %#v, want "PLAN_CLASS_ADDITIVE"`, doc["class"])
	}
}

func TestFieldNamesAreSnakeCase(t *testing.T) {
	b, err := Marshal(&adminpb.PlanSchemaResponse{PlanId: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["plan_id"]; !ok {
		t.Errorf("no plan_id key; protojson defaulted to lowerCamelCase: %v", doc)
	}
}

// The #22 decision. An empty repeated field must be [] and not null, so a
// consumer can iterate it without a nil check — that failure would otherwise
// land at runtime on whichever deployment happened to have an empty list.
func TestEmptyCollectionsAreNotNull(t *testing.T) {
	b, err := Marshal(&adminpb.PlanSchemaResponse{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"parse_errors", "breaking_detail", "impact_report"} {
		v, ok := doc[key]
		if !ok {
			t.Errorf("%s is absent; want []", key)
			continue
		}
		if v == nil {
			t.Errorf("%s is null; want []", key)
			continue
		}
		if _, isSlice := v.([]any); !isSlice {
			t.Errorf("%s = %#v, want []", key, v)
		}
	}
}

// The other half of the same decision, and the reason EmitDefaultValues is used
// rather than EmitUnpopulated: a proto3 `optional` left unset stays absent, so
// "not set" remains distinguishable from "set to zero".
func TestUnsetOptionalStaysAbsent(t *testing.T) {
	b, err := Marshal(&adminpb.EntityLineageEntry{EntityId: "user"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc["removed_at"]; ok {
		t.Errorf("removed_at present as %#v on an entry that was never removed; "+
			"EmitUnpopulated would do this, EmitDefaultValues must not", v)
	}

	removed := int64(9)
	b, err = Marshal(&adminpb.EntityLineageEntry{EntityId: "user", RemovedAt: &removed})
	if err != nil {
		t.Fatal(err)
	}
	doc = nil
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["removed_at"] != "9" {
		t.Errorf(`removed_at = %#v, want the string "9"`, doc["removed_at"])
	}
}

// Without inlining, `tide diff --format=json | jq '.diff.additive'` returns
// null — and that pipeline is the reason the command has a JSON mode.
func TestMarshalIndentInliningSplicesJSONPayloads(t *testing.T) {
	b, err := MarshalIndentInlining(&adminpb.DiffSchemaVersionsResponse{
		Diff:   []byte(`{"additive":[{"kind":"add_field"}]}`),
		FromIr: []byte(`{"entities":[]}`),
	}, "diff", "from_ir", "to_ir")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Diff struct {
			Additive []struct {
				Kind string `json:"kind"`
			} `json:"additive"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("diff did not decode as JSON — still base64? %v\n%s", err, b)
	}
	if len(doc.Diff.Additive) != 1 || doc.Diff.Additive[0].Kind != "add_field" {
		t.Errorf("spliced payload lost its content: %s", b)
	}
}

// Matching is by name at any depth, so `args` renders the same way whether it
// arrives on one job or inside a list of dead ones.
func TestInliningReachesNestedFields(t *testing.T) {
	b, err := MarshalIndentInlining(&adminpb.ListDeadJobsResponse{
		Jobs: []*adminpb.JobStatus{{JobId: "j1", Args: []byte(`{"n":1}`)}},
	}, "args")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs []struct {
			Args struct {
				N int `json:"n"`
			} `json:"args"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("nested args did not decode as JSON: %v\n%s", err, b)
	}
	if len(doc.Jobs) != 1 || doc.Jobs[0].Args.N != 1 {
		t.Errorf("nested payload lost its content: %s", b)
	}
}

// A payload that is not JSON is left as protojson rendered it. Emitting null
// would turn a server-side bug into a client-side one, and hide it.
func TestInliningLeavesNonJSONPayloadsAlone(t *testing.T) {
	b, err := MarshalIndentInlining(&adminpb.JobStatus{Args: []byte{0xff, 0xfe}}, "args")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if _, isString := doc["args"].(string); !isString {
		t.Errorf("args = %#v, want the base64 string protojson produced", doc["args"])
	}
}

func TestInliningIsANoOpWithoutNamedFields(t *testing.T) {
	msg := &adminpb.JobStatus{JobId: "j1", Args: []byte(`{"n":1}`)}
	with, err := MarshalIndentInlining(msg)
	if err != nil {
		t.Fatal(err)
	}
	without, err := MarshalIndent(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(with) != string(without) {
		t.Errorf("naming no fields should not change the output:\n%s\nvs\n%s", with, without)
	}
}
