package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// startCLI opens a grant and returns the two codes.
func (f *fixture) startCLI(t *testing.T, caller string) (deviceCode, userCode string) {
	t.Helper()
	rec := f.post(t, "/api/cli/start",
		`{"caller":`+jsonString(caller)+`,"hostname":"dev-box"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		VerifyURL  string `json:"verify_url"`
		Interval   int    `json:"interval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.Interval <= 0 {
		t.Fatalf("start response incomplete: %s", rec.Body.String())
	}
	return out.DeviceCode, out.UserCode
}

func (f *fixture) pollCLI(t *testing.T, deviceCode string) (status string, body map[string]any) {
	t.Helper()
	rec := f.post(t, "/api/cli/poll", `{"device_code":`+jsonString(deviceCode)+`}`)
	if rec.Code != http.StatusOK {
		return fmt.Sprintf("http %d", rec.Code), nil
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	s, _ := body["status"].(string)
	return s, body
}

// cliAssertionClaims decodes the payload without verifying — these tests hold
// the issuer, and what is under test is which claims were minted.
func cliAssertionClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := 0
	start, end := 0, 0
	for i, r := range token {
		if r == '.' {
			parts++
			if parts == 1 {
				start = i + 1
			}
			if parts == 2 {
				end = i
			}
		}
	}
	if parts != 2 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[start:end])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	return claims
}

func TestCLIGrantLifecycle(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "dev@example.com")
	session, _, _ := f.enrol(t, "dev@example.com")
	f.alsoMember(t, "dev@example.com", "acme", "https://console.test", identity.RoleAdmin)
	if err := f.db.SetEnrollURL(context.Background(), "acme", "https://enroll.test"); err != nil {
		t.Fatal(err)
	}

	deviceCode, userCode := f.startCLI(t, "api")

	if status, _ := f.pollCLI(t, deviceCode); status != "pending" {
		t.Fatalf("before approval, poll = %q, want pending", status)
	}

	look := f.postJSON(t, "/api/cli/lookup", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`}`)
	if look.Code != http.StatusOK {
		t.Fatalf("lookup: %d %s", look.Code, look.Body.String())
	}
	var shown struct{ Caller, Hostname string }
	if err := json.Unmarshal(look.Body.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Caller != "api" || shown.Hostname != "dev-box" {
		t.Errorf("the approval page would show %+v; the person cannot see what is asking", shown)
	}

	dec := f.postJSON(t, "/api/cli/decide", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`,"org":"acme","approve":true}`)
	if dec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", dec.Code, dec.Body.String())
	}

	status, body := f.pollCLI(t, deviceCode)
	if status != "approved" {
		t.Fatalf("after approval, poll = %q (%v)", status, body)
	}
	if got := body["enroll_url"]; got != "https://enroll.test" {
		t.Errorf("enroll_url = %v", got)
	}
	if got := body["org"]; got != "acme" {
		t.Errorf("org = %v", got)
	}

	claims := cliAssertionClaims(t, body["assertion"].(string))
	if got := claims["purpose"]; got != "cli-enroll" {
		t.Errorf("purpose = %v — the session exchange would accept this token", got)
	}
	if got := claims["caller"]; got != "api" {
		t.Errorf("caller claim = %v, want the caller the person approved", got)
	}
	if got := claims["org"]; got != "acme" {
		t.Errorf("org claim = %v", got)
	}
	aud, _ := claims["aud"].(string)
	if aud != "https://enroll.test" {
		t.Errorf("aud = %v, want the enrolment listener", claims["aud"])
	}

	// The pickup spent the grant. A second poll is a replay and learns only
	// "start over".
	rec := f.post(t, "/api/cli/poll", `{"device_code":`+jsonString(deviceCode)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("second pickup answered %d, want a refusal", rec.Code)
	}
}

func TestCLIGrantDenial(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "dev@example.com")
	session, _, _ := f.enrol(t, "dev@example.com")
	f.alsoMember(t, "dev@example.com", "acme", "", identity.RoleAdmin)

	deviceCode, userCode := f.startCLI(t, "api")
	dec := f.postJSON(t, "/api/cli/decide", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`,"approve":false}`)
	if dec.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", dec.Code, dec.Body.String())
	}
	if status, _ := f.pollCLI(t, deviceCode); status != "denied" {
		t.Errorf("poll after denial = %q", status)
	}
}

func TestCLIGrantRefusesANonMemberOrg(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "dev@example.com")
	session, _, _ := f.enrol(t, "dev@example.com")
	f.alsoMember(t, "dev@example.com", "acme", "", identity.RoleAdmin)

	deviceCode, userCode := f.startCLI(t, "api")
	dec := f.postJSON(t, "/api/cli/decide", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`,"org":"globex","approve":true}`)
	if dec.Code != http.StatusForbidden {
		t.Fatalf("approving into a foreign org answered %d", dec.Code)
	}
	// The failed approval charged the grant, and the grant is still pending.
	if status, _ := f.pollCLI(t, deviceCode); status != "pending" {
		t.Errorf("poll = %q, want still pending", status)
	}
}

// Five misses burn the grant: the short code's guessing budget is the
// attempt counter, not its entropy.
func TestCLIGrantAttemptsBurn(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "dev@example.com")
	session, _, _ := f.enrol(t, "dev@example.com")
	f.alsoMember(t, "dev@example.com", "acme", "", identity.RoleAdmin)

	_, userCode := f.startCLI(t, "api")
	for range 5 {
		f.db.ChargeCLIGrantAttempt(context.Background(), userCode)
	}
	look := f.postJSON(t, "/api/cli/lookup", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`}`)
	if look.Code != http.StatusNotFound {
		t.Errorf("a burned grant still answered %d", look.Code)
	}
	dec := f.postJSON(t, "/api/cli/decide", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`,"org":"acme","approve":true}`)
	if dec.Code != http.StatusNotFound {
		t.Errorf("a burned grant could still be decided: %d", dec.Code)
	}
}

// The poll rides its own limiter, keyed on the device code. Six consecutive
// polls from one address inside a minute exceed the 5/min budget the auth
// routes share; every one must still be answered.
func TestCLIPollDoesNotSpendTheSignInBudget(t *testing.T) {
	f := newFixture(t)
	deviceCode, _ := f.startCLI(t, "api")

	ip := f.nextIP()
	for i := range 6 {
		rec := f.postFrom(t, ip, "/api/cli/poll",
			`{"device_code":`+jsonString(deviceCode)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll %d answered %d", i+1, rec.Code)
		}
		var out struct{ Status string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Status != "pending" {
			t.Fatalf("poll %d = %q, want pending", i+1, out.Status)
		}
	}
}

func TestCLIPollRefusesWhereNoEnrollURLIsRegistered(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "dev@example.com")
	session, _, _ := f.enrol(t, "dev@example.com")
	f.alsoMember(t, "dev@example.com", "acme", "", identity.RoleAdmin)

	deviceCode, userCode := f.startCLI(t, "api")
	dec := f.postJSON(t, "/api/cli/decide", session, f.srvOrigin(),
		`{"user_code":`+jsonString(userCode)+`,"org":"acme","approve":true}`)
	if dec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", dec.Code, dec.Body.String())
	}
	rec := f.post(t, "/api/cli/poll", `{"device_code":`+jsonString(deviceCode)+`}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("poll with no enroll_url answered %d, want 503 naming what is missing", rec.Code)
	}
}
