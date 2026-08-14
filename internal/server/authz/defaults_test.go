package authz

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestDefaultCapabilitiesArePinned states the bundles literally. The point is
// not to restate the source — it is that widening them has to be a deliberate
// edit to a test that says "this is what registering a caller grants", rather
// than a one-line append that reviews as housekeeping.
func TestDefaultCapabilitiesArePinned(t *testing.T) {
	readOnly := Names(DefaultCapabilities(false))
	sort.Strings(readOnly)
	want := []string{"CAPABILITY_JOBS_READ", "CAPABILITY_SCHEMA_READ", "CAPABILITY_WORKERS_READ"}
	if strings.Join(readOnly, ",") != strings.Join(want, ",") {
		t.Errorf("can_mutate=false grants %v, want %v", readOnly, want)
	}

	mutating := Names(DefaultCapabilities(true))
	sort.Strings(mutating)
	wantMutating := []string{
		"CAPABILITY_JOBS_READ", "CAPABILITY_JOBS_WRITE", "CAPABILITY_SCHEMA_APPLY",
		"CAPABILITY_SCHEMA_PLAN", "CAPABILITY_SCHEMA_READ", "CAPABILITY_WORKERS_READ",
	}
	if strings.Join(mutating, ",") != strings.Join(wantMutating, ",") {
		t.Errorf("can_mutate=true grants %v, want %v", mutating, wantMutating)
	}
}

// TestDefaultsNeverIncludeOperator is the invariant the migration comment
// argues for at length: can_mutate meant "may apply my own schema", and no
// amount of it should add up to authority over other callers. OPERATOR and
// LOGS_READ are granted by hand or by the console-bootstrap migration, never
// by registration.
func TestDefaultsNeverIncludeOperator(t *testing.T) {
	for _, canMutate := range []bool{false, true} {
		for _, c := range DefaultCapabilities(canMutate) {
			if c == adminpb.Capability_CAPABILITY_OPERATOR || c == adminpb.Capability_CAPABILITY_LOGS_READ {
				t.Errorf("can_mutate=%v grants %s; cross-caller authority must be explicit", canMutate, c)
			}
		}
	}
}

// TestManagedSetCoversBothBundles proves RegisterCaller's revoke step can
// actually revoke everything its grant step can issue. If a capability were
// grantable but unmanaged, flipping can_mutate off would leave it behind — a
// demotion that looks applied and is not.
func TestManagedSetCoversBothBundles(t *testing.T) {
	managed := make(map[adminpb.Capability]bool)
	for _, c := range ManagedCapabilities() {
		managed[c] = true
	}
	for _, canMutate := range []bool{false, true} {
		for _, c := range DefaultCapabilities(canMutate) {
			if !managed[c] {
				t.Errorf("%s is granted by can_mutate=%v but is not in ManagedCapabilities", c, canMutate)
			}
		}
	}
}

