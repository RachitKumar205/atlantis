package admin

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func TestRequiredExtensions_VectorTrigger(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Doc", Namespace: "search", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "embedding", Type: dsl.FieldType{Name: "vector", VecDim: 1536}},
		},
	}}}
	got := requiredExtensions(ir)
	if len(got) != 1 || got[0].Name != "vector" {
		t.Fatalf("expected [vector], got %+v", got)
	}
	if !strings.Contains(got[0].Trigger, "search.Doc") || !strings.Contains(got[0].Trigger, "vector(1536)") {
		t.Errorf("trigger should name the entity + dim, got %q", got[0].Trigger)
	}
}

func TestRequiredExtensions_HypertableTrigger(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Event", Namespace: "audit", Kind: dsl.EntityKindHypertable,
		Fields: []dsl.Field{{Name: "ts", Type: dsl.FieldType{Name: "timestamptz"}}},
	}}}
	got := requiredExtensions(ir)
	if len(got) != 1 || got[0].Name != "timescaledb" {
		t.Fatalf("expected [timescaledb], got %+v", got)
	}
	if !strings.Contains(got[0].Trigger, "audit.Event") {
		t.Errorf("trigger should name the entity, got %q", got[0].Trigger)
	}
}

func TestRequiredExtensions_CitextTrigger(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "User", Namespace: "consumer", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "email", Type: dsl.FieldType{Name: "citext"}, Unique: true},
		},
	}}}
	got := requiredExtensions(ir)
	if len(got) != 1 || got[0].Name != "citext" {
		t.Fatalf("expected [citext], got %+v", got)
	}
}

func TestRequiredExtensions_AllThreeAtOnce(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{
		{Name: "User", Namespace: "consumer", Kind: dsl.EntityKindRegular,
			Fields: []dsl.Field{{Name: "email", Type: dsl.FieldType{Name: "citext"}}}},
		{Name: "Event", Namespace: "audit", Kind: dsl.EntityKindHypertable,
			Fields: []dsl.Field{{Name: "ts", Type: dsl.FieldType{Name: "timestamptz"}}}},
		{Name: "Doc", Namespace: "search", Kind: dsl.EntityKindRegular,
			Fields: []dsl.Field{{Name: "emb", Type: dsl.FieldType{Name: "vector", VecDim: 768}}}},
	}}
	got := requiredExtensions(ir)
	names := make([]string, len(got))
	for i, r := range got {
		names[i] = r.Name
	}
	want := []string{"citext", "timescaledb", "vector"}
	if len(names) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("order[%d]: got %q want %q (must be sorted for deterministic plan responses)", i, n, want[i])
		}
	}
}

func TestRequiredExtensions_EmptyIR(t *testing.T) {
	if got := requiredExtensions(nil); got != nil {
		t.Errorf("nil IR should return nil, got %+v", got)
	}
	if got := requiredExtensions(&dsl.IR{}); len(got) != 0 {
		t.Errorf("empty IR should return empty slice, got %+v", got)
	}
}

func TestRequiredExtensions_VanillaSchemaTriggersNothing(t *testing.T) {
	// A text/bigint/jsonb-only schema must not require vector or
	// timescaledb. Today's codegen.EmitInitial unconditionally emits
	// CREATE EXTENSION for both — this test pins the contract that the
	// new detector will not.
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Note", Namespace: "consumer", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "title", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "jsonb"}},
		},
	}}}
	if got := requiredExtensions(ir); len(got) != 0 {
		t.Errorf("vanilla schema should require zero extensions, got %+v", got)
	}
}

func TestClassifyExtensions(t *testing.T) {
	required := []extensionReq{
		{Name: "vector", Trigger: "consumer.Doc field embedding is vector(1536)"},
		{Name: "timescaledb", Trigger: "audit.Event is a hypertable"},
		{Name: "citext", Trigger: "consumer.User field email is citext"},
	}
	installed := map[string]bool{"vector": true}                 // already enabled
	available := map[string]bool{"vector": true, "citext": true} // citext installable; timescaledb missing

	got := classifyExtensions(required, installed, available)
	if len(got) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(got))
	}
	byName := map[string]extensionStatus{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["vector"].Action != "ok" {
		t.Errorf("vector should be ok (already installed), got %q", byName["vector"].Action)
	}
	if byName["citext"].Action != "enable" {
		t.Errorf("citext should be enable (available, not installed), got %q", byName["citext"].Action)
	}
	if byName["timescaledb"].Action != "missing" {
		t.Errorf("timescaledb should be missing (not available), got %q", byName["timescaledb"].Action)
	}
	if !strings.Contains(byName["timescaledb"].InstallHint, "timescale.com") {
		t.Errorf("timescaledb hint should point at install docs, got %q", byName["timescaledb"].InstallHint)
	}
}

func TestExtensionsMissingError_LayoutSurvivesCopyPaste(t *testing.T) {
	// The error is what operators paste into a runbook. Format should
	// include the trigger AND the install hint per extension, plus the
	// promise that atlantis auto-enables once the OS package lands.
	missing := []extensionStatus{
		{
			Name:        "timescaledb",
			Trigger:     "audit.Event is a hypertable",
			Action:      "missing",
			InstallHint: osInstallHint("timescaledb"),
		},
	}
	err := extensionsMissingError(missing)
	s := err.Error()
	if !strings.Contains(s, "timescaledb") || !strings.Contains(s, "audit.Event is a hypertable") {
		t.Errorf("error must name the extension + trigger, got:\n%s", s)
	}
	if !strings.Contains(s, "timescale.com") {
		t.Errorf("error must include the install hint URL, got:\n%s", s)
	}
	if !strings.Contains(s, "atlantis will enable each extension automatically") {
		t.Errorf("error must promise auto-enable after OS install, got:\n%s", s)
	}
}

func TestOsInstallHint_KnownNamesReturnSpecific(t *testing.T) {
	tests := []struct{ name, mustContain string }{
		{"vector", "pgvector"},
		{"timescaledb", "timescale.com"},
		{"citext", "postgresql-contrib"},
		{"made_up", "made_up"},
	}
	for _, tc := range tests {
		if got := osInstallHint(tc.name); !strings.Contains(got, tc.mustContain) {
			t.Errorf("osInstallHint(%q) = %q, want substring %q", tc.name, got, tc.mustContain)
		}
	}
}

func TestQuoteIdent_RejectsEmbeddedQuote(t *testing.T) {
	// Defensive: extension names come from our closed set today, but if
	// a future addition slipped a literal quote into the list, quoteIdent
	// must double it.
	if got := quoteIdent(`vec"tor`); got != `"vec""tor"` {
		t.Errorf("quoteIdent embedded quote: got %q want %q", got, `"vec""tor"`)
	}
}
