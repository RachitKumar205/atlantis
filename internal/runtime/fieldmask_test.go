package runtime

import "testing"

func TestFieldSet(t *testing.T) {
	cases := []struct {
		name    string
		mask    []string
		present bool
		want    bool
	}{
		{"no mask, present", nil, true, true},
		{"no mask, absent", nil, false, false},
		{"named, present", []string{"body"}, true, true},
		// The case the mask exists for: NULL into a nullable column.
		{"named, absent", []string{"body"}, false, true},
		{"unnamed, present", []string{"title"}, true, false},
		{"unnamed, absent", []string{"title"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FieldSet(c.mask, "body", c.present); got != c.want {
				t.Errorf("FieldSet(%v, body, %v) = %v, want %v", c.mask, c.present, got, c.want)
			}
		})
	}
}
