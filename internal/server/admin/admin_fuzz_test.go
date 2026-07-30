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

// FuzzBindCallerIdentity hammers the one authorization decision that stayed in
// the handlers after capabilities moved to the interceptor.
//
// The invariant: when the cert CN is a real identity (non-empty,
// non-"anonymous") and req.Caller does not match it, the guard MUST return an
// error naming the mismatch. A bypass is direct privilege escalation — a
// caller holding CAPABILITY_SCHEMA_APPLY for its own namespace could push
// schema into someone else's. The interceptor cannot catch this: it sees the
// method and the connection, never the request body that names the target.
//
// The converse is deliberately weaker. A matching CN, or no CN at all, means
// this guard has nothing to say; whether the RPC proceeds is then the
// capability check's business, and that lives elsewhere.
func FuzzBindCallerIdentity(f *testing.F) {
	seeds := []struct {
		cn, reqCaller string
		proxied       bool
		mayApply      bool
	}{
		{"", "", false, false},
		{"backend", "backend", false, false},
		{"backend", "vendor", false, false},
		{"anonymous", "backend", false, false},
		{"", "backend", false, false},
		{"backend", "backend", true, true},
		{"backend", "backend", true, false},
		{"backend", "vendor", true, true},
		{strings.Repeat("a", 1000), strings.Repeat("a", 1000), false, false},
	}
	for _, s := range seeds {
		f.Add(s.cn, s.reqCaller, s.proxied, s.mayApply)
	}

	f.Fuzz(func(t *testing.T, cn, reqCaller string, proxied, mayApply bool) {
		s := &Service{
			callerFromContext:    func(context.Context) string { return cn },
			trustedProxyMayApply: mayApply,
		}
		if proxied {
			s.proxyForwarded = func(context.Context) bool { return true }
		}
		err := s.bindCallerIdentity(context.Background(), reqCaller)

		// A proxied request without the opt-in is refused before identity is
		// even considered, so the mismatch invariant does not apply to it.
		if proxied && !mayApply {
			if err == nil {
				t.Fatalf("proxied apply accepted with MAY_APPLY off (cn=%q reqCaller=%q)", cn, reqCaller)
			}
			return
		}

		hasIdentity := cn != "" && cn != "anonymous"
		if hasIdentity && reqCaller != cn {
			if err == nil {
				t.Fatalf("cn=%q reqCaller=%q mismatch was accepted", cn, reqCaller)
			}
			if !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("expected a 'does not match' error, got %v", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("cn=%q reqCaller=%q should bind cleanly, got %v", cn, reqCaller, err)
		}
	})
}

// FuzzGuardOperatorTransport pins the remaining half of the old operator gate:
// not who may operate — that is CAPABILITY_OPERATOR, checked by the
// interceptor — but whether an operator RPC may arrive over an edge-terminated
// connection at all.
//
// The invariant is a conjunction, and the failure that matters is it becoming
// a disjunction: a forwarded request is refused unless
// ATL_TRUSTED_PROXY_MAY_OPERATE is set, and the caller's identity is
// irrelevant to that decision. The apply flag must not stand in for it.
func FuzzGuardOperatorTransport(f *testing.F) {
	seeds := []struct {
		cn                   string
		proxied              bool
		mayApply, mayOperate bool
	}{
		{"", false, false, false},
		{"atlantis-console", false, false, false},
		{"atlantis-console", true, false, false},
		{"atlantis-console", true, true, false},
		{"atlantis-console", true, false, true},
		{"ci-backend", true, true, true},
	}
	for _, s := range seeds {
		f.Add(s.cn, s.proxied, s.mayApply, s.mayOperate)
	}

	f.Fuzz(func(t *testing.T, cn string, proxied, mayApply, mayOperate bool) {
		s := &Service{
			callerFromContext:      func(context.Context) string { return cn },
			trustedProxyMayApply:   mayApply,
			trustedProxyMayOperate: mayOperate,
		}
		if proxied {
			s.proxyForwarded = func(context.Context) bool { return true }
		}
		err := s.guardOperatorTransport(context.Background())

		if proxied && !mayOperate {
			if err == nil {
				t.Fatalf("cn=%q: forwarded operator RPC accepted with MAY_OPERATE off (may_apply=%v)", cn, mayApply)
			}
			return
		}
		if err != nil {
			t.Fatalf("cn=%q proxied=%v may_operate=%v: unexpected refusal %v", cn, proxied, mayOperate, err)
		}
	})
}
