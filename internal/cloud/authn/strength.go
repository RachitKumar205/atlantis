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
// classes. 3 means "safely unguessable: moderate protection from an offline
// slow-hash scenario", which is the situation this product is actually in — the
// hashes are argon2id in a database that could leak.
//
// A composition rule (one upper, one digit, one symbol) is the alternative and
// is worse on both axes: NIST 800-63B advises against it, and it accepts
// `Passw0rd!` while rejecting a long passphrase.
const MinScore = 3

// MinLength is a floor beneath which zxcvbn's estimate stops being the point.
//
// NIST puts the minimum at 8. This is higher because a short password that
// scores 3 is short and unusual rather than strong, and the whole reason to
// prefer an estimator is that it rewards length.
const MinLength = 12

// sha1Hex is the lookup key the range API is defined over: uppercase hex of the
// SHA-1 of the password.
//
// Its own function so the tests compute fixtures the same way the request does.
// A test that built the hash independently would pass while disagreeing with
// the implementation about case or encoding, and the failure would look like
// "the corpus does not have this password".
func sha1Hex(password string) string {
	sum := sha1.Sum([]byte(password)) //nolint:gosec // the range API is defined over SHA-1
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// ErrWeakPassword reports a password refused on strength.
var ErrWeakPassword = errors.New("password is too easy to guess")

// ErrBreachedPassword reports a password that appears in a known breach.
//
// Separate from ErrWeakPassword because the remedy the user is given differs:
// a weak password needs to be longer or less predictable, and a breached one
// may be neither — it may be excellent and simply already public.
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
		// The suggestion comes from zxcvbn where it has one, because "make it
		// stronger" is not actionable and is how users end up appending "1".
		return fmt.Errorf("%w: try a longer passphrase of unrelated words", ErrWeakPassword)
	}
	return nil
}

// BreachChecker asks whether a password is known to have leaked.
//
// An interface so the HTTP one can be swapped in tests and so the whole check
// can be turned off by configuration without a branch at every call site.
type BreachChecker interface {
	Breached(ctx context.Context, password string) (bool, error)
}

// NoBreachCheck answers "not breached" without asking anyone.
//
// What CLOUD_HIBP_CHECK=false selects. Deliberately a type rather than a nil
// check at the call site: a nil BreachChecker would be a panic waiting for the
// one path that forgot to guard it.
type NoBreachCheck struct{}

func (NoBreachCheck) Breached(context.Context, string) (bool, error) { return false, nil }

// HIBP checks against Have I Been Pwned's range API.
//
// # Why the password never leaves this process
//
// The API is k-anonymous: the client sends the first five hex characters of the
// SHA-1 of the password and receives every suffix sharing that prefix — some
// hundreds of hashes — then matches locally. The service learns a prefix shared
// by tens of thousands of passwords and nothing else.
//
// SHA-1 here is not a security choice and is not ours to make; it is the
// protocol the range API is defined over. It is a lookup key against a public
// corpus, not a password hash — the password hash is argon2id, next door.
type HIBP struct {
	Client *http.Client

	// BaseURL is the range endpoint, overridable so tests can serve their own
	// corpus rather than reaching the internet.
	BaseURL string
}

// NewHIBP returns a checker with sane timeouts.
//
// The timeout is short on purpose. This runs inside a sign-up request, and the
// caller treats a failure as "not breached" — so a slow third party must cost
// the user a second, not thirty.
func NewHIBP() *HIBP {
	return &HIBP{
		Client:  &http.Client{Timeout: 3 * time.Second},
		BaseURL: "https://api.pwnedpasswords.com/range/",
	}
}

// Breached reports whether password appears in the corpus.
//
// The error is returned rather than swallowed, and the CALLER decides to fail
// open — see the sign-up handler. That split is deliberate: this function's job
// is to answer or say why it cannot, and the policy of what an unanswerable
// check means belongs where the request is, not here.
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

	// Bounded: a prefix bucket is a few hundred lines, and padding raises that.
	// 4 MiB is far above any real response and far below a memory problem.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, fmt.Errorf("read the breach corpus response: %w", err)
	}

	for line := range strings.SplitSeq(string(body), "\n") {
		// Each line is SUFFIX:COUNT. A padded entry has count 0 and must not
		// be treated as a hit — that is the one way this check can produce a
		// false positive, and a false positive here refuses a good password.
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
