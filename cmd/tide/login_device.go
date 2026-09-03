package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// `tide login` with no flags: the device-code flow against Atlantis Cloud.
//
// The CLI opens a grant, prints a short code, and polls. The person signs in
// on any browser, types the code, and approves; the poll then hands back a
// two-minute assertion and the address of the organisation's enrolment
// listener, where the ordinary certificate exchange runs. Everything below
// the assertion — CSR, bundle, store, renewal — is the token flow's code.

// defaultCloudURL is where Atlantis Cloud lives, baked into release builds
// via -ldflags. Empty in a development build, where ATL_CLOUD_URL says.
var defaultCloudURL = ""

// devicePollBudget caps the whole wait, matching the grant's own TTL so the
// CLI gives up when the server has.
const devicePollBudget = 15 * time.Minute

type deviceStart struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	VerifyURL  string `json:"verify_url"`
	Interval   int    `json:"interval"`
	ExpiresIn  int    `json:"expires_in"`
}

type devicePoll struct {
	Status    string `json:"status"`
	RetryIn   int    `json:"retry_in"`
	Assertion string `json:"assertion"`
	Org       string `json:"org"`
	Role      string `json:"role"`
	EnrollURL string `json:"enroll_url"`
	Error     string `json:"error"`
}

// cloudBaseURL resolves where Cloud is, and refuses when nothing says.
func cloudBaseURL() (string, error) {
	if v := os.Getenv("ATL_CLOUD_URL"); v != "" {
		return v, nil
	}
	if defaultCloudURL != "" {
		return defaultCloudURL, nil
	}
	return "", fmt.Errorf("this build has no Cloud address baked in; set ATL_CLOUD_URL")
}

// deviceCaller resolves which caller to enrol as: tide.yaml's `caller:` where
// the command runs in a caller repository, else the flag.
func deviceCaller(callerFlag string) (string, error) {
	if callerFlag != "" {
		return callerFlag, nil
	}
	if cfg, err := parseTideConfig("tide.yaml"); err == nil && cfg.Caller != "" {
		return cfg.Caller, nil
	}
	return "", fmt.Errorf("which caller? Run this in a repository whose "+
		"tide.yaml names one — `tide init --caller %s` writes it — or pass --caller",
		"<name>")
}

func deviceLogin(caller, caFile string) int {
	cloudURL, err := cloudBaseURL()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide login:", err)
		return 3
	}

	hostname, _ := os.Hostname()
	start, err := startDeviceLogin(cloudURL, caller, hostname)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide login:", err)
		return 3
	}

	cliout.Header(os.Stdout, "tide login")
	cliout.Row(os.Stdout, "brass", start.UserCode, "type this code in the browser")
	cliout.Row(os.Stdout, "muted", "at", start.VerifyURL)
	fmt.Println()
	openBrowser(start.VerifyURL)

	approval, err := waitForApproval(cloudURL, start)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide login:", err)
		return 1
	}

	// What was approved, before anything touches the store: a wrong
	// organisation is visible while it is still only words on a screen.
	cliout.Infof("approved as %s in %s (%s)", caller, approval.Org, approval.Role)

	bundle, err := enrol(approval.EnrollURL, approval.Org, caFile, map[string]string{
		"assertion": approval.Assertion,
		"caller":    caller,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide login: %v\n", err)
		return 1
	}
	return storeAndReport(bundle)
}

func startDeviceLogin(cloudURL, caller, hostname string) (*deviceStart, error) {
	body, err := json.Marshal(map[string]string{"caller": caller, "hostname": hostname})
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(cloudURL+"/api/cli/start", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", cloudURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out struct {
		deviceStart
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("unreadable response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, fmt.Errorf("cloud refused: %s", out.Error)
		}
		return nil, fmt.Errorf("cloud refused with status %d", resp.StatusCode)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerifyURL == "" {
		return nil, fmt.Errorf("the response is missing the codes")
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out.deviceStart, nil
}

// waitForApproval polls until the person decides, the grant dies, or the
// budget runs out. The server's interval and slow_down answers set the pace.
func waitForApproval(cloudURL string, start *deviceStart) (*devicePoll, error) {
	deadline := time.Now().Add(devicePollBudget)
	interval := time.Duration(start.Interval) * time.Second

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		out, err := pollDeviceLogin(cloudURL, start.DeviceCode)
		if err != nil {
			return nil, err
		}
		switch out.Status {
		case "approved":
			return out, nil
		case "pending":
		case "slow_down":
			if out.RetryIn > 0 {
				interval = time.Duration(out.RetryIn) * time.Second
			} else {
				interval *= 2
			}
		case "denied":
			return nil, fmt.Errorf("the request was denied in the browser")
		case "expired":
			return nil, fmt.Errorf("the code expired before it was approved; run `tide login` again")
		default:
			return nil, fmt.Errorf("unexpected status %q", out.Status)
		}
	}
	return nil, fmt.Errorf("gave up waiting for approval; run `tide login` again")
}

func pollDeviceLogin(cloudURL, deviceCode string) (*devicePoll, error) {
	body, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(cloudURL+"/api/cli/poll", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", cloudURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var out devicePoll
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("unreadable response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, fmt.Errorf("cloud refused: %s", out.Error)
		}
		return nil, fmt.Errorf("cloud refused with status %d", resp.StatusCode)
	}
	return &out, nil
}

// openBrowser is best-effort. The URL is already printed, so a machine with
// no opener — SSH, a container — loses nothing.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
