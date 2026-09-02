package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// checkOutcome is what `tide generate --check` decided, and the exit code it
// answers with.
//
// Stale and unusable are separate codes. A pipeline that cannot tell a
// network fault from an out-of-date client will eventually treat both as
// noise.
type checkOutcome struct {
	Code    int
	Deltas  []treeDelta
	Message string
}

const (
	checkCurrent = 0 // the tree matches
	checkStale   = 1 // it does not
	checkUnknown = 3 // the question could not be answered
)

// checkGenerated compares a manifest against a tree, and optionally against
// what the emitters would produce now.
//
// Pure: the caller supplies the manifest, the hashes it read off disk, and —
// for the server comparison — the hashes of a fresh render. Nothing here
// touches the filesystem or the network.
//
// TideVersion is not compared. main.go defaults version to "dev", so a release
// binary and `go run ./cmd/tide` would disagree on every run.
func checkGenerated(m *generateManifest, disk map[string]string, caller, modulePrefix string, fresh map[string]string) checkOutcome {
	if m.Caller != "" && m.Caller != caller {
		return checkOutcome{
			Code: checkUnknown,
			Message: fmt.Sprintf("the manifest was written for caller %q and this is %q",
				m.Caller, caller),
		}
	}
	if m.ModulePrefix != "" && m.ModulePrefix != modulePrefix {
		return checkOutcome{
			Code: checkStale,
			Message: fmt.Sprintf("the client was generated for module prefix %q and this repository is %q",
				m.ModulePrefix, modulePrefix),
		}
	}

	if d := diffTrees(manifestHashes(m), disk); len(d) > 0 {
		return checkOutcome{Code: checkStale, Deltas: d,
			Message: "the generated client does not match its manifest"}
	}

	if fresh == nil {
		return checkOutcome{Code: checkCurrent}
	}
	// Everything the manifest records except pb/, which buf produced and the
	// emitters do not render. Filtering by the manifest's paths rather than by
	// fresh's keys: a recorded file the emitters no longer render is an entity
	// the schema dropped, and a subset built from fresh would never compare it.
	recorded := manifestHashes(m)
	subset := make(map[string]string, len(recorded))
	for p, h := range recorded {
		if !strings.HasPrefix(p, "pb/") {
			subset[p] = h
		}
	}
	if d := diffTrees(subset, fresh); len(d) > 0 {
		return checkOutcome{Code: checkStale, Deltas: d,
			Message: "the schema has moved since this client was generated"}
	}
	return checkOutcome{Code: checkCurrent}
}

// runCheck reads the tree and reports. Writes nothing.
func runCheck(cfg *tideConfig, outDir, modulePrefix string, againstServer bool, timeout time.Duration) int {
	m, err := readManifest(outDir)
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "tide generate: %s has no %s — run `tide generate`\n",
			outDir, generateManifestName)
		return checkStale
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return checkUnknown
	}

	disk, err := hashOwnedTree(outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return checkUnknown
	}

	var fresh map[string]string
	if againstServer {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		ir, _, err := fetchCanonicalIR(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tide generate:", err)
			return checkUnknown
		}
		planned, err := planSDKFiles(codegen.FilterIR(ir, cfg.Generate), modulePrefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tide generate:", err)
			return checkUnknown
		}
		fresh = hashContents(planned)
	}

	out := checkGenerated(m, disk, cfg.Caller, modulePrefix, fresh)
	switch out.Code {
	case checkCurrent:
		return 0
	case checkUnknown:
		fmt.Fprintln(os.Stderr, "tide generate:", out.Message)
		return checkUnknown
	}

	cliout.Errorf("%s", out.Message)
	for _, d := range out.Deltas {
		fmt.Fprintf(os.Stderr, "      %s\n", d)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "      bring it up to date with: %s\n", cliout.Bold("tide generate"))
	return checkStale
}

// recordGenerateProvenance notes the checkpoint the client was generated
// against, beside the pull cache.
//
// Not in the manifest: the hash covers every caller's schema merged together
// and moves when an unrelated caller applies, so a committed file holding it
// would differ from itself while the client stayed identical. Here it is
// gitignored, and a failure to write it is not worth failing a generate over.
func recordGenerateProvenance(contentHash string) {
	if contentHash == "" {
		return
	}
	raw, err := json.Marshal(struct {
		ContentHash string `json:"content_hash"`
		At          string `json:"at"`
	}{contentHash, time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return
	}
	if err := os.MkdirAll(tideCacheDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(tideCacheDir, "generate.json"), raw, 0o644)
}
