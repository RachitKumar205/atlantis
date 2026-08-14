package admin

import (
	"context"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The per-CN mutation and operator allowlists these tests used to cover are
// gone: authorization is now a capability grant checked by the interceptor in
// internal/server/authz, before any method body runs. What remains in the
// handlers is what an interceptor structurally cannot do — bind req.Caller to
// the connection's identity, and restrict how a request arrived. Those are the
// properties tested here.

// identityService wires only the fields the two guards read.
func identityService(cn string) *Service {
	return &Service{callerFromContext: func(context.Context) string { return cn }}
}

// proxiedService reports every request as arriving over a trusted front proxy.
func proxiedService(cn string, mayApply, mayOperate bool) *Service {
	return &Service{
		callerFromContext:      func(context.Context) string { return cn },
		proxyForwarded:         func(context.Context) bool { return true },
		trustedProxyMayApply:   mayApply,
		trustedProxyMayOperate: mayOperate,
	}
}

func TestBindCallerIdentityAcceptsOwnNamespace(t *testing.T) {
	s := identityService("backend")
	if err := s.bindCallerIdentity(context.Background(), "backend"); err != nil {
		t.Fatalf("a caller writing to its own namespace should be permitted: %v", err)
	}
}

// The escalation this guard exists to stop. CAPABILITY_SCHEMA_APPLY says the
// caller may apply; without this check it would say the caller may apply to
// *anyone's* schema, because the interceptor that granted it never saw the
// request body naming the target.
func TestBindCallerIdentityRejectsCrossCaller(t *testing.T) {
	s := identityService("backend")
	err := s.bindCallerIdentity(context.Background(), "vendor")
	if err == nil {
		t.Fatal("a caller must not be able to apply to another caller's namespace")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("expected a same-CN mismatch error, got %v", err)
	}
}

// In insecure dev mode there is no cert identity to bind against, so the check
// has nothing to compare and is skipped. The capability requirement is what
// governs the RPC in that configuration — or rather, would, except that
// cmd/server does not install the interceptor without TLS either, for the same
// reason: a self-asserted header is not an identity.
func TestBindCallerIdentitySkipsWithoutAnIdentity(t *testing.T) {
	for _, cn := range []string{"", "anonymous"} {
		if err := identityService(cn).bindCallerIdentity(context.Background(), "anything"); err != nil {
			t.Errorf("cn=%q: no identity to bind against, want skip, got %v", cn, err)
		}
	}
}

func TestBindCallerIdentityHonoursNilExtractor(t *testing.T) {
	s := &Service{}
	if err := s.bindCallerIdentity(context.Background(), "anything"); err != nil {
		t.Fatalf("a Service with no extractor has no identity to bind: %v", err)
	}
}

// Transport restrictions are orthogonal to capability: an edge-terminated
// connection is held to a different standard than direct mTLS no matter what
// the caller was granted.
func TestGuardOperatorTransportRejectsProxyByDefault(t *testing.T) {
	err := proxiedService("atlantis-console", true, false).guardOperatorTransport(context.Background())
	if err == nil {
		t.Fatal("operator RPCs must not arrive over a trusted proxy unless opted in")
	}
	if !strings.Contains(err.Error(), "ATL_TRUSTED_PROXY_MAY_OPERATE") {
		t.Errorf("error should name the opt-in, got %v", err)
	}
}

func TestGuardOperatorTransportAcceptsProxyWhenOptedIn(t *testing.T) {
	if err := proxiedService("atlantis-console", false, true).guardOperatorTransport(context.Background()); err != nil {
		t.Fatalf("opted-in proxy operator RPC should be permitted: %v", err)
	}
}

func TestGuardOperatorTransportAcceptsDirectMTLS(t *testing.T) {
	if err := identityService("atlantis-console").guardOperatorTransport(context.Background()); err != nil {
		t.Fatalf("direct mTLS is the unrestricted path: %v", err)
	}
}

// The two flags are independent: a deployment that lets the edge carry a
// caller's own applies must not thereby let it carry operator RPCs.
func TestProxyApplyAndOperateAreSeparateGrants(t *testing.T) {
	s := proxiedService("backend", true, false)
	if err := s.bindCallerIdentity(context.Background(), "backend"); err != nil {
		t.Fatalf("MAY_APPLY should permit a proxied self-apply: %v", err)
	}
	if err := s.guardOperatorTransport(context.Background()); err == nil {
		t.Fatal("MAY_APPLY must not confer MAY_OPERATE")
	}
}

func TestBindCallerIdentityRejectsProxiedApplyWhenNotOptedIn(t *testing.T) {
	err := proxiedService("backend", false, false).bindCallerIdentity(context.Background(), "backend")
	if err == nil {
		t.Fatal("a proxied apply must be refused when MAY_APPLY is off")
	}
	if !strings.Contains(err.Error(), "ATL_TRUSTED_PROXY_MAY_APPLY") {
		t.Errorf("error should name the opt-in, got %v", err)
	}
}

func TestParseSubmitted_ProducesFiles(t *testing.T) {
	files := []SubmittedFile{
		{Path: "a.atl", Content: []byte(`entity A in x { id bigint primary }`)},
		{Path: "b.atl", Content: []byte(`entity B in x { id bigint primary }`)},
	}
	parsed, errs := parseSubmitted("caller-1", files)
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(parsed) != 2 {
		t.Errorf("expected 2 parsed files, got %d", len(parsed))
	}
	// Path is prefixed with caller so cross-caller error attribution works.
	if !strings.HasPrefix(parsed[0].Path, "caller-1:") {
		t.Errorf("expected caller prefix on path, got %q", parsed[0].Path)
	}
}

func TestParseSubmitted_SurfacesErrors(t *testing.T) {
	files := []SubmittedFile{
		{Path: "good.atl", Content: []byte(`entity A in x { id bigint primary }`)},
		{Path: "bad.atl", Content: []byte(`entity { definitely not a parseable .atl file }`)},
	}
	_, errs := parseSubmitted("caller-1", files)
	if len(errs) == 0 {
		t.Fatalf("expected at least one parse error")
	}
	if !strings.Contains(errs[0], "bad.atl") {
		t.Errorf("error should name the offending file, got %q", errs[0])
	}
}

func TestComputePlanID_StableForSameInput(t *testing.T) {
	files := []*dsl.File{
		{Path: "caller-1:a.atl"},
		{Path: "caller-1:b.atl"},
	}
	id1 := computePlanID("caller-1", files, "")
	id2 := computePlanID("caller-1", files, "")
	if id1 != id2 {
		t.Errorf("PlanID should be deterministic: %q != %q", id1, id2)
	}
}

func TestComputePlanID_ChangesWithTheDependencyHash(t *testing.T) {
	files := []*dsl.File{{Path: "caller-1:a.atl"}}
	idA := computePlanID("caller-1", files, "")
	idB := computePlanID("caller-1", files, "9f86d081884c7d65")
	if idA == idB {
		t.Errorf("PlanID should change when the dependency hash changes; both = %q", idA)
	}
}

func TestComputePlanID_StableUnderFileReorder(t *testing.T) {
	f1 := []*dsl.File{{Path: "caller-1:a.atl"}, {Path: "caller-1:b.atl"}}
	f2 := []*dsl.File{{Path: "caller-1:b.atl"}, {Path: "caller-1:a.atl"}}
	if computePlanID("caller-1", f1, "") != computePlanID("caller-1", f2, "") {
		t.Errorf("PlanID should be invariant under file order; reordering changed it")
	}
}

func TestComputePlanID_DiffersAcrossCallers(t *testing.T) {
	files := []*dsl.File{{Path: "caller:a.atl"}}
	idA := computePlanID("caller-A", files, "")
	idB := computePlanID("caller-B", files, "")
	if idA == idB {
		t.Errorf("different callers should produce different PlanIDs; got %q for both", idA)
	}
}

// Merged-schema version is the cache-key that lets `tide pull` short-circuit
// when nothing has changed. Three invariants pin its behavior so a quiet
// regression — say, switching the byte separator — can't slip past CI.

func TestComputeMergedSchemaVersion_StableForIdenticalInput(t *testing.T) {
	entries := []mergedEntry{
		{caller: "api", path: "auth/schema.atl", content: "entity A in x {}"},
		{caller: "data-pipeline", path: "catalog/schema.atl", content: "entity B in x {}"},
	}
	v1 := computeMergedSchemaVersion(entries)
	v2 := computeMergedSchemaVersion(entries)
	if v1 != v2 {
		t.Errorf("version is not deterministic across calls: %s vs %s", v1, v2)
	}
}

func TestComputeMergedSchemaVersion_ShiftsOnContentChange(t *testing.T) {
	base := []mergedEntry{{caller: "x", path: "p.atl", content: "alpha"}}
	bumped := []mergedEntry{{caller: "x", path: "p.atl", content: "beta"}}
	if computeMergedSchemaVersion(base) == computeMergedSchemaVersion(bumped) {
		t.Errorf("content change must shift the version (otherwise tide pull would never refresh)")
	}
}

// Length-prefix-free encodings can collide when values contain the
// separator. We use NUL bytes to avoid that — pin it.
func TestComputeMergedSchemaVersion_FieldBoundariesDoNotCollide(t *testing.T) {
	a := []mergedEntry{{caller: "ab", path: "cd", content: "ef"}}
	// Splice in a way that would collide if we joined fields without a
	// terminator byte: (caller="a", path="bcd", content="ef").
	b := []mergedEntry{{caller: "a", path: "bcd", content: "ef"}}
	if computeMergedSchemaVersion(a) == computeMergedSchemaVersion(b) {
		t.Errorf("field boundaries must be preserved in the hash; got collision")
	}
}

func TestTranslateClass(t *testing.T) {
	cases := map[codegen.ChangeClass]ClassName{
		codegen.ClassAdditive:            ClassAdditive,
		codegen.ClassBackfillRequired:    ClassBackfill,
		codegen.ClassCrossCallerBreaking: ClassBreaking,
	}
	for in, want := range cases {
		if got := translateClass(in); got != want {
			t.Errorf("translateClass(%v) = %s want %s", in, got, want)
		}
	}
}

func TestImpactReport_IncludesPlanCaller(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindEntityAdded, EntityID: "x.A", Detail: "added"},
		},
	}
	rep := buildImpactReport("caller-1", nil, d, nil)
	if len(rep) != 1 {
		t.Fatalf("want 1 entry, got %d", len(rep))
	}
	if rep[0].Caller != "caller-1" {
		t.Errorf("expected planning caller in report, got %q", rep[0].Caller)
	}
	if !rep[0].Affected {
		t.Errorf("planning caller should always be marked affected")
	}
}

