package certs

import (
	"bytes"
	"crypto/x509"
	"testing"
	"time"
)

// Reissuing the console's credentials.
//
// The dangerous mistake here is not a rotation that fails — that is loud. It is
// a rotation that mints a new authority along with the leaves, because both
// certificates would still work, the console would still connect, and every
// caller certificate in the organisation would stop authenticating at once with
// nothing pointing back at this function.
//
// So the authority assertions below are the load-bearing ones, and they are
// written as byte comparisons rather than as "still verifies": a regenerated
// root verifies its own fresh leaves perfectly well.

// The point of the exercise: the credentials actually change.
//
// Without this, a no-op implementation satisfies every other test in this file,
// and an operator who rotated after a suspected leak would still be holding the
// leaked certificate.
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

// Neither authority is touched, byte for byte.
//
// This is the assertion that catches the catastrophic version. ensureCerts
// refuses to regenerate an authority for exactly this reason: a fresh root
// orphans every caller certificate already issued under the old one, and the
// organisation finds out when its callers stop authenticating rather than when
// this function runs.
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

// A caller certificate issued before the rotation still works after it.
//
// The same property as above, stated the way an organisation experiences it.
// The authority comparison is what a reviewer checks; this is what breaks at
// three in the morning, so it is worth asserting directly rather than trusting
// that the two are equivalent.
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

// The reissued leaves chain where they are supposed to, and nowhere else.
//
// certs_test.go asserts this for Generate. It has to hold for the rotation path
// too: signing the signer-client leaf with the issuing CA would collapse the two
// trust domains into one, which is the failure the package comment exists to
// prevent — and every leaf would still look valid.
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

// Everything that is not the console's is left alone.
//
// The server leaves are mounted by running pods. Replacing them here would make
// a rotation require a restart, which is the property that makes this cheap
// enough to run unattended.
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

// The names and usages are the ones the rest of the system checks for.
//
// ConsoleCN is what the server's allowlist admits and what
// SIGNER_ALLOWED_CLIENT_CNS matches. A rotation that produced a correct
// certificate under a different name would be refused everywhere, and the
// error would be about a certificate rather than about this.
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

// The lifetime restarts from the rotation, which is the point of rotating.
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

// A bundle that cannot sign is refused, and refused before anything is replaced.
//
// Half a rotation is the worst outcome available: a console holding a fresh
// certificate for atlantis and a stale one for the signer works until the next
// enrolment, then fails somewhere unrelated to this.
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
