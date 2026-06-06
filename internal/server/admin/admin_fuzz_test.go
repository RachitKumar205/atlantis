package admin

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzValidCallerName tests the regex-equivalent caller-name validator
// that gates RegisterCaller. The contract:
//
//  1. Never accept strings containing characters outside [a-z0-9-].
//     A leak here lets an operator register a caller name with control
//     characters / shell metas / null bytes / unicode lookalikes — any
//     of which would later flow into SQL identifiers, cert subjects, or
//     log lines.
//  2. Never accept names with edge-position hyphens or empty strings.
//  3. Never accept names longer than 64 chars.
//  4. Output is deterministic — same input → same boolean.
//
// Reserved-name checking is handled separately in RegisterCaller (after
// the name shape is validated); this fuzz only covers the syntactic
// gate.
func FuzzValidCallerName(f *testing.F) {
	seeds := []string{
		"",
		"a",
		"ab",
		"backend",
		"ci-backend",
		"a-b-c",
		"-leading",
		"trailing-",
		"two--hyphens",
		"UPPERCASE",
		"with space",
		"with.dot",
		"with/slash",
		"with;semi",
		"with\x00null",
		"with\nnewline",
		"with\ttab",
		"unicode-α",
		strings.Repeat("a", 64),
		strings.Repeat("a", 65),
		strings.Repeat("a", 1000),
		"a" + string(rune(0x200B)) + "b", // zero-width space
		"a" + string(rune(0x202E)) + "b", // RTL override
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		// (4) Determinism: call twice, same result.
		a := validCallerName(name)
		b := validCallerName(name)
		if a != b {
			t.Fatalf("non-deterministic for %q: %v vs %v", name, a, b)
		}
		if !a {
			return // rejection is fine
		}

		// (2,3) Length bounds.
		if len(name) == 0 {
			t.Fatalf("accepted empty string")
		}
		if len(name) > 64 {
			t.Fatalf("accepted name of length %d (> 64)", len(name))
		}

		// (1) Character set + edge-hyphen rule. Iterate bytes since the
		// allowed set is pure ASCII — any byte outside that set means a
		// multi-byte rune sneaked through.
		for i := 0; i < len(name); i++ {
			c := name[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= '0' && c <= '9':
			case c == '-':
				if i == 0 || i == len(name)-1 {
					t.Fatalf("accepted edge hyphen at %d in %q", i, name)
				}
			default:
				t.Fatalf("accepted byte 0x%02x (rune %U) at %d in %q", c, rune(c), i, name)
			}
		}

		// Sanity: the accepted name should also be valid UTF-8 (it had
		// better be, since it's all ASCII).
		if !utf8.ValidString(name) {
			t.Fatalf("accepted name is not valid UTF-8: %q", name)
		}
	})
}

