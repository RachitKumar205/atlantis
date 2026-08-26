package server

import (
	"strings"
	"testing"
)

// How Cloud decides it can send mail. The behaviour under test is a refusal,
// and a refusal that no test exercises can be removed by a two-line edit that
// breaks nothing: the failure it prevents is a deployment that starts, prints
// password-reset links into its log, and looks from the outside exactly like
// one that is delivering them.

// setBaseEnv sets everything ConfigFromEnv requires apart from mail, so each
// test below varies only the thing it is about.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLOUD_PG_URL", "postgres://cloud/x")
	t.Setenv("CLOUD_ISSUER", "https://cloud.example.test")
	t.Setenv("CLOUD_PUBLIC_URL", "https://cloud.example.test")
	t.Setenv("CLOUD_DATA_KEY", "a-keyset")
	// Cleared explicitly. These are read from the process environment, and a
	// developer with either exported would otherwise change what these tests
	// mean without changing a line of them.
	t.Setenv("CLOUD_RESEND_API_KEY", "")
	t.Setenv("CLOUD_MAIL_FROM", "")
	t.Setenv("CLOUD_MAIL_DEV", "")
	t.Setenv("CLOUD_SMTP_ADDR", "")
	t.Setenv("CLOUD_SMTP_FROM", "")
}

// The inverted default: nothing configured is now a refusal.
func TestCloudRefusesToStartWithNoMailTransport(t *testing.T) {
	setBaseEnv(t)

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("Cloud started with no way to send mail; verification and password " +
			"reset would silently go nowhere")
	}
	// The message has to carry the way out, or the first person to hit this in
	// development is stuck at a refusal with no next step.
	for _, want := range []string{"CLOUD_RESEND_API_KEY", "CLOUD_MAIL_DEV"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %v", want, err)
		}
	}
}

// Resend, configured completely, is accepted and selected.
func TestResendConfigurationIsAccepted(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_RESEND_API_KEY", "re_live_key")
	t.Setenv("CLOUD_MAIL_FROM", "Atlantis <no-reply@tryatlantis.dev>")

	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a complete Resend configuration was refused: %v", err)
	}
	if c.ResendAPIKey != "re_live_key" || c.MailFrom != "Atlantis <no-reply@tryatlantis.dev>" {
		t.Errorf("the mail settings did not survive: key=%q from=%q", c.ResendAPIKey, c.MailFrom)
	}
}

// Half a transport is a mistake, not a choice — the same rule the OAuth
// providers beside it follow.
func TestCloudRefusesAHalfConfiguredTransport(t *testing.T) {
	t.Run("a key with no sender", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("CLOUD_RESEND_API_KEY", "re_live_key")

		_, err := ConfigFromEnv()
		if err == nil {
			t.Fatal("a transport with no sender was accepted; every receiver " +
				"refuses a message with no From")
		}
		if !strings.Contains(err.Error(), "CLOUD_MAIL_FROM") {
			t.Errorf("the refusal does not name the missing setting: %v", err)
		}
	})

	t.Run("an SMTP server with no sender", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("CLOUD_SMTP_ADDR", "smtp.example.test:587")

		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("an SMTP transport with no sender was accepted")
		}
	})
}

// CLOUD_SMTP_FROM still means what it meant.
//
// A deployment configured before CLOUD_MAIL_FROM existed keeps working without
// being edited — the reason the older name is a fallback rather than a removal.
func TestTheOlderSenderSettingStillWorks(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_SMTP_ADDR", "smtp.example.test:587")
	t.Setenv("CLOUD_SMTP_FROM", "no-reply@tryatlantis.dev")

	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a deployment using the older setting was refused: %v", err)
	}
	if c.MailFrom != "no-reply@tryatlantis.dev" {
		t.Errorf("MailFrom is %q; CLOUD_SMTP_FROM did not carry over", c.MailFrom)
	}
}

// CLOUD_MAIL_FROM wins when both are set, since it is the one that describes
// the message rather than a transport.
func TestTheNewerSenderSettingWins(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_SMTP_ADDR", "smtp.example.test:587")
	t.Setenv("CLOUD_SMTP_FROM", "old@tryatlantis.dev")
	t.Setenv("CLOUD_MAIL_FROM", "new@tryatlantis.dev")

	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.MailFrom != "new@tryatlantis.dev" {
		t.Errorf("MailFrom is %q, want the CLOUD_MAIL_FROM value", c.MailFrom)
	}
}

// Two transports is ambiguous rather than redundant.
func TestCloudRefusesTwoTransports(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_RESEND_API_KEY", "re_live_key")
	t.Setenv("CLOUD_SMTP_ADDR", "smtp.example.test:587")
	t.Setenv("CLOUD_MAIL_FROM", "no-reply@tryatlantis.dev")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("both transports were accepted; a failed delivery would be " +
			"investigated against whichever one the code happened not to pick")
	}
}

// The development path still exists, and takes one explicit setting.
func TestTheLoggingMailerRequiresAskingForIt(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_MAIL_DEV", "true")

	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("CLOUD_MAIL_DEV=true was refused: %v", err)
	}
	if !c.MailDev {
		t.Error("MailDev did not survive the environment")
	}
}

// Asking for the logging mailer AND a real transport is refused.
//
// The logging mailer would win, so the deployment would send nothing while
// holding a valid API key — which reads, from the configuration, as though mail
// were set up.
func TestCloudRefusesTheLoggingMailerAlongsideARealTransport(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("CLOUD_MAIL_DEV", "true")
	t.Setenv("CLOUD_RESEND_API_KEY", "re_live_key")
	t.Setenv("CLOUD_MAIL_FROM", "no-reply@tryatlantis.dev")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a deployment holding both was accepted; it would deliver nothing " +
			"while looking configured")
	}
	if !strings.Contains(err.Error(), "CLOUD_MAIL_DEV") {
		t.Errorf("the refusal does not name the setting to unset: %v", err)
	}
}
