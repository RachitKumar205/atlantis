package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The runner's token endpoint requires its bearer and passes the audience
// through; the flow refuses cleanly where no workload identity exists.
func TestWorkloadIDToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runner-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("audience"); got != "atlantis-enroll" {
			t.Errorf("audience = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "the-id-token"})
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/token?api-version=2")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-token")

	tok, err := workloadIDToken("atlantis-enroll")
	if err != nil {
		t.Fatalf("workloadIDToken: %v", err)
	}
	if tok != "the-id-token" {
		t.Errorf("token = %q", tok)
	}
}

func TestWorkloadIDTokenRefusesOutsideCI(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	if _, err := workloadIDToken("x"); err == nil {
		t.Error("no ambient identity was accepted")
	}
}
