package certs

import (
	"bytes"
	"crypto/x509"
	"testing"
	"time"
)

// Reissuing the console's credentials.
//
// A rotation that mints a new authority along with the leaves is silent: both
// certificates still work, the console still connects, and every caller
// certificate in the organisation stops authenticating at once with nothing
// pointing back at this function.
//
// The authority assertions below are byte comparisons rather than "still
// verifies", because a regenerated root verifies its own fresh leaves.

// Without this, a no-op implementation satisfies every other test in this file,
// and a rotation after a suspected leak leaves the leaked certificate in place.
func TestReissuingReplacesBothConsoleLeaves(t *testing.T) {
	b := generate(t, "acme")
	oldConsole := append([]byte(nil), b.Console.CertPEM...)
	oldConsoleKey := append([]byte(nil), b.Console.KeyPEM...)
	oldSignerClient := append([]byte(nil), b.SignerClient.CertPEM...)
	oldSignerKey := append([]byte(nil), b.SignerClient.KeyPEM...)

	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	if bytes.Equal(oldConsole, b.Console.CertPEM) {
		t.Error("the console certificate is unchanged, so a rotation after a leak " +
			"would leave the leaked credential working")
	}
	if bytes.Equal(oldSignerClient, b.SignerClient.CertPEM) {
		t.Error("the signer-client certificate is unchanged")
	}
	// The private keys too. Reissuing the certificate over the same key would
	// leave anyone holding that key able to keep using it.
	if bytes.Equal(oldConsoleKey, b.Console.KeyPEM) {
		t.Error("the console private key is unchanged, so the old key still works")
	}
	if bytes.Equal(oldSignerKey, b.SignerClient.KeyPEM) {
		t.Error("the signer-client private key is unchanged")
	}
}

// Neither authority is touched, byte for byte. ensureCerts refuses to
// regenerate one for the same reason: a fresh root orphans every caller
// certificate issued under the old one, and the organisation finds out when its
// callers stop authenticating rather than when this function runs.
func TestReissuingLeavesBothAuthoritiesUntouched(t *testing.T) {
	b := generate(t, "acme")
	caCert := append([]byte(nil), b.CA.CertPEM...)
	caKey := append([]byte(nil), b.CA.KeyPEM...)
	signerCACert := append([]byte(nil), b.SignerCA.CertPEM...)
	signerCAKey := append([]byte(nil), b.SignerCA.KeyPEM...)

	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	if !bytes.Equal(caCert, b.CA.CertPEM) || !bytes.Equal(caKey, b.CA.KeyPEM) {
		t.Error("the issuing CA changed; every caller certificate in this " +
			"organisation has just been orphaned")
	}
	if !bytes.Equal(signerCACert, b.SignerCA.CertPEM) || !bytes.Equal(signerCAKey, b.SignerCA.KeyPEM) {
		t.Error("the signer client CA changed")
	}
}

// The same property as above, from the organisation's side: a caller
// certificate issued before the rotation still verifies after it.
func TestACallerCertificateSurvivesAConsoleRotation(t *testing.T) {
	b := generate(t, "acme")

	// Stand in for a caller the signer issued: any client leaf under the
	// organisation's issuing CA. b.Console is one, minted before the rotation.
	callerBefore := append([]byte(nil), b.Console.CertPEM...)
	if !verifies(t, callerBefore, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Fatal("the pre-rotation certificate does not verify to begin with")
	}

	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	if !verifies(t, callerBefore, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("a certificate issued before the rotation no longer verifies against " +
			"the organisation's CA — the rotation replaced the authority")
	}
}

// certs_test.go asserts this for Generate; it has to hold for the rotation path
// too. Signing the signer-client leaf with the issuing CA collapses the two
// trust domains into one, and every leaf still looks valid.
func TestReissuedLeavesKeepTheirSeparateRoots(t *testing.T) {
	b := generate(t, "acme")
	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	if !verifies(t, b.Console.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the reissued console leaf does not chain to the issuing CA")
	}
	if !verifies(t, b.SignerClient.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the reissued signer-client leaf does not chain to the signer CA")
	}
	// And the crossings must not build.
	if verifies(t, b.Console.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the reissued console leaf verifies against the signer CA; the two " +
			"authorities are no longer separate")
	}
	if verifies(t, b.SignerClient.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the reissued signer-client leaf verifies against the issuing CA")
	}
}

