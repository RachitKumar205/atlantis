package issuer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKeySurvivesAReload is what stops a restart from signing everyone out.
//
// The kid is derived from the key rather than stored, so the two could
// disagree without anything failing loudly: the issuer would publish the same
// key under a new name, and every assertion already in flight would reference
// a kid the key set no longer lists. Consoles would report invalid tokens.
func TestKeySurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key.pem")

	first, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if !created {
		t.Error("reported an existing key for a path that did not exist")
	}

	second, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (reload): %v", err)
	}
	if created {
		t.Error("generated a second key over one that already existed")
	}
	if second.ID != first.ID {
		t.Errorf("kid changed across a reload: %q then %q", first.ID, second.ID)
	}

	// And it is the same key, not merely one with a matching name.
	if !second.priv.Equal(first.priv) {
		t.Error("the reloaded key is a different key with the same kid")
	}
}

// TestKeyIsWrittenPrivately: anyone who can read this file can mint an
// assertion for any user in any organisation, and it would verify perfectly at
// every console.
func TestKeyIsWrittenPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "signing-key.pem")

	if _, _, err := LoadOrCreateKey(path); err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %#o, want 0600", perm)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("key directory mode = %#o, want 0700", perm)
	}
}

func TestLoadKeyRejectsRubbish(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"not PEM", "this is not a key"},
		{"PEM but not a key", "-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "signing-key.pem")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, _, err := LoadOrCreateKey(path); err == nil {
				t.Fatal("accepted a file that is not a signing key")
			}
		})
	}
}

// TestKeyFileIsNotOverwritten: losing a signing key by writing over it would
// invalidate every assertion in flight, and the file it replaced is gone.
func TestKeyFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	original, _, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if _, _, err := LoadOrCreateKey(path); err != nil {
		t.Fatalf("second load: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the key file was rewritten; kid was %s", original.ID)
	}
}
