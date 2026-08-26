package authn

import (
	"context"
	"crypto/sha1" //nolint:gosec // HIBP's range API is defined over SHA-1; see Breached.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/trustelem/zxcvbn"
)

// MinScore is the zxcvbn score a password must reach.
//
// zxcvbn scores 0–4 by estimating guesses rather than counting character
// classes. 3 is "safely unguessable: moderate protection from an offline
// slow-hash scenario", which is what argon2id hashes in a leakable database are.
//
// NIST 800-63B advises against composition rules, which accept `Passw0rd!` and
// reject a long passphrase.
const MinScore = 3

// MinLength is a floor beneath which zxcvbn's estimate stops being the point.
//
// NIST puts the minimum at 8. A short password scoring 3 is unusual rather than
// strong.
const MinLength = 12

// sha1Hex is the lookup key the range API is defined over: uppercase hex of the
// SHA-1 of the password.
//
// Its own function so tests compute fixtures the same way the request does. A
// second implementation could disagree on case or encoding and still pass.
func sha1Hex(password string) string {
	sum := sha1.Sum([]byte(password)) //nolint:gosec // the range API is defined over SHA-1
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// ErrWeakPassword reports a password refused on strength.
var ErrWeakPassword = errors.New("password is too easy to guess")

// ErrBreachedPassword reports a password that appears in a known breach.
//
// Separate from ErrWeakPassword because the remedy differs: a breached password
// may be strong and simply already public.
var ErrBreachedPassword = errors.New("password has appeared in a data breach")

// Strength checks a password locally.
//
// userInputs are values an attacker would try first for this particular person
// — their email, their name, the organisation. zxcvbn penalises a password
// built from them, which is the case a generic dictionary check misses.
func Strength(password string, userInputs ...string) error {
	if len(password) < MinLength {
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinLength)
	}
	// zxcvbn's own guidance is that very long inputs cost time to score without
	// changing the answer; anything this long is unguessable regardless.
	if len(password) > 128 {
		return nil
	}

	result := zxcvbn.PasswordStrength(password, userInputs)
	if result.Score < MinScore {
		// A concrete suggestion rather than "make it stronger".
		return fmt.Errorf("%w: try a longer passphrase of unrelated words", ErrWeakPassword)
	}
	return nil
}

// BreachChecker asks whether a password is known to have leaked.
//
// An interface, so tests swap the HTTP one out and configuration turns the
// check off without a branch at every call site.
type BreachChecker interface {
	Breached(ctx context.Context, password string) (bool, error)
}

// NoBreachCheck answers "not breached" without asking anyone.
//
// What CLOUD_HIBP_CHECK=false selects. A type rather than a nil BreachChecker,
// which would panic on any path that forgot to guard it.
type NoBreachCheck struct{}

func (NoBreachCheck) Breached(context.Context, string) (bool, error) { return false, nil }

// HIBP checks against Have I Been Pwned's range API.
//
// The password never leaves this process. The API is k-anonymous: it takes the
// first five hex characters of the password's SHA-1 and returns every suffix
// sharing that prefix, some hundreds of hashes, which are matched locally.
//
// SHA-1 is the protocol the range API is defined over, a lookup key against a
// public corpus. The password hash is argon2id, next door.
type HIBP struct {
	Client *http.Client

	// BaseURL is the range endpoint, overridable so tests can serve their own
	// corpus rather than reaching the internet.
	BaseURL string
}

// NewHIBP returns a checker with sane timeouts.
//
// The timeout is short: this runs inside a sign-up request, and the caller
// treats a failure as "not breached".
func NewHIBP() *HIBP {
	return &HIBP{
		Client:  &http.Client{Timeout: 3 * time.Second},
		BaseURL: "https://api.pwnedpasswords.com/range/",
	}
}

// Breached reports whether password appears in the corpus.
//
// It answers or reports why it cannot. Whether an unanswerable check accepts
// the password is the caller's policy; see the sign-up handler.
func (h *HIBP) Breached(ctx context.Context, password string) (bool, error) {
	full := sha1Hex(password)
	prefix, suffix := full[:5], full[5:]

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+prefix, nil)
	if err != nil {
		return false, err
	}
	// Documented by the service: pads the response with random hashes so its
	// length does not narrow down which prefix was asked for.
	req.Header.Set("Add-Padding", "true")

	resp, err := h.Client.Do(req)
	if err != nil {
		return false, fmt.Errorf("reach the breach corpus: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("breach corpus returned %d", resp.StatusCode)
	}

	// Bounded: a prefix bucket is a few hundred lines, plus padding.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, fmt.Errorf("read the breach corpus response: %w", err)
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		// Each line is SUFFIX:COUNT. A padded entry has count 0, and counting
		// it as a hit refuses a good password.
		hash, count, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if strings.EqualFold(hash, suffix) && strings.TrimSpace(count) != "0" {
			return true, nil
		}
	}
	return false, nil
}