func TestImpactReport_SortedByCaller(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindEntityAdded, EntityID: "x.A", Detail: "added"},
		},
	}
	others := []*dsl.File{
		{Path: "zeta-caller:a.atl"},
		{Path: "alpha-caller:a.atl"},
		{Path: "mu-caller:a.atl"},
	}
	rep := buildImpactReport("planning-caller", others, d, nil)
	for i := 1; i < len(rep); i++ {
		if rep[i-1].Caller > rep[i].Caller {
			t.Errorf("report not sorted: %s > %s", rep[i-1].Caller, rep[i].Caller)
		}
	}
}

func TestIndexOf(t *testing.T) {
	cases := []struct {
		s    string
		c    byte
		want int
	}{
		{"hello", 'l', 2},
		{"hello", 'z', -1},
		{"", 'a', -1},
		{"a:b:c", ':', 1},
	}
	for _, c := range cases {
		if got := indexOf(c.s, c.c); got != c.want {
			t.Errorf("indexOf(%q, %c) = %d want %d", c.s, c.c, got, c.want)
		}
	}
}

// ---- buildEntityOwnership tests ----

func TestBuildEntityOwnership_AssignsCallerCorrectly(t *testing.T) {
	callerFiles := parseSrc(t, "vendor", `entity Product in vendor { id bigint primary }`)
	otherFiles := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	own := buildEntityOwnership("vendor", callerFiles, otherFiles)

	if own["vendor.Product"] != "vendor" {
		t.Errorf("vendor.Product should be owned by vendor, got %q", own["vendor.Product"])
	}
	if own["consumer.Account"] != "consumer" {
		t.Errorf("consumer.Account should be owned by consumer, got %q", own["consumer.Account"])
	}
}

