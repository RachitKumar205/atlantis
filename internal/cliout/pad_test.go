package cliout

import (
	"fmt"
	"strings"
	"testing"
)

// Padding a coloured cell with %-Ns is the defect this replaces: fmt counts
// bytes, colour adds invisible ones, and the column collapses by exactly the
// length of the escape sequence.
func TestPadIgnoresColour(t *testing.T) {
	prev := Enabled
	Enabled = true
	t.Cleanup(func() { Enabled = prev })

	coloured := Cyan("orders")
	if len(coloured) == len("orders") {
		t.Skip("colour is not being applied in this environment")
	}

	// The behaviour being replaced, asserted so the reason stays visible.
	if got := len(fmt.Sprintf("%-20s", coloured)); got == 20 {
		t.Fatal("fmt padded a coloured string to the right visible width, so this " +
			"helper is unnecessary — check the assumption before deleting it")
	}

	padded := Pad(coloured, 20)
	if got := Width(padded); got != 20 {
		t.Errorf("Pad(Cyan(%q), 20) is %d visible columns, want 20", "orders", got)
	}
	if !strings.HasPrefix(StripANSI(padded), "orders") {
		t.Errorf("Pad mangled the content: %q", StripANSI(padded))
	}

	// A cell wider than the field keeps all its content: a clipped identifier
	// is worse than a ragged row.
	long := strings.Repeat("x", 30)
	if Width(Pad(long, 20)) != 30 {
		t.Error("Pad truncated content that overflowed its column")
	}

	// Uncoloured input must behave exactly like %-Ns.
	if Pad("abc", 6) != "abc   " {
		t.Errorf("Pad(%q, 6) = %q", "abc", Pad("abc", 6))
	}

	// Multi-byte runes count as one column each, not as their byte length.
	if got := Width("héllo"); got != 5 {
		t.Errorf("Width(\"héllo\") = %d, want 5", got)
	}
}