// FuzzAuthorizeSelfApply hammers the per-CN authorization gate that
// stands between an authenticated caller and ApplyMigration /
// BeginBackfillPlan. Two safety invariants matter:
//
//  1. **Same-CN binding.** When the cert CN is set (non-empty,
//     non-"anonymous") AND req.Caller doesn't match it, authorize MUST
//     return an error containing "does not match". Bypass here is a
//     direct privilege-escalation — a backend cert could push schema as
//     vendor.
//  2. **Mutation gate.** When neither AllowApplyMutation nor the per-CN
//     allowlist grants permission, authorize MUST return an error.
//     Bypass here defeats the entire mutation-control surface.
//
// We construct a Service by hand (no pool needed; isRegisteredCaller
// short-circuits on nil pool) and vary the inputs.
func FuzzAuthorizeSelfApply(f *testing.F) {
	seeds := []struct {
		cn, reqCaller string
		wildcard      bool
		allowed       string // comma-separated
	}{
		{"", "", false, ""},
		{"backend", "backend", false, ""},
		{"backend", "vendor", true, ""},
		{"backend", "backend", true, ""},
		{"backend", "backend", false, "backend"},
		{"backend", "backend", false, "ci-backend"},
		{"anonymous", "backend", true, ""},
		{"", "backend", true, ""},
		{strings.Repeat("a", 1000), strings.Repeat("a", 1000), true, ""},
	}
	for _, s := range seeds {
		f.Add(s.cn, s.reqCaller, s.wildcard, s.allowed)
	}

	f.Fuzz(func(t *testing.T, cn, reqCaller string, wildcard bool, allowed string) {
		var allowedList []string
		if allowed != "" {
			allowedList = strings.Split(allowed, ",")
		}
		set := map[string]bool{}
		for _, c := range allowedList {
			if c = strings.TrimSpace(c); c != "" {
				set[c] = true
			}
		}
		s := &Service{
			allowApplyMutation: wildcard,
			mutationAllowed:    set,
			callerFromContext:  func(context.Context) string { return cn },
		}
		err := s.authorizeSelfApply(context.Background(), reqCaller)

		// (1) Same-CN binding: when a real CN is present, mismatch MUST
		// error regardless of allowlist contents.
		if cn != "" && cn != "anonymous" && reqCaller != cn {
			if err == nil {
				t.Fatalf("CN=%q reqCaller=%q mismatch was accepted (wildcard=%v allowed=%v)",
					cn, reqCaller, wildcard, set)
			}
			if !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("expected 'does not match' error, got %v", err)
			}
			return
		}

		// (2) Mutation gate: with no wildcard AND no allowlist entry
		// AND the (effective) CN absent or not on the list, error MUST
		// fire. When CN is empty/anonymous in dev mode and wildcard is
		// off, gate must reject.
		grants := wildcard || (cn != "" && cn != "anonymous" && set[cn])
		if !grants {
			if err == nil {
				t.Fatalf("CN=%q reqCaller=%q got nil error with no grant (wildcard=%v allowed=%v)",
					cn, reqCaller, wildcard, set)
			}
		}
	})
}

// FuzzAuthorizeOperator stresses the per-CN operator allowlist that
// gates every operator-mutating RPC (revoke, rollback, adopt, register).
// The safety contract: when operatorAllowed is non-empty ONLY those CNs
// proceed, regardless of the wildcard. Wildcard fallback applies only
// when the operator set is empty — adding ANY entry must disable the
// legacy global gate, otherwise tightening the policy weakens it.
func FuzzAuthorizeOperator(f *testing.F) {
	seeds := []struct {
		cn       string
		wildcard bool
		allowed  string
	}{
		{"", false, ""},
		{"", true, ""},
		{"atlantis-console", false, "atlantis-console"},
		{"ci-backend", false, "atlantis-console"},
		{"atlantis-console", false, ""},
		{"atlantis-console", true, ""},
		{"atlantis-console", true, "ci-backend"},
	}
	for _, s := range seeds {
		f.Add(s.cn, s.wildcard, s.allowed)
	}

	f.Fuzz(func(t *testing.T, cn string, wildcard bool, allowed string) {
		set := map[string]bool{}
		if allowed != "" {
			for _, c := range strings.Split(allowed, ",") {
				if c = strings.TrimSpace(c); c != "" {
					set[c] = true
				}
			}
		}
		s := &Service{
			allowApplyMutation: wildcard,
			operatorAllowed:    set,
			callerFromContext:  func(context.Context) string { return cn },
		}
		err := s.authorizeOperator(context.Background())

		// Contract: if operatorAllowed is non-empty AND CN is not on
		// the list, error MUST fire regardless of wildcard. (The
		// wildcard fallback only applies when operatorAllowed is
		// empty — defined deliberately so adding ANY operator entry
		// disables the legacy global gate.)
		if len(set) > 0 {
			if set[cn] {
				if err != nil {
					t.Fatalf("CN=%q on allowlist was rejected: %v", cn, err)
				}
			} else {
				if err == nil {
					t.Fatalf("CN=%q not on allowlist was accepted (allowed=%v)", cn, set)
				}
			}
			return
		}

		// operatorAllowed empty: fall back to wildcard.
		if wildcard {
			if err != nil {
				t.Fatalf("wildcard fallback should grant, got %v", err)
			}
		} else {
			if err == nil {
				t.Fatalf("empty operatorAllowed + no wildcard must reject, got nil")
			}
		}
	})
}
