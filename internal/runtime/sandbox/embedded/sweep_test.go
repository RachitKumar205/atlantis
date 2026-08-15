package embedded

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The sweep must be able to tell a crashed predecessor's leftovers from
// another process's live working directory.
//
// It could not. The previous version globbed the two prefixes and removed every
// match, which meant the console deleted directories belonging to whatever else
// was running on the host. It surfaced when a console test called the real
// New(): `go test` runs packages concurrently, and the sweep destroyed three
// sandbox packages' directories mid-extraction, producing "rename ...: no such
// file or directory" two packages away from anything that had changed.
//
// TMPDIR is confined per test so these never see, or touch, a real sandbox.

// claimedDir creates a sandbox-shaped directory owned by the given pid.
func claimedDir(t *testing.T, prefix string, pid int) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), prefix+"deadbeef"+strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile),
		[]byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// deadPID returns the PID of a process that has exited.
//
// A real process rather than a large constant: a constant might be in use on
// the machine running the tests, and the test would then assert the opposite of
// what it means. Reuse of a just-exited PID within the next few milliseconds is
// possible in principle and would make this test pass vacuously rather than
// fail wrongly.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestSweepRemovesOnlyDirectoriesWhoseOwnerIsGone(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	live := claimedDir(t, "atlantis-sandbox-data-", os.Getpid())
	dead := claimedDir(t, "atlantis-sandbox-runtime-", deadPID(t))

	SweepAbandoned(func(string, ...any) {})

	if _, err := os.Stat(live); err != nil {
		t.Errorf("the sweep deleted a directory owned by a running process (%v). "+
			"That is a live sandbox vanishing under an active session, reported "+
			"wherever the session next touches the database.", err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("a directory whose owner has exited survived the sweep (err=%v); "+
			"the cleanup path does nothing and temp space grows without bound", err)
	}
}

// Anything the sweep cannot attribute is kept, and said out loud.
//
// Both cases are reachable: a directory created by a build that predates the
// owner file, and one whose marker was truncated by a crash mid-write. Deleting
// on "I cannot tell" is the behaviour being removed, so the test names the
// direction rather than just the outcome.
func TestSweepKeepsWhatItCannotAttribute(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	noMarker := filepath.Join(os.TempDir(), "atlantis-sandbox-data-nomarker")
	if err := os.MkdirAll(noMarker, 0o755); err != nil {
		t.Fatal(err)
	}
	badMarker := claimedDir(t, "atlantis-sandbox-runtime-", 4242)
	if err := os.WriteFile(filepath.Join(badMarker, ownerFile), []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged []string
	SweepAbandoned(func(format string, args ...any) {
		logged = append(logged, format)
	})

	for _, dir := range []string{noMarker, badMarker} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s was deleted despite the sweep being unable to say whose it "+
				"was: %v", filepath.Base(dir), err)
		}
	}
	if len(logged) < 2 {
		t.Errorf("kept %d unattributable directories but logged %d times; a directory "+
			"held forever with no mention of it is a disk leak nobody can find",
			2, len(logged))
	}
	for _, msg := range logged {
		if !strings.Contains(msg, "left in place") {
			t.Errorf("log line does not say the directory was kept: %q", msg)
		}
	}
}

// A directory this process creates carries its own claim.
//
// Without it the sweep has nothing to read, every directory is unattributable,
// and the cleanup path silently stops working — which is the failure the
// previous behaviour traded away.
func TestMakeTempDirClaimsTheDirectory(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir, err := makeTempDir("atlantis-sandbox-data-")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if err != nil {
		t.Fatalf("a freshly created sandbox directory has no owner file: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("owner file records %q, want this process's pid %d", got, os.Getpid())
	}

	// And the sweep must leave it alone while this process is alive.
	SweepAbandoned(func(string, ...any) {})
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the sweep removed a directory this very process had just claimed: %v", err)
	}
}
