package authn

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters.
//
// Six digits over thirty seconds with SHA-1 is what authenticator apps
// implement; anything else computes different codes in Google Authenticator and
// its imitators.
//
// Skew of one step either side absorbs clock drift, and triples the window a
// captured code is valid for, which is why SpendTOTPStep records the step.
const (
	totpPeriod = 30
	totpSkew   = 1
	totpDigits = otp.DigitsSix
	totpAlgo   = otp.AlgorithmSHA1
)

// ErrBadCode reports a code that does not match.
var ErrBadCode = errors.New("that code is not right")

// NewTOTPSecret mints a secret and the otpauth:// URI that enrols it.
//
// issuer is the account's label in the authenticator app, so it is the product
// name rather than a hostname.
func NewTOTPSecret(issuer, account string) (secret, uri string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgo,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate TOTP secret: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// VerifyTOTP checks a code and reports which time step it came from.
//
// It returns the step, not just a boolean. A TOTP code is valid for its whole
// period and for the periods either side once skew is allowed, ninety seconds
// in total here, so the same code works repeatedly within that window. The
// caller records the step to refuse a second use.
//
// The comparison is constant-time; the code comes from the request.
func VerifyTOTP(secret, code string, now time.Time) (int64, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, ErrBadCode
	}

	opts := totp.ValidateOpts{
		Period:    totpPeriod,
		Skew:      0, // stepped through below, one at a time
		Digits:    totpDigits,
		Algorithm: totpAlgo,
	}

	// Every candidate step is checked even after one matches, so the time taken
	// does not reveal which step was right.
	matched := int64(-1)
	for skew := -totpSkew; skew <= totpSkew; skew++ {
		at := now.Add(time.Duration(skew) * totpPeriod * time.Second)
		want, err := totp.GenerateCodeCustom(secret, at, opts)
		if err != nil {
			return 0, fmt.Errorf("compute expected code: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			matched = at.Unix() / totpPeriod
		}
	}
	if matched < 0 {
		return 0, ErrBadCode
	}
	return matched, nil
}

// TOTPStep is the step number a moment falls in, for callers that need to
// record one without verifying a code.
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// CodeAt computes the code an authenticator would show at a moment.
//
// The exact inverse of VerifyTOTP, sharing its parameters. Exported so tests
// can produce a code the same way the server checks one — a fixture that
// computed codes independently would disagree with the implementation about
// period or digits and fail in a way that reads as "the code is wrong" rather
// than "the two disagree".
func CodeAt(secret string, at time.Time) (string, error) {
	return totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    totpDigits,
		Algorithm: totpAlgo,
	})
}
