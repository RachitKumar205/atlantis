package provision

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The organisation's data keyset seals the managed database's DSN.
//
// The property that matters is that it survives: a keyset minted a second time
// cannot decrypt what the first one sealed, and the organisation would keep
// running against a connection string nothing can read. The only symptom would
// be plans against the wrong database, so this is asserted rather than trusted
// to the apply being idempotent.

func TestDataKeyIsMintedOnce(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()

	first, err := k.ensureDataKey(ctx, "org-acme", "acme")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first == "" {
		t.Fatal("minted an empty keyset")
	}

	second, err := k.ensureDataKey(ctx, "org-acme", "acme")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second != first {
		t.Error("the keyset was regenerated. Everything the previous one sealed " +
			"is unreadable, and the managed database would have to be named again")
	}
}

// Two organisations do not share a keyset: one organisation's key must not
// open another's connection string.
func TestDataKeyIsPerOrganisation(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()

	acme, err := k.ensureDataKey(ctx, "org-acme", "acme")
	if err != nil {
		t.Fatalf("acme: %v", err)
	}
	globex, err := k.ensureDataKey(ctx, "org-globex", "globex")
	if err != nil {
		t.Fatalf("globex: %v", err)
	}
	if acme == globex {
		t.Error("both organisations hold the same keyset, so either can unseal " +
			"the other's managed-database connection string")
	}
}

// An empty stored keyset is refused rather than replaced, for the reason
// ensureCerts refuses a broken authority: replacing it silently discards what
// it sealed.
func TestAnEmptyStoredKeysetIsRefused(t *testing.T) {
	k := newTestKube(t, &corev1.Secret{
		TypeMeta: typeMeta("v1", "Secret"),
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretDataKey,
			Namespace: "org-acme",
		},
		Data: map[string][]byte{"keyset": []byte("")},
	})

	_, err := k.ensureDataKey(context.Background(), "org-acme", "acme")
	if err == nil {
		t.Fatal("an empty keyset was accepted, so a fresh one would be minted " +
			"over whatever the old one sealed")
	}
	// The message has to say what to delete: an operator who cannot act on it
	// will delete the namespace instead.
	for _, want := range []string{secretDataKey, "org-acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// The atlantis Deployment reads it from the Secret rather than carrying the
// value, so the keyset is not in the pod spec anything can read.
func TestAtlantisMountsTheDataKeyByReference(t *testing.T) {
	k := newTestKube(t)
	var found *corev1.EnvVar
	for _, e := range k.atlantisEnv() {
		if e.Name == "ATL_DATA_KEY" {
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatal("the atlantis Deployment does not carry ATL_DATA_KEY; " +
			"SetManagedDatabase refuses with 'no keyring is configured'")
	}
	if found.Value != "" {
		t.Error("the keyset is inline in the pod spec")
	}
	if found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
		t.Fatal("ATL_DATA_KEY does not come from a Secret")
	}
	if got := found.ValueFrom.SecretKeyRef.Name; got != secretDataKey {
		t.Errorf("reads secret %q, want %q", got, secretDataKey)
	}
}
