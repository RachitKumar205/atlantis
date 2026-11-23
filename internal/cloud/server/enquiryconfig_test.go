package server

import "testing"

// CLOUD_SITE_ORIGIN is compared byte for byte with each request's Origin after
// originOf, so a value in any other form would refuse every enquiry.
func TestSiteOriginMustBeABareOrigin(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"https://tryatlantis.dev", "https://tryatlantis.dev", true},
		{"https://tryatlantis.dev/", "https://tryatlantis.dev", true},
		{"http://localhost:4325", "http://localhost:4325", true},
		{"https://tryatlantis.dev/contact", "", false},
		{"tryatlantis.dev", "", false},
		{"https://TryAtlantis.dev", "", false},
		{"https://tryatlantis.dev:443", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("CLOUD_MAIL_DEV", "true")
			t.Setenv("CLOUD_SITE_ORIGIN", c.in)

			cfg, err := ConfigFromEnv()
			switch {
			case c.ok && err != nil:
				t.Fatalf("refused: %v", err)
			case !c.ok && err == nil:
				t.Fatalf("accepted as %q; it can never equal an Origin header", cfg.SiteOrigin)
			case c.ok && cfg.SiteOrigin != c.want:
				t.Fatalf("SiteOrigin = %q, want %q", cfg.SiteOrigin, c.want)
			}
		})
	}
}
