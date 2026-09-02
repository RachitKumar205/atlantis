package main

import (
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// generatedFileName turns a name the SERVER chose into a path this process
// writes to. Everything it guards against arrives over the wire.

func TestGeneratedFileNameRefusesPathTraversal(t *testing.T) {
	// A Postgres identifier is only constrained by what fits inside quotes, so
	// every one of these is a table a customer could really have, and
	// SuggestedName carries the characters through. filepath.Join CLEANS its
	// result, so ".." is not neutralised by joining — it resolves.
	for _, name := range []string{
		"../../etc/passwd",
		"..",
		"a/b",
		`a\b`,
		"/absolute",
		"with space",
		"semi;colon",
		"dot.dot",
		"",
		// A leading digit, which a check asking only whether every rune is
		// alphanumeric or underscore accepts. `2024_events` is an ordinary
		// legacy table name; it yields "2024Events", which lexes as a number
		// followed by an identifier, so the .atl written for it does not parse.
		"2024Events",
		"1",
	} {
		if got, err := generatedFileName(name); err == nil {
			t.Errorf("entity name %q was accepted as filename %q — the server "+
				"chooses this name from a table name, so accepting it lets a "+
				"table name decide where this process writes", name, got)
		}
	}
}

func TestGeneratedFileNameAcceptsOrdinaryNames(t *testing.T) {
	for name, want := range map[string]string{
		"Customer":   "customer.atl",
		"OrderLine":  "orderline.atl",
		"order_line": "order_line.atl",
		"V2":         "v2.atl",
		"_private":   "_private.atl",
		// Non-ASCII letters. The DSL's isIdentStart is unicode.IsLetter, so
		// these are valid entity names, and an ASCII-only check refuses them.
		"CaféOrders": "caféorders.atl",
		"Ünicode":    "ünicode.atl",
	} {
		got, err := generatedFileName(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}

// TestRefuseMissingSchema drives the decision directly.
//
// An AST tripwire over cmdInspect — collecting *ast.Ident and *ast.BasicLit and
// asserting "generating" and "ErrNotExist" appear — passes against a body with
// both negations inverted. A negation is a UnaryExpr, which such a walk never
// visits, so `!generating` reads the same as `generating`.
//
// The rows below are the whole input space of refuseMissingSchema.
func TestRefuseMissingSchema(t *testing.T) {
	other := errors.New("permission denied")

	for _, tc := range []struct {
		name       string
		generating bool
		walkErr    error
		files      int
		wantRefuse bool
		wantErr    error
	}{
		// The case the whole fix exists for: a fresh repo, no schema directory.
		{"generate, no schema dir", true, iofs.ErrNotExist, 0, false, nil},
		// A schema directory that exists and is empty.
		{"generate, empty schema dir", true, nil, 0, false, nil},
		// Generation in a repo that already has declarations is ordinary.
		{"generate, files present", true, nil, 3, false, nil},

		// Plain inspect has nothing to compare against and must still refuse,
		// which is the half a looser fix would have broken.
		{"inspect, no schema dir", false, iofs.ErrNotExist, 0, true, iofs.ErrNotExist},
		{"inspect, empty schema dir", false, nil, 0, true, nil},
		{"inspect, files present", false, nil, 3, false, nil},

		// A permission error is not a fresh repo, on either path.
		{"generate, unreadable dir", true, other, 0, true, other},
		{"inspect, unreadable dir", false, other, 0, true, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuse, why := refuseMissingSchema(tc.generating, tc.walkErr, tc.files)
			if refuse != tc.wantRefuse {
				t.Errorf("refuse = %v, want %v", refuse, tc.wantRefuse)
			}
			if tc.wantErr == nil && why != nil {
				t.Errorf("returned error %v, want none", why)
			}
			if tc.wantErr != nil && !errors.Is(why, tc.wantErr) {
				t.Errorf("returned error %v, want %v", why, tc.wantErr)
			}
		})
	}
}

// TestCmdInspectUsesTheSharedDecision keeps the decision from being reinlined.
// refuseMissingSchema is only worth having if cmdInspect actually calls it;
// a copy of the logic back inside cmdInspect would leave the table above
// passing while the command did something else.
func TestCmdInspectUsesTheSharedDecision(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "inspect.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse inspect.go: %v", err)
	}
	var calls int
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdInspect" {
			return true
		}
		ast.Inspect(fn, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuseMissingSchema" {
				calls++
			}
			return true
		})
		return false
	})
	if calls != 1 {
		t.Errorf("cmdInspect calls refuseMissingSchema %d times, want exactly 1 — "+
			"the tested decision is not the one the command makes", calls)
	}
}