// ---- buildCrossCallerRefs tests ----

func TestBuildCrossCallerRefs_DetectsFK(t *testing.T) {
	otherFiles := parseSrc(t, "consumer", `
entity Account in consumer {
  id bigint primary
  product_id bigint references vendor.Product.id
}
`)
	refs := buildCrossCallerRefs(otherFiles)

	if !refs["vendor.Product"] {
		t.Error("expected vendor.Product in cross-caller refs (entity-level)")
	}
	if !refs["vendor.Product.id"] {
		t.Error("expected vendor.Product.id in cross-caller refs (field-level)")
	}
}

func TestBuildCrossCallerRefs_EmptyWhenNoRefs(t *testing.T) {
	otherFiles := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	refs := buildCrossCallerRefs(otherFiles)
	if len(refs) != 0 {
		t.Errorf("expected empty refs, got %v", refs)
	}
}

// ---- impact report fix test ----

func TestImpactReport_OnlyAffectedCallersMarked(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindFieldAdded, EntityID: "vendor.Product", Detail: "added"},
		},
	}
	// consumer declares consumer.Account, not vendor.Product
	others := parseSrc(t, "consumer", `entity Account in consumer { id bigint primary }`)
	rep := buildImpactReport("vendor", others, d, nil)

	for _, entry := range rep {
		if entry.Caller == "consumer" && entry.Affected {
			t.Error("consumer should NOT be marked affected — the diff only touches vendor.Product")
		}
	}
}

