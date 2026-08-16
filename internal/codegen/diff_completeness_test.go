package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Assertions over the coverage registry in diff_coverage.go: it must be
// exhaustive over the IR, and every claim in it must be checkable.
//
// # Why a registry rather than a source scan
//
// The first version of this file AST-scanned the differ for field selectors.
// It reported dsl.Entity.Kind as covered — because dsl.Index.Kind is read at
// diff.go:1012 and a name-based scan cannot tell them apart. A completeness
// guard fooled by a name collision is worse than none, because it reports
// safety it has not checked.
//
// The registry names the differ, and the check below scans *that function's
// body* for the field. Scoping the scan to one named function is what defeats
// the collision the whole-file scan could not: diffIndexes reading .Kind is
// scoped to diffIndexes, which is what covers Entity.Indexes.

func TestEveryEntityAttributeHasACoverageDecision(t *testing.T) {
	assertDecided(t, reflect.TypeOf(dsl.Entity{}), entityCoverage, "dsl.Entity", "entityCoverage")
}

func TestEveryFieldAttributeHasACoverageDecision(t *testing.T) {
	assertDecided(t, reflect.TypeOf(dsl.Field{}), fieldCoverage, "dsl.Field", "fieldCoverage")
}

func assertDecided(t *testing.T, typ reflect.Type, cov map[string]coverage, label, mapName string) {
	t.Helper()
	declared := differFuncsInPackage(t)
	claimed := claimedDiffers()

	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() || f.Anonymous {
			continue
		}
		seen[f.Name] = true
		c, ok := cov[f.Name]
		if !ok {
			t.Errorf("%s.%s has no entry in %s.\n"+
				"  A change to it is currently invisible to the diff, so it emits no DDL "+
				"and rehearsal would report pass for something it never examined.\n"+
				"  Add an entry: {diffed, \"<differ>\", \"\"}, {notSchema, \"\", \"<why>\"}, "+
				"or {gap, \"\", \"<why, and the task that closes it>\"}.",
				label, f.Name, mapName)
			continue
		}
		switch c.kind {
		case diffed:
			switch {
			case c.by == "":
				t.Errorf("%s.%s claims to be diffed but names no differ", label, f.Name)
			case declared[c.by] == nil:
				t.Errorf("%s.%s claims coverage by %s, which this package does not declare. "+
					"Either the differ was renamed or removed, or the claim was never true.",
					label, f.Name, c.by)
			case !readsField(declared, c.by, f.Name, claimed):
				t.Errorf("%s.%s claims coverage by %s, but %s never reads .%s.\n"+
					"  Naming a function that exists is not coverage — the first version of "+
					"this guard checked only existence, so {\"CompositePK\": {diffed, "+
					"\"indexByID\"}} would have passed.",
					label, f.Name, c.by, c.by, f.Name)
			case !producesChange(declared, c.by, claimed):
				t.Errorf("%s.%s claims coverage by %s, but %s never produces a Change.\n"+
					"  Reading a field is not diffing it. {\"Fields\": {diffed, "+
					"\"fieldsByName\"}} named a pure map builder that reads .Fields and "+
					"emits nothing, and satisfied every check here — coverage laundered "+
					"through a helper.",
					label, f.Name, c.by, c.by)
			}
			if c.why != "" {
				t.Errorf("%s.%s is diffed but also carries a reason %q — a stale why "+
					"outliving a reclassification is how these maps start lying",
					label, f.Name, c.why)
			}
		case notSchema, gap:
			if strings.TrimSpace(c.why) == "" {
				t.Errorf("%s.%s is exempted or marked a gap with no reason", label, f.Name)
			}
			if c.by != "" {
				t.Errorf("%s.%s is not diffed but names differ %q", label, f.Name, c.by)
			}
		}
	}

	// A stale entry means the IR dropped a field and the map still claims to
	// cover it — harmless at runtime, but it makes the map lie about what it
	// has audited.
	for name := range cov {
		if !seen[name] {
			t.Errorf("%s names %s.%s, which no longer exists on the struct", mapName, label, name)
		}
	}
}