// capabilityNamesIn returns the quoted CAPABILITY_* literals in a .sql file,
// excluding line comments.
//
// The exclusion is the point. Both migrations discuss capabilities in prose
// above the SQL, and 0018 line 17 quotes 'CAPABILITY_SCHEMA_READ' inside a
// comment explaining why the column stores names — so a naive regex over the
// whole file reports a grant that the VALUES list may not contain, and every
// assertion built on it passes on the strength of a sentence.
func capabilityNamesIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	quoted := regexp.MustCompile(`'(CAPABILITY_[A-Z_]+)'`)
	out := make(map[string]bool)
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		for _, m := range quoted.FindAllStringSubmatch(line, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// TestBundlesMatchTheBackfillMigration ties the Go rule to the SQL that applied
// it once, historically.
//
// Two callers with the same can_mutate flag, one registered before migration
// 0018 and one after, must hold the same grants. Nothing in the type system
// connects a string literal in a .sql file to a []Capability in Go, so the
// check is a read of the file: every capability the Go bundles issue must be
// granted by the migration, and every name the migration grants must be one
// this binary recognizes.
//
// The two bundles are checked separately against the migration's two blocks.
// Checking a flat union would pass if CAPABILITY_SCHEMA_APPLY moved out of the
// `WHERE ci.can_mutate` block into the unconditional one — which would grant
// apply to every caller in the deployment.
func TestBundlesMatchTheBackfillMigration(t *testing.T) {
	const path = "../../../migrations/infra/0018_caller_capabilities.up.sql"
	inMigration := capabilityNamesIn(t, path)
	if len(inMigration) == 0 {
		t.Fatalf("%s grants no quoted capability outside comments; the pattern no longer matches", path)
	}
	for name := range inMigration {
		if _, ok := adminpb.Capability_value[name]; !ok {
			t.Errorf("migration grants %s, which is not a Capability this binary recognizes", name)
		}
	}

	// The console-only grants are in the migration but not in the registration
	// bundles, by design; the containment therefore runs one way.
	for _, name := range Names(ManagedCapabilities()) {
		if !inMigration[name] {
			t.Errorf("RegisterCaller grants %s but the 0018 backfill does not, so a caller "+
				"registered before the migration holds less than one registered after", name)
		}
	}

	// Per-block: the unconditional INSERT must carry exactly the read bundle,
	// and the `WHERE ci.can_mutate` INSERT exactly the mutating additions.
	unconditional, gated := backfillBlocks(t, path)
	assertSameSet(t, "0018 unconditional backfill", unconditional, Names(DefaultCapabilities(false)))
	assertSameSet(t, "0018 can_mutate backfill", gated, mutatingOnlyNames())
}

// backfillBlocks splits 0018's two caller-wide INSERTs by whether the statement
// carries a `WHERE ci.can_mutate` clause. The console block is skipped: it is
// keyed on a specific caller, not on the flag.
func backfillBlocks(t *testing.T, path string) (unconditional, gated map[string]bool) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var stripped []string
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		stripped = append(stripped, line)
	}
	quoted := regexp.MustCompile(`'(CAPABILITY_[A-Z_]+)'`)
	unconditional, gated = map[string]bool{}, map[string]bool{}
	for _, stmt := range strings.Split(strings.Join(stripped, "\n"), ";") {
		if !strings.Contains(stmt, "INSERT INTO atlantis.caller_capabilities") {
			continue
		}
		if strings.Contains(stmt, "ci.caller =") {
			continue // the console block
		}
		target := unconditional
		if strings.Contains(stmt, "WHERE ci.can_mutate") {
			target = gated
		}
		for _, m := range quoted.FindAllStringSubmatch(stmt, -1) {
			target[m[1]] = true
		}
	}
	if len(unconditional) == 0 || len(gated) == 0 {
		t.Fatalf("%s: expected two caller-wide INSERT blocks, found %d unconditional and %d gated",
			path, len(unconditional), len(gated))
	}
	return unconditional, gated
}

// mutatingOnlyNames is what can_mutate adds on top of the read bundle.
func mutatingOnlyNames() []string {
	base := map[string]bool{}
	for _, n := range Names(DefaultCapabilities(false)) {
		base[n] = true
	}
	var out []string
	for _, n := range Names(DefaultCapabilities(true)) {
		if !base[n] {
			out = append(out, n)
		}
	}
	return out
}

func assertSameSet(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s omits %s", what, name)
		}
	}
	wanted := map[string]bool{}
	for _, name := range want {
		wanted[name] = true
	}
	for name := range got {
		if !wanted[name] {
			t.Errorf("%s grants %s, which the Go bundle does not", what, name)
		}
	}
}

