package cliout

import (
	"strings"
	"unicode/utf8"
)

// Width returns how many terminal columns s occupies, ignoring ANSI colour.
//
// fmt's %-Ns pads by BYTES, and a colour wrapper adds ~23 invisible bytes, so
// `fmt.Printf("%-32s", Cyan(name))` pads a 3-character name by 6 instead of 29
// and the column collapses. Worse, when only some cells are coloured — a
// warning highlight, say — the columns jump left and right row to row, which
// looks like corruption rather than styling.
func Width(s string) int {
	return utf8.RuneCountInString(StripANSI(s))
}

// StripANSI removes SGR escape sequences.
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Pad left-aligns s in a field of n visible columns.
//
// Use instead of %-Ns wherever a cell may be coloured. Over-long content is
// not truncated — a clipped identifier is worse than a ragged row, because the
// reader cannot tell which object it names.
func Pad(s string, n int) string {
	if w := Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
