package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// fakeCloud answers the device-flow routes: pending twice, a slow_down, then
// approval. What the client must survive is exactly this sequence.
func fakeCloud(t *testing.T, enrollURL string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cli/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Caller, Hostname string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Caller == "" {
			http.Error(w, `{"error":"caller required"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "device-abc",
			"user_code":   "QK7T-XW2M",
			"verify_url":  "https://cloud.test/cli",
			"interval":    0, // the client must not take zero at face value
			"expires_in":  900,
		})
	})
	mux.HandleFunc("POST /api/cli/poll", func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "slow_down", "retry_in": 1})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "approved", "assertion": "jwt-here",
				"org": "acme", "role": "admin", "enroll_url": enrollURL,
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestDeviceFlowFollowsTheServerPacing(t *testing.T) {
	cloud, polls := fakeCloud(t, "https://enroll.test")

	start, err := startDeviceLogin(cloud.URL, "api", "dev-box")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start.UserCode != "QK7T-XW2M" {
		t.Errorf("user code = %q", start.UserCode)
	}
	if start.Interval <= 0 {
		t.Errorf("a zero interval survived normalisation: %d", start.Interval)
	}

	start.Interval = 0 // keep the test fast; pacing correctness is the sequence
	got, err := waitForApproval(cloud.URL, start)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got.Org != "acme" || got.Assertion != "jwt-here" || got.EnrollURL != "https://enroll.test" {
		t.Errorf("approval = %+v", got)
	}
	if n := polls.Load(); n != 3 {
		t.Errorf("%d polls; pending and slow_down were not each honoured once", n)
	}
}

func TestDeviceFlowStopsOnDenial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cli/start", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "d", "user_code": "u", "verify_url": "v", "interval": 1})
	})
	mux.HandleFunc("POST /api/cli/poll", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "denied"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start, err := startDeviceLogin(srv.URL, "api", "dev-box")
	if err != nil {
		t.Fatal(err)
	}
	start.Interval = 0
	if _, err := waitForApproval(srv.URL, start); err == nil {
		t.Error("a denial kept the client waiting")
	}
}

// The caller comes from tide.yaml where the command runs in a caller
// repository, and the flag wins over it. With neither, the error names both.
func TestDeviceCallerResolution(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := deviceCaller(""); err == nil {
		t.Error("no tide.yaml and no flag was accepted")
	}
	if got, err := deviceCaller("flagged"); err != nil || got != "flagged" {
		t.Errorf("flag alone = %q, %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tide.yaml"),
		[]byte("caller: from-yaml\nschema_paths: [\".\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := deviceCaller(""); err != nil || got != "from-yaml" {
		t.Errorf("tide.yaml = %q, %v", got, err)
	}
	if got, err := deviceCaller("flagged"); err != nil || got != "flagged" {
		t.Errorf("the flag must win over tide.yaml: %q, %v", got, err)
	}
}
