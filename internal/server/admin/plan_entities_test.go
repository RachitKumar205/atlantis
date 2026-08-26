package admin

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// The Schema page's pending-changes strip asks "what is in flight for this one
// entity", and planEntityIDs is the whole of the answer. A bucket it fails to
// read is not a rendering bug: the strip reports "nothing pending" for an entity
// with a change waiting, which is the same false negative the destructive bucket
// produced five times elsewhere in this repo before All() existed.

// TestPlanEntityIDsReadsEveryBucket is the structural guard.
//
// It builds a change in every bucket by reflection over Diff's fields, so a
// fifth bucket added later fails here rather than going unnoticed until an
// operator trusts an empty strip. It round-trips through JSON because
// planEntityIDs reads a stored diff, so a bucket All() returns but that does
// not survive marshalling is equally invisible.
func TestPlanEntityIDsReadsEveryBucket(t *testing.T) {
	typ := reflect.TypeOf(codegen.Diff{})
	d := &codegen.Diff{}
	v := reflect.ValueOf(d).Elem()

	var want []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type != reflect.TypeOf([]codegen.Change{}) {
			continue
		}
		// The entity is named after the bucket, so a failure names the
		// bucket that went missing rather than reporting a count.
		id := "ns." + f.Name
		v.Field(i).Set(reflect.ValueOf([]codegen.Change{{
			Kind: codegen.KindEntityAdded, EntityID: id,
		}}))
		want = append(want, id)
	}
	if len(want) < 4 {
		t.Fatalf("found %d change buckets on codegen.Diff; this test is not reading "+
			"what it thinks it is", len(want))
	}

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal diff: %v", err)
	}
	got := planEntityIDs(raw)

	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("planEntityIDs dropped %q.\n"+
				"  Every bucket must reach the strip: an entity whose only pending "+
				"change sits in a bucket this misses renders as 'no pending changes', "+
				"which is worse than not rendering at all.\n"+
				"  got %v", id, got)
		}
	}
}

func TestPlanEntityIDsDeduplicatesAndSorts(t *testing.T) {
	// One entity changed three ways across two buckets is one row in the
	// strip, not three, and the order must not depend on bucket order.
	raw, err := json.Marshal(&codegen.Diff{
		Additive: []codegen.Change{
			{EntityID: "shop.Order", Field: "a"},
			{EntityID: "shop.Order", Field: "b"},
			{EntityID: "billing.Invoice"},
		},
		Destructive: []codegen.Change{{EntityID: "shop.Order", Field: "c"}},
	})
	if err != nil {
		t.Fatalf("marshal diff: %v", err)
	}

	got := planEntityIDs(raw)
	want := []string{"billing.Invoice", "shop.Order"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planEntityIDs = %v, want %v", got, want)
	}
}

// TestPlanEntityIDsSkipsChangesWithNoEntity keeps the strip from growing a row
// keyed on the empty string, which would match no entity on the page and render
// as an unnamed pending change on every one of them under a loose comparison.
func TestPlanEntityIDsSkipsChangesWithNoEntity(t *testing.T) {
	raw, err := json.Marshal(&codegen.Diff{
		Additive: []codegen.Change{{EntityID: ""}, {EntityID: "shop.Order"}},
	})
	if err != nil {
		t.Fatalf("marshal diff: %v", err)
	}

	got := planEntityIDs(raw)
	if want := []string{"shop.Order"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planEntityIDs = %v, want %v", got, want)
	}
}

// Pins the choice to list the plan without its entities rather than fail the
// whole queue: a row whose diff will not decode was edited outside this server,
// and refusing to serve the approval queue leaves nothing approvable.
//
// A renamed bucket decodes to empty and returns no error, so this arm is not
// the guard for that. TestPlanEntityIDsReadsEveryBucket is.
func TestPlanEntityIDsToleratesAnUndecodableDiff(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"not json", []byte("{{{")},
		{"json of the wrong shape", []byte(`["additive"]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planEntityIDs(tc.raw); len(got) != 0 {
				t.Errorf("planEntityIDs(%s) = %v, want none", tc.name, got)
			}
		})
	}
}
