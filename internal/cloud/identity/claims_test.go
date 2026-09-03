package identity

import "testing"

// requireRole and the change policy compare against these values exactly, so
// the set is closed: a near-miss authenticates and then fails every
// authorization check, which reads as a revoked grant.
func TestRoleValidity(t *testing.T) {
	valid := []Role{RoleAdmin, RoleDeveloper, RoleViewer}
	for _, r := range valid {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false, want true", r)
		}
	}
	invalid := []Role{"", "Admin", "administrator", "Developer", "dev", "owner", "viewer "}
	for _, r := range invalid {
		if r.Valid() {
			t.Errorf("Role(%q).Valid() = true, want false", r)
		}
	}
}
