package server

import "testing"

// originOf reduces both a registered console URL and a browser's Origin header
// to one form, so the step-up route compares like with like.
func TestOriginOfReducesToTheBrowsersForm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://console.example", "https://console.example"},
		{"https://console.example/", "https://console.example"},
		{"https://console.example/org/acme", "https://console.example"},
		{"HTTPS://Console.Example", "https://console.example"},
		{"https://console.example:443", "https://console.example"},
		{"https://console.example:8443", "https://console.example:8443"},
		{"http://localhost:80", "http://localhost"},
		{"http://localhost:3000", "http://localhost:3000"},
		{"http://localhost:443", "http://localhost:443"},
		{"https://[::1]:443", "https://[::1]"},
		{"https://[::1]:8443", "https://[::1]:8443"},
		{"https://[::1]", "https://[::1]"},
		{"null", ""},
		{"", ""},
		{"console.example", ""},
		{"://nonsense", ""},
	} {
		if got := originOf(tc.in); got != tc.want {
			t.Errorf("originOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
