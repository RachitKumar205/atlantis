package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// assertionWith builds an unsigned JWT carrying claims. actorFromAssertion
// reads only the payload, so the header and signature are placeholders.
func assertionWith(t *testing.T, claims map[string]string) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestActorFromAssertion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		assertion string
		want      actorIdentity
	}{
		{
			name: "a signed-in account",
			assertion: assertionWith(t, map[string]string{
				"sub": "usr_42", "email": "ada@example.com", "name": "Ada Lovelace",
			}),
			want: actorIdentity{ID: "console:usr_42", Email: "ada@example.com", Name: "Ada Lovelace"},
		},
		{
			name:      "no subject",
			assertion: assertionWith(t, map[string]string{"email": "ada@example.com"}),
		},
		{name: "two segments", assertion: "e30.e30"},
		{name: "a payload that is not base64", assertion: "e30.!!!.c2ln"},
		{name: "a payload that is not JSON", assertion: "e30." + base64.RawURLEncoding.EncodeToString([]byte("[")) + ".c2ln"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actorFromAssertion(tc.assertion); got != tc.want {
				t.Errorf("actorFromAssertion = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestActorEnvironmentOverridesTheStore(t *testing.T) {
	c := &tideConfig{storeActor: actorIdentity{ID: "console:usr_42", Email: "ada@example.com"}}

	t.Setenv("ATL_ACTOR", "")
	if got := c.actor(); got != c.storeActor {
		t.Errorf("with ATL_ACTOR unset, actor = %+v, want the store's %+v", got, c.storeActor)
	}

	t.Setenv("ATL_ACTOR", " ci:deploy-bot ")
	t.Setenv("ATL_ACTOR_EMAIL", "bot@example.com")
	t.Setenv("ATL_ACTOR_NAME", "Deploy Bot")
	want := actorIdentity{ID: "ci:deploy-bot", Email: "bot@example.com", Name: "Deploy Bot"}
	if got := c.actor(); got != want {
		t.Errorf("with ATL_ACTOR set, actor = %+v, want %+v", got, want)
	}
}

// TestTheStoreKeepsTheLoginsActor covers the three writers of actor.json: a
// browser login writes it, renewal leaves it, and a later login that names no
// person removes it.
func TestTheStoreKeepsTheLoginsActor(t *testing.T) {
	storeFixture(t)
	ada := actorIdentity{ID: "console:usr_42", Email: "ada@example.com", Name: "Ada Lovelace"}

	bundle := fakeCredentials(t, "acme", "billing")
	bundle.Actor = ada
	if err := writeNewCredentials(bundle); err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials("acme", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != ada {
		t.Fatalf("after login, stored actor = %+v, want %+v", got.Actor, ada)
	}

	if err := writeSidecars(got.Dir, &storedCredentials{
		CAPEM: got.CAPEM, Endpoint: got.Endpoint, EnrollURL: got.EnrollURL,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err = loadCredentials("acme", "billing"); err != nil {
		t.Fatal(err)
	}
	if got.Actor != ada {
		t.Fatalf("after renewal, stored actor = %+v, want %+v", got.Actor, ada)
	}

	if err := os.Remove(filepath.Join(got.Dir, clientPEMName)); err != nil {
		t.Fatal(err)
	}
	if err := writeNewCredentials(fakeCredentials(t, "acme", "billing")); err != nil {
		t.Fatal(err)
	}
	if got, err = loadCredentials("acme", "billing"); err != nil {
		t.Fatal(err)
	}
	if got.Actor != (actorIdentity{}) {
		t.Errorf("after a token login, stored actor = %+v, want none", got.Actor)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, actorName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s survived a login that named no person: %v", actorName, err)
	}
}
