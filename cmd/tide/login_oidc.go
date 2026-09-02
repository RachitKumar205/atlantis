package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// `tide login --oidc`: a CI workload trades its ambient OIDC token for a
// short-lived certificate, under a federation rule the organisation
// configured in the console.
//
// Nothing is stored between runs and nothing needs to be: the certificate
// lands in the runner's ephemeral store and dies with the job, renewable
// only within the budget the rule set.

// oidcTokenTimeout bounds the fetch of the runner's own token.
const oidcTokenTimeout = 15 * time.Second

// oidcLogin resolves the workload token and enrols with it.
func oidcLogin(caller, enrollURL, org, audience, caFile string) int {
	if enrollURL == "" {
		enrollURL = os.Getenv("ATL_ENROLL_URL")
	}
	if enrollURL == "" {
		fmt.Fprintln(os.Stderr, "tide login: --oidc needs the enrolment address; set ATL_ENROLL_URL or pass --url")
		return 3
	}
	if org == "" {
		fmt.Fprintln(os.Stderr, "tide login: --oidc needs the organisation; set `org:` in tide.yaml, ATL_ORG, or --org")
		return 3
	}

	idToken, err := workloadIDToken(audience)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide login:", err)
		return 3
	}

	bundle, err := enrol(enrollURL, org, caFile, map[string]string{
		"id_token": idToken,
		"caller":   caller,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}
	return storeAndReport(bundle)
}

// workloadIDToken fetches the runner's OIDC token from whatever ambient
// issuer is present. GitHub Actions today; the environment variables are the
// detection.
func workloadIDToken(audience string) (string, error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", fmt.Errorf("no workload identity is available here.\n\n" +
			"On GitHub Actions, grant it in the workflow:\n\n" +
			"  permissions:\n    id-token: write")
	}

	u := reqURL
	if audience != "" {
		sep := "&"
		if !containsQuery(u) {
			sep = "?"
		}
		u += sep + "audience=" + url.QueryEscape(audience)
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+reqTok)
	client := &http.Client{Timeout: oidcTokenTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch the workload token: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return "", fmt.Errorf("unreadable token response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || out.Value == "" {
		return "", fmt.Errorf("the runner refused to mint a token (status %d)", resp.StatusCode)
	}
	return out.Value, nil
}

func containsQuery(u string) bool {
	for _, r := range u {
		if r == '?' {
			return true
		}
	}
	return false
}
