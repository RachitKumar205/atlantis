package cloudauth

import (
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

// TestSelectKey covers the filters directly.
//
// A behavioural test cannot reach all of them. Refusing to verify an RS256
// signature with a key published for ES256 has no exploit to demonstrate,
// because the key types differ and go-jose would refuse on that ground
// first — so an end-to-end test would pass whether the filter were here or
// not. The filter still belongs: `alg` and `use` are the issuer stating what a
// key is for, and honouring that is not conditional on there being a known way
// to abuse ignoring it today.
func TestSelectKey(t *testing.T) {
	ec := newTestKey(t)

	keyWith := func(kid, alg, use string) jose.JSONWebKey {
		return jose.JSONWebKey{Key: ec.priv.Public(), KeyID: kid, Algorithm: alg, Use: use}
	}
	setOf := func(keys ...jose.JSONWebKey) *jose.JSONWebKeySet {
		return &jose.JSONWebKeySet{Keys: keys}
	}

	for _, tc := range []struct {
		name string
		set  *jose.JSONWebKeySet
		kid  string
		alg  string
		want bool
	}{
		{"no key set at all", nil, "k1", "ES256", false},
		{"empty key set", setOf(), "k1", "ES256", false},
		{"kid matches, algorithm agrees", setOf(keyWith("k1", "ES256", "sig")), "k1", "ES256", true},
		{"kid does not match", setOf(keyWith("k1", "ES256", "sig")), "k2", "ES256", false},

		// The issuer said this key is for ES256; a token asking to verify an
		// RS256 signature with it is asking for something the key set does not
		// offer.
		{"algorithm disagrees", setOf(keyWith("k1", "ES256", "sig")), "k1", "RS256", false},

		// alg is optional in RFC 7517 and plenty of issuers omit it, so an
		// undeclared key must stay usable — otherwise this console silently
		// stops working against a conformant issuer.
		{"no algorithm declared", setOf(keyWith("k1", "", "sig")), "k1", "ES256", true},

		{"use is sig", setOf(keyWith("k1", "ES256", "sig")), "k1", "ES256", true},
		{"use is enc", setOf(keyWith("k1", "ES256", "enc")), "k1", "ES256", false},
		{"no use declared", setOf(keyWith("k1", "ES256", "")), "k1", "ES256", true},

		// Documents why Verify refuses a token with no kid before it ever
		// gets here: a key published without one is a genuine match for the
		// empty lookup.
		{"empty kid matches a key published without one", setOf(keyWith("", "ES256", "sig")), "", "ES256", true},

		// Later keys are still considered, so one unusable entry does not
		// hide a usable one behind it.
		{"second key matches", setOf(keyWith("k1", "ES256", "enc"), keyWith("k1", "ES256", "sig")), "k1", "ES256", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectKey(tc.set, tc.kid, tc.alg)
			if ok != tc.want {
				t.Fatalf("selectKey(%q, %q) found = %v, want %v", tc.kid, tc.alg, ok, tc.want)
			}
			if ok && got.KeyID != tc.kid {
				t.Errorf("returned key %q, want %q", got.KeyID, tc.kid)
			}
		})
	}
}
