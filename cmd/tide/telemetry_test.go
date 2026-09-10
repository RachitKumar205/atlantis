package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captured is one delivery the fake ingestion route received.
type captured struct {
	path string
	body map[string]any
}

// ingestion stands in for the deployment's /api/t/batch route.
func ingestion(t *testing.T, got *[]captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = append(*got, captured{path: r.URL.Path, body: body})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// event returns the single event of the single delivery.
func event(t *testing.T, got []captured) map[string]any {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(got))
	}
	batch, ok := got[0].body["batch"].([]any)
	if !ok || len(batch) != 1 {
		t.Fatalf("batch is %v, want one event", got[0].body["batch"])
	}
	e, _ := batch[0].(map[string]any)
	return e
}

// isolate points the credential store and the Cloud address at this test.
func isolate(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("ATLANTIS_HOME", t.TempDir())
	t.Setenv("ATL_CLOUD_URL", endpoint)
	t.Setenv("ATL_ORG", "")
	// tide.yaml is read from the working directory, and this package's own
	// directory has none.
}

func TestAFinishedCommandIsReported(t *testing.T) {
	var got []captured
	srv := ingestion(t, &got)
	isolate(t, srv.URL)

	reportRun("apply", 0, 1500*time.Millisecond)

	if got[0].path != "/api/t/batch" {
		t.Errorf("posted to %q, want /api/t/batch", got[0].path)
	}
	if _, ok := got[0].body["api_key"]; ok {
		t.Errorf("the CLI sent a project key: %v", got[0].body)
	}

	e := event(t, got)
	if e["event"] != "cli.command" {
		t.Errorf("event %q, want cli.command", e["event"])
	}
	props, _ := e["properties"].(map[string]any)
	if props["command"] != "apply" {
		t.Errorf("command is %v, want apply", props["command"])
	}
	if props["outcome"] != "ok" {
		t.Errorf("outcome is %v, want ok", props["outcome"])
	}
	if props["took_ms"] != float64(1500) {
		t.Errorf("took_ms is %v, want 1500", props["took_ms"])
	}
	if props["$process_person_profile"] != false {
		t.Errorf("a command line created a person profile: %v", props)
	}
}

// A failing command reports the outcome and the code, and nothing else about
// why it failed.
func TestAFailedCommandReportsNoReason(t *testing.T) {
	var got []captured
	srv := ingestion(t, &got)
	isolate(t, srv.URL)

	reportRun("plan", 1, time.Second)

	props, _ := event(t, got)["properties"].(map[string]any)
	if props["outcome"] != "failed" {
		t.Errorf("outcome is %v, want failed", props["outcome"])
	}
	if props["exit_code"] != float64(1) {
		t.Errorf("exit_code is %v, want 1", props["exit_code"])
	}
}

// A subcommand nobody recognises is reported as "unknown".
//
// main's default arm reaches reportRun with whatever was typed, and a shell
// history is not something to forward.
func TestAnUnknownSubcommandIsNotEchoed(t *testing.T) {
	var got []captured
	srv := ingestion(t, &got)
	isolate(t, srv.URL)

	reportRun("s3://acme-secrets/prod.env", 2, time.Second)

	encoded, _ := json.Marshal(event(t, got))
	if strings.Contains(string(encoded), "acme-secrets") {
		t.Errorf("the typed argument crossed:\n%s", encoded)
	}
	props, _ := event(t, got)["properties"].(map[string]any)
	if props["command"] != "unknown" {
		t.Errorf("command is %v, want unknown", props["command"])
	}
}

// With no Cloud address there is nowhere to report, and the command still
// finishes.
func TestNoCloudAddressReportsNothing(t *testing.T) {
	var got []captured
	srv := ingestion(t, &got)
	_ = srv
	t.Setenv("ATLANTIS_HOME", t.TempDir())
	t.Setenv("ATL_CLOUD_URL", "")

	reportRun("apply", 0, time.Second)

	if len(got) != 0 {
		t.Fatalf("got %d deliveries with no address, want 0", len(got))
	}
}

// An unreachable deployment costs the command nothing but the budget.
func TestAnUnreachableDeploymentIsSilent(t *testing.T) {
	t.Setenv("ATLANTIS_HOME", t.TempDir())
	t.Setenv("ATL_CLOUD_URL", "http://127.0.0.1:1")

	start := time.Now()
	reportRun("apply", 0, time.Second)
	if elapsed := time.Since(start); elapsed > telemetryBudget+time.Second {
		t.Fatalf("a refused connection took %v", elapsed)
	}
}

// The installation id is written once and reused.
func TestTheInstallationIDIsStable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ATLANTIS_HOME", home)

	first, err := installationID()
	if err != nil {
		t.Fatalf("installationID: %v", err)
	}
	second, err := installationID()
	if err != nil {
		t.Fatalf("installationID again: %v", err)
	}
	if first != second {
		t.Errorf("two runs reported %q and %q", first, second)
	}
	if len(first) != 32 {
		t.Errorf("id %q is %d characters, want 32", first, len(first))
	}

	raw, err := os.ReadFile(filepath.Join(home, "installation"))
	if err != nil {
		t.Fatalf("the id was not persisted: %v", err)
	}
	if strings.TrimSpace(string(raw)) != first {
		t.Errorf("the file holds %q, want %q", strings.TrimSpace(string(raw)), first)
	}
}

// Every arm of main's switch is reportable, so a real command is never
// recorded as "unknown".
func TestEverySubcommandIsAllowlisted(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var found int
	for _, line := range strings.Split(string(source), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `case "`) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, `case "`), `":`)
		if strings.Contains(name, `"`) {
			continue
		}
		found++
		if !telemetryCommands[name] {
			t.Errorf("main dispatches %q, which telemetryCommands does not list", name)
		}
	}
	if found < 20 {
		t.Fatalf("found %d subcommands in main.go, want at least 20", found)
	}
}
