package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The estimator, not a composition rule.
//
// The two cases that matter are the ones a character-class rule gets backwards:
// a long passphrase of ordinary words is strong, and `Passw0rd!` satisfies every
// composition rule ever written while being among the first thousand guesses.
func TestStrength(t *testing.T) {
	accepted := []string{
		"correct horse battery staple",
		"unrelated-marmalade-turbine-9",
		"the quick brown fox jumps over",
	}
	for _, pw := range accepted {
		if err := Strength(pw); err != nil {
			t.Errorf("refused a strong password %q: %v", pw, err)
		}
	}

	refused := map[string]string{
		"short":                     "aB3!x",
		"a composition-rule winner": "Passw0rd!",
		"common and long enough":    "qwertyuiop123",
		"repetition":                "abcabcabcabcabc",
	}
	for name, pw := range refused {
		t.Run(name, func(t *testing.T) {
			err := Strength(pw)
			if err == nil {
				t.Fatalf("accepted %q", pw)
			}
			if !errors.Is(err, ErrWeakPassword) {
				t.Errorf("got %v, want ErrWeakPassword", err)
			}
		})
	}
}

// A password built from the user's own details is refused even when it looks
// unusual, which is the case a dictionary check misses.
func TestStrengthPenalisesTheUsersOwnDetails(t *testing.T) {
	const pw = "ada.lovelace@example.com"

	// Without context it is long and mixed enough to pass.
	if err := Strength(pw); err != nil {
		t.Skipf("this password is refused even without context, so the test "+
			"cannot show the difference context makes: %v", err)
	}

	// With the address as context, zxcvbn knows it is the first thing an
	// attacker targeting this person would try.
	if err := Strength(pw, "ada.lovelace@example.com", "Ada Lovelace"); err == nil {
		t.Error("accepted a password that is the user's own email address")
	}
}

// The length floor is enforced before the estimator runs.
//
// A short password that happens to score well is short and unusual rather than
// strong, and the reason to use an estimator at all is that it rewards length.
func TestStrengthHasALengthFloor(t *testing.T) {
	err := Strength("x9$Kq2")
	if !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("got %v, want ErrWeakPassword", err)
	}
	if !strings.Contains(err.Error(), "characters") {
		t.Errorf("the error does not say what the requirement is: %v", err)
	}
}

// The breach check sends a prefix, never the password or its full hash.
func TestHIBPSendsOnlyAPrefix(t *testing.T) {
	const pw = "correct horse battery staple"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// A response that does not contain the suffix: not breached.
		_, _ = w.Write([]byte("0000000000000000000000000000000000A:3\r\n"))
	}))
	defer srv.Close()

	h := &HIBP{Client: srv.Client(), BaseURL: srv.URL + "/"}
	breached, err := h.Breached(context.Background(), pw)
	if err != nil {
		t.Fatalf("Breached: %v", err)
	}
	if breached {
		t.Error("reported a breach for a hash the corpus did not contain")
	}

	prefix := strings.TrimPrefix(gotPath, "/")
	if len(prefix) != 5 {
		t.Fatalf("sent %q, want a five-character prefix", prefix)
	}
	// The whole point of the range API: the service must not learn the
	// password or enough of its hash to identify it.
	if strings.Contains(gotPath, pw) {
		t.Fatal("the password itself was sent to the breach service")
	}
}

// A hash the corpus holds is reported as breached.
func TestHIBPReportsAKnownPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	suffix := hibpSuffix(pw)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:1\r\n" + suffix + ":42\r\n"))
	}))
	defer srv.Close()

	h := &HIBP{Client: srv.Client(), BaseURL: srv.URL + "/"}
	breached, err := h.Breached(context.Background(), pw)
	if err != nil {
		t.Fatalf("Breached: %v", err)
	}
	if !breached {
		t.Fatal("a password present in the corpus was reported as clean")
	}
}

// A padded entry is not a hit.
//
// The service pads responses with random hashes at count 0 so their length does
// not narrow down the prefix. Counting one as a match is the only way this
// check produces a false positive — and a false positive here refuses a good
// password with an alarming message.
func TestHIBPIgnoresPadding(t *testing.T) {
	const pw = "correct horse battery staple"
	suffix := hibpSuffix(pw)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(suffix + ":0\r\n"))
	}))
	defer srv.Close()

	h := &HIBP{Client: srv.Client(), BaseURL: srv.URL + "/"}
	breached, err := h.Breached(context.Background(), pw)
	if err != nil {
		t.Fatalf("Breached: %v", err)
	}
	if breached {
		t.Fatal("a zero-count padding entry was treated as a breach, which " +
			"refuses a password that is not in the corpus at all")
	}
}

// An unreachable service is an error, not a silent "clean".
//
// The caller fails open — that is a policy decision made where the request is.
// What must not happen is this function deciding it for them, because then the
// counter that says how often the check is skipped can never exist.
func TestHIBPReportsAFailureRatherThanGuessing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := &HIBP{Client: srv.Client(), BaseURL: srv.URL + "/"}
	breached, err := h.Breached(context.Background(), "anything at all here")
	if err == nil {
		t.Fatal("an unreachable corpus was reported as a clean answer, so a " +
			"caller cannot tell 'not breached' from 'not checked'")
	}
	if breached {
		t.Error("reported breached on an error")
	}
}

// The disabled checker answers without asking anyone.
func TestNoBreachCheck(t *testing.T) {
	breached, err := NoBreachCheck{}.Breached(context.Background(), "hunter2")
	if err != nil || breached {
		t.Errorf("got (%v, %v), want (false, nil)", breached, err)
	}
}

// hibpSuffix is the part of the SHA-1 the range API returns, computed the same
// way Breached does so the fixtures cannot drift from the implementation.
func hibpSuffix(password string) string { return sha1Hex(password)[5:] }