// TestConsoleBootstrapGrantsEveryRPCTheConsoleCalls is the fresh-install path,
// checked against what the console actually does rather than against a list.
//
// Removing the admin allowlist exemption means the console reaches admin RPCs
// only if it holds grants, and it holds grants only if a migration seeds them.
// Without the identity row the first boot of a new deployment has no identity
// able to call RegisterCaller — the RPC that creates the first identity — and
// the install is unrecoverable over gRPC. 0019 is where that row is written, so
// that one claim is pinned to 0019 by name.
//
// A missing grant for any *other* RPC fails quietly instead: the server starts,
// and one console page returns PermissionDenied to whoever happens to open it.
// So the expectation is computed — every RPC name appearing in internal/console
// source, mapped through the proto's own declarations — and a new console
// feature calling a new RPC fails here until some migration grants what it
// needs.
//
// # Why the grants are scanned across every migration, not just 0019
//
// 0019 has already run on every deployment that exists. A grant appended to it
// now would reach fresh installs and nothing else, so the upgrade path — the
// one with users on it — would get a console page that returns PermissionDenied
// forever. New grants therefore go in the migration that introduces the feature
// needing them, and this reads the union.
//
// The union is discovered rather than listed. An earlier version of this test
// named one file, and the first feature to add a console grant elsewhere failed
// it with a message blaming the wrong migration.
func TestConsoleBootstrapGrantsEveryRPCTheConsoleCalls(t *testing.T) {
	const migrationsDir = "../../../migrations/infra"

	if !capabilityNamesIn(t, migrationsDir+"/0019_console_identity.up.sql")["CAPABILITY_OPERATOR"] {
		t.Fatal("0019 does not grant the console CAPABILITY_OPERATOR; a fresh install " +
			"would have no identity able to call RegisterCaller")
	}

	granted := map[string]bool{}
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}
	var seeding []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		body, err := os.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// Only files that actually name the console identity outside a comment
		// count, so prose about a capability is not read as a grant of it.
		if !strings.Contains(stripSQLLineComments(string(body)), "'atlantis-console'") {
			continue
		}
		seeding = append(seeding, e.Name())
		for name := range capabilityNamesIn(t, migrationsDir+"/"+e.Name()) {
			granted[name] = true
		}
	}
	if len(seeding) == 0 {
		t.Fatal("no migration seeds grants for 'atlantis-console'; the scan is no " +
			"longer finding them and this test proves nothing")
	}

	policy, err := AdminPolicy()
	if err != nil {
		t.Fatal(err)
	}
	methods := policy.Methods()

	called := rpcNamesReferencedIn(t, "../../console", methods)
	if len(called) < 10 {
		t.Fatalf("found only %d admin RPCs referenced in internal/console (%v); the scan is "+
			"no longer finding the console's call sites and this test proves nothing", len(called), called)
	}
	for _, name := range called {
		want := methods[name].String()
		if !granted[want] {
			t.Errorf("the console calls %s, which requires %s, but none of %v grants it — "+
				"that page returns PermissionDenied on every deployment", name, want, seeding)
		}
	}
}

// stripSQLLineComments drops `-- ...` so a migration that discusses the console
// in prose is not counted as granting it anything.
func stripSQLLineComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// rpcNamesReferencedIn returns the admin RPC names that appear as string
// literals in the Go sources under dir.
//
// Intersecting quoted strings with the descriptor's method set is what makes
// this robust: the console names its methods as literals passed to
// adminClient.invoke, and a string that is not an RPC name cannot survive the
// intersection. It would miss a dynamically-constructed method name, which is
// why the caller asserts a floor on the count.
func rpcNamesReferencedIn(t *testing.T, dir string, methods map[string]adminpb.Capability) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	literal := regexp.MustCompile(`"([A-Z][A-Za-z]+)"`)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(body), -1) {
			if _, ok := methods[m[1]]; ok {
				seen[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Nothing RegisterCaller hands out may include CAPABILITY_SCHEMA_APPROVE.
//
// The separation this asserts is the whole reason the capability is not just
// folded into CAPABILITY_OPERATOR: the identity that wants a schema change must
// never be the identity that permits it. Today that holds because applying
// identities are machine cert CNs registered through RegisterCaller, and the
// only holder of SCHEMA_APPROVE is the console — which 0019 deliberately
// withheld SCHEMA_APPLY from.
//
// It would stop holding the moment somebody appends this to a bundle to make a
// CI pipeline stop asking. That edit reads as a one-line convenience; this is
// what makes it read as removing the gate.
func TestApproveIsInNoRegistrationBundle(t *testing.T) {
	for _, canMutate := range []bool{false, true} {
		for _, name := range Names(DefaultCapabilities(canMutate)) {
			if name == "CAPABILITY_SCHEMA_APPROVE" {
				t.Errorf("DefaultCapabilities(can_mutate=%v) grants SCHEMA_APPROVE. "+
					"Every caller that can apply could then approve its own change, "+
					"and the policy would decide nothing.", canMutate)
			}
		}
	}
	for _, name := range Names(ManagedCapabilities()) {
		if name == "CAPABILITY_SCHEMA_APPROVE" {
			t.Error("ManagedCapabilities includes SCHEMA_APPROVE, so re-registering a " +
				"caller would reconcile it — granting or revoking approval authority " +
				"as a side effect of an unrelated identity edit")
		}
	}
}