// readsField reports whether fn, or any package-local helper reachable from it,
// mentions .name in selector position.
//
// Rooted at the named differ rather than scanning the file, which is what
// defeats the collision a whole-file scan could not: diffIndexes reading .Kind
// is not reachable from diffChecks, so claiming diffChecks covers Entity.Kind
// still fails.
//
// The transitive step is necessary, not convenience — diffFields reads .Fields
// through fieldsByName and diffChecks reads .Checks through resolvedChecks.
// Requiring the access to sit in the differ's literal body would force those
// helpers to be inlined to satisfy a test, which is the test dictating the
// code's shape rather than checking it.
//
// But the walk stops at any function that is itself a claimed differ, and that
// is what keeps the rooting meaningful. Without it, diffEntity — which calls
// every sub-differ — reads every attribute any of them reads, so
// {diffed, "diffEntity"} satisfied Kind, Checks, Indexes, Uniques, Cache,
// QueryTimeoutMS, TableName and all seven diffed Field attributes. One
// plausible name would have blanket-covered the registry, which is the
// wildcard the scoping exists to prevent. A differ delegating to another differ
// must claim the delegate by name.
func readsField(all map[string]*ast.FuncDecl, root, name string, claimed map[string]bool) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(fnName string) bool {
		if seen[fnName] {
			return false
		}
		seen[fnName] = true
		fn := all[fnName]
		if fn == nil {
			return false
		}
		found := false
		var callees []string
		ast.Inspect(fn, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				if e.Sel.Name == name {
					found = true
				}
			case *ast.CallExpr:
				// Package-local calls only. A method call (x.f()) is a
				// SelectorExpr callee and is deliberately not followed —
				// following methods would widen the closure to most of the
				// package and give back the collision this scoping prevents.
				if id, ok := e.Fun.(*ast.Ident); ok {
					callees = append(callees, id.Name)
				}
			}
			return true
		})
		if found {
			return true
		}
		for _, c := range callees {
			if c != root && claimed[c] {
				continue // another differ's coverage is its own to claim
			}
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// claimedDiffers is the set of function names any coverage entry names as its
// differ. readsField refuses to walk into one from another, so delegation does
// not launder coverage.
func claimedDiffers() map[string]bool {
	out := map[string]bool{}
	for _, m := range []map[string]coverage{entityCoverage, fieldCoverage} {
		for _, c := range m {
			if c.kind == diffed && c.by != "" {
				out[c.by] = true
			}
		}
	}
	return out
}

// producesChange reports whether fn, or a package-local helper reachable from
// it, actually files a Change into a Diff — d.append(...), or appending to one
// of the Diff's three slices.
//
// This is the other half of coverage. readsField establishes that the named
// function looks at the attribute; without this, a claim could name any helper
// that merely touches the field. `fieldsByName` reads .Fields and builds a map;
// claiming it covered Entity.Fields passed the whole suite while meaning
// nothing.
//
// Same claimed-differ stop as readsField, for the same reason: a function that
// only delegates to another differ has not covered anything itself.
func producesChange(all map[string]*ast.FuncDecl, root string, claimed map[string]bool) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		fn := all[name]
		if fn == nil {
			return false
		}
		found := false
		var callees []string
		ast.Inspect(fn, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				// d.append(...) — the method, or any of the Diff's four slices
				// on the left of an append.
				//
				// Destructive was missing from this list, so a differ that
				// files ONLY destructive changes was invisible here and its
				// attribute reported as uncovered. diffPartition is the first
				// such differ: both adding and removing tenant isolation are
				// decisions an operator must make explicitly, so neither is
				// additive, and the checker could not see either.
				switch e.Sel.Name {
				case "append", "Additive", "BackfillRequired", "Breaking", "Destructive":
					found = true
				}
			case *ast.CallExpr:
				if id, ok := e.Fun.(*ast.Ident); ok {
					callees = append(callees, id.Name)
				}
			}
			return true
		})
		if found {
			return true
		}
		for _, c := range callees {
			if c != root && claimed[c] {
				continue
			}
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// differFuncsInPackage returns the top-level function declarations in this
// package's diff sources, so a coverage claim naming a differ can be checked
// against a differ that both exists and reads the field.
func differFuncsInPackage(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.FuncDecl{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, "diff") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		scanned++
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				out[fn.Name.Name] = fn
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no differ sources; this test is not looking where it thinks")
	}
	return out
}

// The gap list is the input to rehearsal's unverified rule, so an empty one
// would silently mean "everything is covered" and let rehearsal claim a
// completeness it does not have.
func TestUncoveredAttributesAreReported(t *testing.T) {
	gaps := UncoveredIRAttributes()
	if len(gaps) == 0 {
		t.Fatal("the gap list is empty. Either every IR attribute is genuinely " +
			"covered — in which case delete this backstop and say so — or the " +
			"derivation broke and rehearsal is about to claim a completeness it " +
			"does not have. An empty list must never pass silently.")
	}

	// Derived from the maps, so it must agree with them exactly. Asserting only
	// that the list is non-empty left every interesting mutation alive: reading
	// the wrong kind, prefixing every entry "Field.", or dropping the
	// fieldCoverage loop entirely all kept it non-empty.
	want := map[string]bool{}
	for name, c := range entityCoverage {
		if c.kind == gap {
			want["Entity."+name] = true
		}
	}
	for name, c := range fieldCoverage {
		if c.kind == gap {
			want["Field."+name] = true
		}
	}
	got := map[string]bool{}
	for _, g := range gaps {
		got[g] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%s is a declared gap but is missing from UncoveredIRAttributes; "+
				"rehearsal would treat a change to it as verified", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("UncoveredIRAttributes reports %s, which no map marks as a gap", k)
		}
	}

	// Both structs must contribute, or one half of the IR is silently exempt.
	var entities, fields int
	for _, g := range gaps {
		switch {
		case strings.HasPrefix(g, "Entity."):
			entities++
		case strings.HasPrefix(g, "Field."):
			fields++
		default:
			t.Errorf("gap entry %q carries neither prefix, so a consumer cannot "+
				"tell which struct it belongs to", g)
		}
	}
	if entities == 0 || fields == 0 {
		t.Errorf("gaps came from only one struct (%d entity, %d field) — the "+
			"derivation is not reading both maps", entities, fields)
	}
	t.Logf("%d IR attributes are invisible to the differ and must force "+
		"verdict unverified: %v", len(gaps), gaps)
}