func TestImpactReport_AffectedCallerMarked(t *testing.T) {
	d := &codegen.Diff{
		Additive: []codegen.Change{
			{Kind: codegen.KindFieldAdded, EntityID: "vendor.Product", Detail: "added"},
		},
	}
	// other caller also declares vendor.Product
	others := parseSrc(t, "other", `entity Product in vendor { id bigint primary  name text }`)
	rep := buildImpactReport("submitter", others, d, nil)

	found := false
	for _, entry := range rep {
		if entry.Caller == "other" {
			found = true
			if !entry.Affected {
				t.Error("other caller who declares vendor.Product should be marked affected")
			}
		}
	}
	if !found {
		t.Error("other caller should appear in the impact report")
	}
}

// parseSrc is a test helper that parses a single .atl source string as if
// submitted by the given caller, returning the parsed file slice.
func parseSrc(t *testing.T, caller, src string) []*dsl.File {
	t.Helper()
	f, err := dsl.Parse(caller+":test.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", caller, err)
	}
	return []*dsl.File{f}
}

// Full Plan / Apply paths require Postgres and are covered in the
// integration harness (task #25). The pure-Go pieces above pin the
// classification, ID stability, and impact-report shape — the parts that
// would silently break if a refactor regresses them.

func TestValidateCustomSQL_EmptyIR(t *testing.T) {
	if msgs := validateCustomSQL(&dsl.IR{}, "consumer", nil); len(msgs) != 0 {
		t.Errorf("empty IR should produce no errors, got %v", msgs)
	}
}

func TestValidateCustomSQL_HappyPath(t *testing.T) {
	src := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
}

query OutfitsForConsumer for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id, consumer_id FROM consumer_account WHERE id = $id
  }
}
`
	// Parse with caller-prefixed path so SourcePath matches the
	// "<caller>:<file>" shape that parseSubmitted produces in real flow.
	f, err := dsl.Parse("consumer:t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if msgs := validateCustomSQL(ir, "consumer", nil); len(msgs) != 0 {
		t.Errorf("expected no errors, got: %v", msgs)
	}
}

func TestValidateCustomSQL_SurfacesPGErrors(t *testing.T) {
	// A query that lowers cleanly (every $arg is declared, touches()
	// resolves) but whose SQL references a nonexistent table can only
	// be caught by the pg_query_go pass. This test pins that wiring.
	src := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
}

query BadTable for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id FROM consumer_widget WHERE id = $id
  }
}
`
	f, err := dsl.Parse("consumer:t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	msgs := validateCustomSQL(ir, "consumer", nil)
	if len(msgs) == 0 {
		t.Fatal("expected at least one error for unknown table")
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "consumer_widget") {
		t.Errorf("error should mention the bad table; got: %s", joined)
	}
}

