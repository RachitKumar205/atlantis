// Package cliout renders CLI output with TTY-aware ANSI coloring.
//
// Both tide and tidectl print human-readable progress, plan reports,
// drift summaries, etc. When stdout is a terminal, we colorize so the
// eye picks out errors vs successes vs warnings without reading every
// line. When piped to a file or running in CI, we degrade to plain
// text — no surprises in machine-consumed logs.
//
// Standards honored:
//   - NO_COLOR env var (no-color.org): if set to any value, all
//     coloring is off regardless of TTY.
//   - --no-color: callers can set [Enabled] to false explicitly.
//   - Default: on when stdout is a character device.
//
// The color choices are deliberately small — three accents (red,
// yellow, green) plus cyan and grey-dim. We don't paint everything;
// the eye loses information when too much is colored.
package cliout

import (
	"fmt"
	"io"
	"os"
)

// ANSI escape sequences. Kept here rather than reaching for a third-
// party color library — the surface is small enough that an internal
// package is cheaper than a new dep.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	blue   = "\x1b[34m"
	cyan   = "\x1b[36m"
	grey   = "\x1b[90m"
)

// Enabled is the master toggle. Initialized from TTY detection +
// NO_COLOR at package init. Callers can flip it via a --no-color flag.
var Enabled = detectColor()

func detectColor() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// wrap returns s wrapped in the given ANSI sequence when Enabled,
// otherwise s unchanged. The reset is appended automatically so callers
// don't have to remember.
func wrap(seq, s string) string {
	if !Enabled {
		return s
	}
	return seq + s + reset
}

// Bold, Dim, Red, Green, Yellow, Blue, Cyan, Grey return s formatted
// for terminal output. Callers use them inside fmt.Sprintf composition.
func Bold(s string) string   { return wrap(bold, s) }
func Dim(s string) string    { return wrap(dim, s) }
func Red(s string) string    { return wrap(red, s) }
func Green(s string) string  { return wrap(green, s) }
func Yellow(s string) string { return wrap(yellow, s) }
func Blue(s string) string   { return wrap(blue, s) }
func Cyan(s string) string   { return wrap(cyan, s) }
func Grey(s string) string   { return wrap(grey, s) }

// Successf / Warnf / Errorf / Infof print a leveled line to stdout
// (stderr for Error). Pattern matches log levels operators expect.
// The prefix glyph is colored; the message body is left alone so
// embedded entity names / paths stand out by default.
func Successf(format string, args ...any) {
	fmt.Printf("%s %s\n", Green("✔"), fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	fmt.Printf("%s %s\n", Yellow("⚠"), fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", Red("✖"), fmt.Sprintf(format, args...))
}

func Infof(format string, args ...any) {
	fmt.Printf("%s %s\n", Cyan("ℹ"), fmt.Sprintf(format, args...))
}

// Banner draws a single-line section header with a colored bullet
// and a bolded label. Used for the top of multi-section reports
// (e.g., the adopt drift summary).
func Banner(w io.Writer, color, label string) {
	switch color {
	case "red":
		fmt.Fprintf(w, "%s %s\n", Red("●"), Bold(label))
	case "green":
		fmt.Fprintf(w, "%s %s\n", Green("●"), Bold(label))
	case "yellow":
		fmt.Fprintf(w, "%s %s\n", Yellow("●"), Bold(label))
	case "cyan":
		fmt.Fprintf(w, "%s %s\n", Cyan("●"), Bold(label))
	default:
		fmt.Fprintf(w, "● %s\n", Bold(label))
	}
}
