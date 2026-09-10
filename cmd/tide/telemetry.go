package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// What a tide run reports, and what it does not.
//
// The event carries the subcommand, whether it succeeded, how long it took,
// the version and the platform. It carries no path, no flag value, no schema,
// no error text and no address: a command line is a developer's own machine
// and most of what is on it belongs to them.
//
// There is no project key in this binary. The event goes to the deployment's
// own ingestion route, which substitutes the key server-side — the same path
// the browsers use.
//
// A run that cannot reach the network, has no configured deployment, or is
// cancelled reports nothing and says nothing. Telemetry never changes what a
// command prints or how long a developer waits for it beyond telemetryBudget.
const telemetryBudget = 2 * time.Second

// telemetryCommands are the subcommands that may appear in an event.
//
// An allowlist because main's default arm reaches here too: `tide <anything>`
// would otherwise send whatever was typed, and what people type at a shell
// includes things they would not choose to send.
var telemetryCommands = map[string]bool{
	"init": true, "apply": true, "plan": true, "rehearse": true, "inspect": true,
	"pull": true, "generate": true, "list": true, "show": true, "backfill": true,
	"job": true, "workflow": true, "history": true, "diff": true, "blame": true,
	"owners": true, "parked": true, "rollback": true, "sandbox": true,
	"caller": true, "login": true, "version": true,
}

// reportRun delivers one event for a finished command.
//
// Errors are dropped: a developer running `tide plan` on a train is not told
// about analytics.
func reportRun(command string, code int, took time.Duration) {
	endpoint, err := cloudBaseURL()
	if err != nil || endpoint == "" {
		return
	}
	id, err := installationID()
	if err != nil {
		return
	}
	if !telemetryCommands[command] {
		command = "unknown"
	}
	outcome := "ok"
	if code != 0 {
		outcome = "failed"
	}

	ctx, cancel := context.WithTimeout(context.Background(), telemetryBudget)
	defer cancel()

	_ = analytics.PostOnce(ctx, strings.TrimRight(endpoint, "/"), analytics.Event{
		Name:       analytics.EventCLICommand,
		DistinctID: id,
		Org:        telemetryOrg(),
		Props: map[string]any{
			"command":   command,
			"outcome":   outcome,
			"exit_code": code,
			"took_ms":   took.Milliseconds(),
			"version":   version,
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
		},
		// A command line has no person behind it that this binary can name:
		// the credential store holds an organisation and a caller, never a
		// Cloud user.
		NoPersonProfile: true,
	})
}

// installationID is a random identifier for this credential store, created on
// first use.
//
// It distinguishes one installation from another without naming anything: no
// hostname, no username, no machine serial. Deleting the store forgets it, and
// a store that cannot be written reports nothing rather than inventing a fresh
// identity on every run.
func installationID() (string, error) {
	root, err := storeRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "installation")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// telemetryOrg is the organisation this checkout belongs to, or "".
//
// Read from tide.yaml at the path every command defaults to. A --config
// elsewhere, a checkout with no config, or a credential store holding several
// organisations all report no group rather than a guessed one.
func telemetryOrg() string {
	cfg, err := loadPCConfig("tide.yaml")
	if err != nil || cfg == nil {
		return ""
	}
	org, err := cfg.resolveOrg()
	if err != nil {
		return ""
	}
	return org
}