// Diff.All must cover every bucket on the struct.
//
// Adding Destructive found six production sites that listed the three existing
// buckets literally — change counts, lineage, drift reporting, plan detail —
// and every one would have dropped destructive changes without a word. A plan
// would have reported fewer changes than it contained, and the omitted kind
// would have been the one that destroys data.
//
// Derived by reflection rather than by listing the buckets again, because
// listing them again is the bug.
func TestDiffAllCoversEveryBucket(t *testing.T) {
	typ := reflect.TypeOf(Diff{})

	// One change in every bucket, so a bucket All() forgets is one change
	// missing from the total.
	d := &Diff{}
	want := 0
	v := reflect.ValueOf(d).Elem()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type != reflect.TypeOf([]Change{}) {
			continue
		}
		v.Field(i).Set(reflect.ValueOf([]Change{{Kind: KindEntityAdded, EntityID: f.Name}}))
		want++
	}
	if want < 4 {
		t.Fatalf("found %d change buckets on Diff; this test is not reading what it "+
			"thinks it is", want)
	}

	got := d.All()
	if len(got) != want {
		seen := map[string]bool{}
		for _, c := range got {
			seen[c.EntityID] = true
		}
		var missing []string
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Type == reflect.TypeOf([]Change{}) && !seen[f.Name] {
				missing = append(missing, f.Name)
			}
		}
		t.Errorf("All() returned %d of %d changes; missing bucket(s): %v.\n"+
			"  Every caller that counts, lists or reports changes goes through "+
			"All(), so a bucket omitted here is invisible everywhere at once.",
			len(got), want, missing)
	}
	if d.Len() != want {
		t.Errorf("Len() = %d, want %d", d.Len(), want)
	}
	if d.IsEmpty() {
		t.Error("IsEmpty() is true for a diff with a change in every bucket")
	}
}

// TestDiffClassesPresentCoversEveryBucket is the same guard for the method the
// change policy iterates.
//
// The policy is four independent rules, and the gate used to consult only
// HighestClass — so a diff that both broke a caller and dropped a column asked
// the breaking rule alone. With breaking set to auto-apply, the DROP ran
// unattended and no plan was filed, though the operator had explicitly
// required approval for destructive changes.
//
// ClassesPresent replaced that, which makes it the thing a new bucket must not
// escape. A bucket it forgets is a class the policy silently stops governing,
// and the symptom is an apply that does not stop — the failure nobody notices
// until it has already run.
func TestDiffClassesPresentCoversEveryBucket(t *testing.T) {
	typ := reflect.TypeOf(Diff{})

	d := &Diff{}
	buckets := 0
	v := reflect.ValueOf(d).Elem()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type != reflect.TypeOf([]Change{}) {
			continue
		}
		v.Field(i).Set(reflect.ValueOf([]Change{{Kind: KindEntityAdded, EntityID: f.Name}}))
		buckets++
	}
	if buckets < 4 {
		t.Fatalf("found %d change buckets on Diff; this test is not reading what "+
			"it thinks it is", buckets)
	}

	got := d.ClassesPresent()
	if len(got) != buckets {
		t.Errorf("ClassesPresent() returned %d classes for a diff with %d "+
			"populated buckets (%v) — a bucket missing here is a class the "+
			"change policy no longer evaluates, so an apply that should have "+
			"stopped proceeds unattended", len(got), buckets, got)
	}

	// Ordered most restrictive first, because the gate records the plan under
	// the FIRST class that requires approval and names it in the refusal.
	// Wrong order files the plan under the wrong rule.
	want := []ChangeClass{
		ClassCrossCallerBreaking,
		ClassDestructive,
		ClassBackfillRequired,
		ClassAdditive,
	}
	for i := range want {
		if i >= len(got) {
			break
		}
		if got[i] != want[i] {
			t.Errorf("ClassesPresent()[%d] = %v, want %v — the order decides "+
				"which rule the plan is recorded under", i, got[i], want[i])
		}
	}

	// An empty diff gates nothing.
	if c := (&Diff{}).ClassesPresent(); len(c) != 0 {
		t.Errorf("an empty diff reports classes %v", c)
	}
}