func TestValidateCustomSQL_IgnoresOtherCallers(t *testing.T) {
	// Two callers in the merged IR. The "other" caller's query references
	// a nonexistent table — validation should ignore it because it isn't
	// the submitting caller's content. The submitting caller's clean query
	// passes.
	otherSrc := `
entity Account in consumer {
  id          bigint primary
  consumer_id text not null
}

query StaleRef for Account {
  input { id: bigint }
  output as Account
  sql touches(Account) {
    SELECT id FROM consumer_widget WHERE id = $id
  }
}
`
	mineSrc := `
entity Cart in shop {
  id      bigint primary
  user_id bigint not null
}

query Mine for Cart {
  input { id: bigint }
  output as Cart
  sql touches(Cart) {
    SELECT id, user_id FROM shop_cart WHERE id = $id
  }
}
`
	other, err := dsl.Parse("consumer:other.atl", []byte(otherSrc))
	if err != nil {
		t.Fatalf("parse other: %v", err)
	}
	mine, err := dsl.Parse("shop:mine.atl", []byte(mineSrc))
	if err != nil {
		t.Fatalf("parse mine: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{other, mine})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if msgs := validateCustomSQL(ir, "shop", nil); len(msgs) != 0 {
		t.Errorf("submitting caller's content is clean; should be 0 errors, got: %v", msgs)
	}
	if msgs := validateCustomSQL(ir, "consumer", nil); len(msgs) == 0 {
		t.Fatal("when consumer submits, its own stale ref should be caught")
	}
}

// TestMutatingPlaneSwitchRefusesApply covers the deployment-wide switch that
// capability enforcement is easy to mistake for a replacement of.
//
// ATL_ALLOW_APPLY_MUTATION=false is the regulated posture: SQL is reviewed on a
// deployment-repo PR and applied by tidectl, and no grant may route around it.
// It regressed once already — the check used to live inside the same helper as
// the per-CN allowlist, and removing the allowlist took the switch with it,
// which nothing caught because nothing asserted on it. The nil pool is
// deliberate: reaching Postgres would mean the guard let the request through.
func TestMutatingPlaneSwitchRefusesApply(t *testing.T) {
	s := New(nil, Config{
		AllowApplyMutation: false,
		BackfillEnabled:    true,
		CallerFromContext:  func(context.Context) string { return "backend" },
	})
	ctx := context.Background()

	_, err := s.ApplyMigration(ctx, &adminpb.ApplyMigrationRequest{Caller: "backend", PlanId: "p1"})
	if err == nil {
		t.Fatal("ApplyMigration proceeded with the mutating plane switched off")
	}
	if !strings.Contains(err.Error(), "ATL_ALLOW_APPLY_MUTATION") {
		t.Errorf("error should name the switch so an operator can act on it, got %v", err)
	}

	_, err = s.BeginBackfillPlan(ctx, &adminpb.BeginBackfillPlanRequest{Caller: "backend", PlanId: "p1"})
	if err == nil {
		t.Fatal("BeginBackfillPlan proceeded with the mutating plane switched off")
	}
	if !strings.Contains(err.Error(), "ATL_ALLOW_APPLY_MUTATION") {
		t.Errorf("error should name the switch, got %v", err)
	}
}

// And the switch is one-directional: it can close the plane, never narrow who
// may use an open one. Asserting this separately keeps it from drifting into a
// second authorization layer, where "who may apply" would have two answers that
// could disagree.
func TestMutatingPlaneSwitchIsNotAuthorization(t *testing.T) {
	open := New(nil, Config{AllowApplyMutation: true})
	if err := open.requireMutablePlane("schema apply"); err != nil {
		t.Errorf("an open plane refused: %v", err)
	}
	// It reads no identity, so it cannot express a per-caller decision even by
	// accident — there is nothing for one to be derived from.
	if open.callerFromContext != nil {
		t.Fatal("test setup: this Service should have no identity extractor")
	}

	closed := New(nil, Config{AllowApplyMutation: false})
	if err := closed.requireMutablePlane("schema apply"); err == nil {
		t.Error("a closed plane permitted a mutation")
	}
}