// output_dir decides what sweepOwned deletes, and it arrives from YAML.
//
// filepath.Join resolves "..", which is why generatedFileName is guarded the
// same way above. "." would clear ./client and ./pb at the repository root.
func TestValidateOutputDirRefusesEscapes(t *testing.T) {
	for _, tc := range []struct {
		in     string
		reason string
	}{
		{"", "empty names nothing"},
		{"   ", "whitespace is empty"},
		{".", "the repository root, whose client/ and pb/ are not ours"},
		{"./", "the root by another spelling"},
		{"/abs/path", "absolute leaves the repository"},
		{"../sibling", "escapes upward"},
		{"a/../..", "escapes after cleaning"},
		{"a/../../b", "escapes and comes back down elsewhere"},
	} {
		if _, err := validateOutputDir(tc.in); err == nil {
			t.Errorf("validateOutputDir(%q) was accepted; %s", tc.in, tc.reason)
		}
	}

	for _, tc := range []struct{ in, want string }{
		{"internal/gen/pcclient", "internal/gen/pcclient"},
		{"./internal/gen", "internal/gen"},
		{"gen/", "gen"},
		{"a/b/../c", "a/c"},
	} {
		got, err := validateOutputDir(tc.in)
		if err != nil {
			t.Errorf("validateOutputDir(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateOutputDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDiffTrees(t *testing.T) {
	for _, tc := range []struct {
		name       string
		want, got  map[string]string
		wantDeltas []treeDelta
	}{
		{
			name: "identical",
			want: map[string]string{"a": "1", "b": "2"},
			got:  map[string]string{"a": "1", "b": "2"},
		},
		{
			name:       "changed",
			want:       map[string]string{"a": "1"},
			got:        map[string]string{"a": "9"},
			wantDeltas: []treeDelta{{Path: "a", Kind: "changed"}},
		},
		{
			name:       "missing from disk",
			want:       map[string]string{"a": "1"},
			got:        map[string]string{},
			wantDeltas: []treeDelta{{Path: "a", Kind: "missing"}},
		},
		{
			// What an entity dropped from the schema leaves behind, before
			// sweepOwned existed.
			name:       "untracked on disk",
			want:       map[string]string{},
			got:        map[string]string{"client/gone_client.go": "1"},
			wantDeltas: []treeDelta{{Path: "client/gone_client.go", Kind: "untracked"}},
		},
		{
			name: "empty both sides",
			want: map[string]string{},
			got:  map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := diffTrees(tc.want, tc.got)
			if len(got) != len(tc.wantDeltas) {
				t.Fatalf("got %v, want %v", got, tc.wantDeltas)
			}
			for i := range got {
				if got[i] != tc.wantDeltas[i] {
					t.Errorf("delta %d = %v, want %v", i, got[i], tc.wantDeltas[i])
				}
			}
		})
	}
}

// The exit codes are the contract a CI script reads. Stale and unusable are
// separate so a network fault cannot read as an out-of-date client.
func TestCheckGeneratedExitCodes(t *testing.T) {
	const caller, prefix = "shop-api", "example.com/app/gen"
	base := func() *generateManifest {
		return &generateManifest{
			Schema: manifestSchema, Caller: caller, ModulePrefix: prefix,
			Files: []manifestFile{{Path: "client/a.go", SHA256: "1"}},
		}
	}
	disk := map[string]string{"client/a.go": "1"}

	for _, tc := range []struct {
		name  string
		m     *generateManifest
		disk  map[string]string
		fresh map[string]string
		want  int
	}{
		{"current", base(), disk, nil, checkCurrent},
		{"edited by hand", base(), map[string]string{"client/a.go": "9"}, nil, checkStale},
		{"file deleted", base(), map[string]string{}, nil, checkStale},
		{"leftover file", base(), map[string]string{"client/a.go": "1", "client/b.go": "2"}, nil, checkStale},
		{
			name: "another caller's tree",
			m: &generateManifest{Schema: manifestSchema, Caller: "other",
				ModulePrefix: prefix, Files: base().Files},
			disk: disk, want: checkUnknown,
		},
		{
			name: "moved to a different module",
			m: &generateManifest{Schema: manifestSchema, Caller: caller,
				ModulePrefix: "elsewhere/gen", Files: base().Files},
			disk: disk, want: checkStale,
		},
		{"server agrees", base(), disk, map[string]string{"client/a.go": "1"}, checkCurrent},
		{"schema moved", base(), disk, map[string]string{"client/a.go": "9"}, checkStale},
		{
			// The committed tree matches its manifest, but the schema has since
			// dropped an entity: the emitters no longer render one recorded
			// file. A comparison keyed on what the emitters render would never
			// look at it and report current.
			name: "entity removed from the schema",
			m: &generateManifest{Schema: manifestSchema, Caller: caller, ModulePrefix: prefix,
				Files: []manifestFile{
					{Path: "client/a.go", SHA256: "1"},
					{Path: "client/gone_client.go", SHA256: "3"},
				}},
			disk:  map[string]string{"client/a.go": "1", "client/gone_client.go": "3"},
			fresh: map[string]string{"client/a.go": "1"},
			want:  checkStale,
		},
		{
			// The schema gained an entity: the emitters render a file the
			// manifest has never heard of.
			name:  "entity added to the schema",
			m:     base(),
			disk:  disk,
			fresh: map[string]string{"client/a.go": "1", "client/new_client.go": "4"},
			want:  checkStale,
		},
		{
			// buf's output is recorded in the manifest and not rendered by the
			// emitters, so it must not read as a disagreement.
			name: "pb files are not compared against the server",
			m: &generateManifest{Schema: manifestSchema, Caller: caller, ModulePrefix: prefix,
				Files: []manifestFile{
					{Path: "client/a.go", SHA256: "1"},
					{Path: "pb/a.pb.go", SHA256: "2"},
				}},
			disk:  map[string]string{"client/a.go": "1", "pb/a.pb.go": "2"},
			fresh: map[string]string{"client/a.go": "1"},
			want:  checkCurrent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkGenerated(tc.m, tc.disk, caller, prefix, tc.fresh)
			if got.Code != tc.want {
				t.Errorf("code = %d, want %d (%s)", got.Code, tc.want, got.Message)
			}
		})
	}
}

// A manifest that cannot be read must not pass. readCachedVersion swallows its
// errors because an unreadable cache costs a re-fetch; here it would mean
// reporting a tree nothing verified.
func TestReadManifestRefusesWhatItCannotTrust(t *testing.T) {
	dir := t.TempDir()

	if _, err := readManifest(dir); !os.IsNotExist(err) {
		t.Errorf("absent manifest: err = %v, want a not-exist error the caller can branch on", err)
	}

	path := filepath.Join(dir, generateManifestName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(dir); err == nil {
		t.Error("corrupt manifest was accepted")
	}

	if err := os.WriteFile(path, []byte(`{"schema":9999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(dir); err == nil {
		t.Error("a manifest from a later tide was accepted")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := buildManifest("shop-api", "example.com/app/gen", "v0.4.0",
		[]string{"vendorpkg", "consumer"},
		map[string]string{"client/b.go": "2", "client/a.go": "1"})

	if got := []string{m.Namespaces[0], m.Namespaces[1]}; got[0] != "consumer" {
		t.Errorf("namespaces = %v, want them sorted", got)
	}
	if m.Files[0].Path != "client/a.go" {
		t.Errorf("files = %v, want them sorted by path", m.Files)
	}

	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	back, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Caller != m.Caller || len(back.Files) != len(m.Files) {
		t.Errorf("round trip lost data: %+v", back)
	}
	if _, err := os.Stat(filepath.Join(dir, generateManifestName+".tmp")); !os.IsNotExist(err) {
		t.Error("the temporary file survived the rename")
	}
}

// sweepOwned clears what generate rewrites and nothing else. A caller may keep
// a README or notes beside the generated tree.
func TestSweepOwnedLeavesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("client/consumer/account_client.go")
	mustWrite("pb/atlantis/consumer/v1/account.pb.go")
	mustWrite("atlantis/consumer/v1/account.proto")
	mustWrite("README.md")
	mustWrite("notes/design.md")

	if err := sweepOwned(dir); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"client", "pb", "atlantis"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s/ survived the sweep", gone)
		}
	}
	for _, kept := range []string{"README.md", "notes/design.md"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

// planSDKFilesFixture lowers a small schema the way tide generate receives one
// from the server: parsed, lowered, and numbered.
func planSDKFilesFixture(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("fixture.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower fixture: %v", err)
	}
	codegen.AssignProtoNumbers(nil, ir)
	return ir
}

const planFixture = `
entity Author in library {
  id   bigint primary
  name text not null
}

entity Book in library {
  id     bigint primary
  title  text not null
  author bigint not null references library.Author.id
}
`

// planSDKFiles must not depend on the order of the IR it is handed.
//
// "Run it twice and compare" is the weaker assertion: a map-ranging emitter
// passes it roughly half the time. Reversing every slice first is what fails
// against one, because the emitters sort by path and by name and the input
// order must be invisible.
func TestPlanSDKFilesIgnoresInputOrder(t *testing.T) {
	const prefix = "example.com/app/internal/gen"

	forward := planSDKFilesFixture(t, planFixture)
	want, err := planSDKFiles(forward, prefix)
	if err != nil {
		t.Fatalf("planSDKFiles: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("the fixture produced no files")
	}

	reversed := planSDKFilesFixture(t, planFixture)
	reverse(reversed.Entities)
	reverse(reversed.Queries)
	reverse(reversed.Procedures)
	reverse(reversed.Enums)
	reverse(reversed.Jobs)
	reverse(reversed.Workflows)
	reverse(reversed.Ephemerals)

	got, err := planSDKFiles(reversed, prefix)
	if err != nil {
		t.Fatalf("planSDKFiles reversed: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("file count %d, want %d", len(got), len(want))
	}
	for path, content := range want {
		switch other, ok := got[path]; {
		case !ok:
			t.Errorf("%s is missing when the IR is reversed", path)
		case other != content:
			t.Errorf("%s differs when the IR is reversed", path)
		}
	}
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// The emitted client is what the manifest hashes, so it has to be gofmt-clean
// as written. generate no longer shells out to gofmt.
func TestPlanSDKFilesEmitsFormattedGo(t *testing.T) {
	files, err := planSDKFiles(planSDKFilesFixture(t, planFixture), "example.com/app/internal/gen")
	if err != nil {
		t.Fatalf("planSDKFiles: %v", err)
	}
	seen := 0
	for path, content := range files {
		if filepath.Ext(path) != ".go" {
			continue
		}
		seen++
		formatted, err := format.Source([]byte(content))
		if err != nil {
			t.Errorf("%s does not parse: %v", path, err)
			continue
		}
		if string(formatted) != content {
			t.Errorf("%s is not gofmt-clean as emitted", path)
		}
	}
	if seen == 0 {
		t.Fatal("no Go files were emitted")
	}
}