// The server leaves are mounted by running pods, so replacing them here would
// make a rotation require a restart.
func TestReissuingDoesNotTouchTheServerLeaves(t *testing.T) {
	b := generate(t, "acme")
	server := append([]byte(nil), b.Server.CertPEM...)
	signerServer := append([]byte(nil), b.SignerServer.CertPEM...)

	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	if !bytes.Equal(server, b.Server.CertPEM) {
		t.Error("the atlantis server leaf changed, so the rotation now needs a pod restart")
	}
	if !bytes.Equal(signerServer, b.SignerServer.CertPEM) {
		t.Error("the signer server leaf changed")
	}
}

// ConsoleCN is what the server's allowlist admits and what
// SIGNER_ALLOWED_CLIENT_CNS matches. A rotation producing a correct certificate
// under a different name is refused everywhere, and the error names a
// certificate rather than this.
func TestReissuedLeavesCarryTheLoadBearingNames(t *testing.T) {
	b := generate(t, "acme")
	if err := ReissueConsoleLeaves(b, time.Time{}); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	for _, c := range []struct {
		what string
		pem  []byte
		want string
	}{
		{"console", b.Console.CertPEM, ConsoleCN},
		{"signer client", b.SignerClient.CertPEM, SignerClientCN},
	} {
		leaf := parseLeaf(t, c.pem)
		if leaf.Subject.CommonName != c.want {
			t.Errorf("the reissued %s leaf is named %q, want %q",
				c.what, leaf.Subject.CommonName, c.want)
		}
		var client bool
		for _, u := range leaf.ExtKeyUsage {
			if u == x509.ExtKeyUsageClientAuth {
				client = true
			}
		}
		if !client {
			t.Errorf("the reissued %s leaf is not marked for client authentication", c.what)
		}
	}
}

// The lifetime restarts from the rotation.
func TestReissuedLeavesExpireRelativeToTheRotation(t *testing.T) {
	b := generate(t, "acme")
	// A year on, so the new expiry cannot be confused with the original one.
	at := time.Now().Add(365 * 24 * time.Hour)
	if err := ReissueConsoleLeaves(b, at); err != nil {
		t.Fatalf("ReissueConsoleLeaves: %v", err)
	}

	leaf := parseLeaf(t, b.Console.CertPEM)
	want := at.Add(ClientLifetime)
	if leaf.NotAfter.Sub(want).Abs() > time.Minute {
		t.Errorf("the reissued leaf expires at %s, want about %s — the lifetime did "+
			"not restart from the rotation", leaf.NotAfter, want)
	}
	// Backdated, like Generate, because whatever picks this up is on another
	// machine whose clock is not this one to the second.
	if !leaf.NotBefore.Before(at) {
		t.Errorf("the reissued leaf is valid from %s, which is not before %s",
			leaf.NotBefore, at)
	}
}

// Refused before anything is replaced. Half a rotation leaves a console with a
// fresh certificate for atlantis and a stale one for the signer, which works
// until the next enrolment and then fails somewhere unrelated to this.
func TestReissuingRefusesAnUnusableBundle(t *testing.T) {
	t.Run("a missing authority", func(t *testing.T) {
		b := generate(t, "acme")
		b.SignerCA.KeyPEM = nil
		before := append([]byte(nil), b.Console.CertPEM...)

		if err := ReissueConsoleLeaves(b, time.Time{}); err == nil {
			t.Fatal("reissued from a bundle with no signer CA key")
		}
		if !bytes.Equal(before, b.Console.CertPEM) {
			t.Error("the console leaf was replaced even though the reissue failed; " +
				"the organisation is now half-rotated")
		}
	})

	t.Run("a leaf where an authority should be", func(t *testing.T) {
		b := generate(t, "acme")
		// The server leaf is a valid certificate and key pair, and cannot sign.
		b.CA.CertPEM = b.Server.CertPEM
		b.CA.KeyPEM = b.Server.KeyPEM

		err := ReissueConsoleLeaves(b, time.Time{})
		if err == nil {
			t.Fatal("reissued using a leaf as the issuing authority")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("not a certificate authority")) {
			t.Errorf("the error does not say the material is not an authority: %v", err)
		}
	})

	t.Run("no bundle at all", func(t *testing.T) {
		if err := ReissueConsoleLeaves(nil, time.Time{}); err == nil {
			t.Fatal("reissued from a nil bundle")
		}
	})
}
