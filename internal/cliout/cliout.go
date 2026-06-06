// Package cliout renders CLI output styled to match the operator
// console's design language (brass + sage + coral + slate on dark
// terminal), with TTY-aware capability detection so machine-consumed
// pipes get clean text.
//
// Aesthetic: industrial-instrument. The console reads as a brass-lit
// scientific instrument panel; the CLI mirrors that — lowercase labels,
// hairline rules, tabular numerics, restrained color, the same
// concentric-ring sigil that anchors the console's logo.
//
// Capability tiers (auto-detected):
//
//	24-bit ($COLORTERM=truecolor | 24bit | iTerm.app)
//	  → exact console hex via \x1b[38;2;R;G;Bm
//	4-bit ($TERM looks color-capable, no truecolor signal)
//	  → nearest standard ANSI (sage→green, brass→yellow, coral→red,
//	    slate→blue, ink-2/3→bright black)
//	none  (NO_COLOR set, stdout not a TTY, $TERM=dumb)
//	  → all wrappers pass strings through unchanged
//
// Public API:
//
//	Sage(s)  Brass(s)  Coral(s)  Slate(s)  Mute(s)  Faint(s)
//	  semantic foreground colors matching the console palette
//
//	Bold(s)  Dim(s)
//	  intensity modifiers (the only non-color ANSI we emit)
//
//	Red, Green, Yellow, Blue, Cyan, Grey
//	  back-compat aliases — bound to the same semantic palette so
//	  existing call sites pick up the new look without edits.
//
//	Logo(w)  LogoInline(w)
//	  ⊙ sigil + lowercase wordmark, stacked or horizontal
//
//	Header(w, label)  Rule(w, n)
//	  section header with hairline U+2500 rule (22 cols default)
//
//	Field(w, key, val)  Row(w, state, label, meta)
//	  aligned label/value pair and status row with bullet glyph
//
//	Successf  Warnf  Errorf  Infof  Banner
//	  level helpers — printf-style for one-shot status lines.
//
//	NewSpinner(label) ... Spinner.Start/Stop
//	  braille spinner in brass for long-running ops. No-op when
//	  Enabled=false so CI logs stay clean.
package cliout

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Enabled is the master toggle. Initialized from NO_COLOR + TTY +
// $TERM at package init. Callers can flip it via a --no-color flag.
var Enabled = detectEnabled()

// trueColor reports whether the terminal accepts 24-bit color escapes.
// When false but Enabled is true, we fall back to 4-bit ANSI.
var trueColor = detectTruecolor()

func detectEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if t := os.Getenv("TERM"); t == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func detectTruecolor() bool {
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}
	// iTerm2, Apple Terminal, Alacritty, Kitty, WezTerm, modern xterm
	// all support 24-bit even without COLORTERM set in some session
	// configurations. We err on the side of emitting truecolor and
	// trust unsupported terminals to render the unknown sequence as
	// nothing — visually identical to no-color.
	if t := os.Getenv("TERM"); strings.Contains(t, "256color") || strings.Contains(t, "direct") {
		return true
	}
	if os.Getenv("TERM_PROGRAM") != "" {
		return true
	}
	return false
}

// ─────────────────────────── palette ───────────────────────────

// console palette — RGB triples lifted verbatim from
// web/console/src/styles/tokens.css. Keep these in sync if the
// console's brand colors shift.
var (
	sageRGB  = rgb{125, 181, 147} // --sage         #7DB593
	brassRGB = rgb{212, 165, 116} // --accent       #D4A574
	coralRGB = rgb{214, 120, 120} // --coral        #D67878
	slateRGB = rgb{138, 164, 200} // --slate        #8AA4C8
	ink2RGB  = rgb{142, 142, 151} // --ink-2        #8E8E97
	ink3RGB  = rgb{92, 92, 100}   // --ink-3        #5C5C64
)

type rgb struct{ r, g, b uint8 }

func (c rgb) seq() string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
}

// 4-bit fallback codes. The eye sees roughly the same role even if the
// hue is off (sage→green, brass→yellow, coral→red, slate→blue, ink→grey).
const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	dim     = "\x1b[2m"
	fbSage  = "\x1b[32m" // green
	fbBrass = "\x1b[33m" // yellow
	fbCoral = "\x1b[31m" // red
	fbSlate = "\x1b[34m" // blue
	fbInk2  = "\x1b[90m" // bright black
	fbInk3  = "\x1b[90m" // bright black (no separate dim grey)
)

// wrap returns s wrapped in the chosen escape sequence (truecolor or
// fallback), or s unchanged when Enabled is false.
func wrap(c rgb, fb, s string) string {
	if !Enabled {
		return s
	}
	if trueColor {
		return c.seq() + s + reset
	}
	return fb + s + reset
}

func wrapPlain(seq, s string) string {
	if !Enabled {
		return s
	}
	return seq + s + reset
}

// ─────────────────────────── semantic colors ───────────────────────────

// Sage tints text in --sage (#7DB593). Use for additive / success / healthy.
func Sage(s string) string { return wrap(sageRGB, fbSage, s) }

// Brass tints text in --accent (#D4A574). The console's signature
// accent — use for backfill, "will-do" intent, primary highlights.
func Brass(s string) string { return wrap(brassRGB, fbBrass, s) }

// Coral tints text in --coral (#D67878). Use for breaking / errors.
func Coral(s string) string { return wrap(coralRGB, fbCoral, s) }

// Slate tints text in --slate (#8AA4C8). Use for FK references, info,
// neutral metadata that wants a soft callout.
func Slate(s string) string { return wrap(slateRGB, fbSlate, s) }

// Mute tints text in --ink-2 (#8E8E97). Use for metadata, labels,
// muted captions that should sit behind the primary content.
func Mute(s string) string { return wrap(ink2RGB, fbInk2, s) }

// Faint tints text in --ink-3 (#5C5C64). Even fainter — for inactive
// state bullets, captions that shouldn't pull the eye at all.
func Faint(s string) string { return wrap(ink3RGB, fbInk3, s) }

// ─────────────────────────── intensity ───────────────────────────

// Bold makes s bold. Most modern terminals render this as a heavier
// weight rather than brighter; in older ones it brightens the color.
func Bold(s string) string { return wrapPlain(bold, s) }

// Dim halves the perceived intensity of s. Subtle; use sparingly.
func Dim(s string) string { return wrapPlain(dim, s) }

// ─────────────────────────── back-compat aliases ───────────────────────────
//
// Existing call sites use Red / Green / Yellow / Blue / Cyan / Grey.
// Rebind to the semantic palette so they pick up the console look
// without per-call-site edits. New code should prefer the semantic
// names above (Sage / Brass / Coral / Slate / Mute).

// Red is Coral (breaking / error).
func Red(s string) string { return Coral(s) }

// Green is Sage (additive / success).
func Green(s string) string { return Sage(s) }

// Yellow is Brass (backfill / accent).
func Yellow(s string) string { return Brass(s) }

// Blue is Slate (FK / info).
func Blue(s string) string { return Slate(s) }

// Cyan is Slate. Cyan-as-info was the old convention; we collapse
// onto slate so the palette stays small.
func Cyan(s string) string { return Slate(s) }

// Grey is Mute (--ink-2).
func Grey(s string) string { return Mute(s) }

// ─────────────────────────── glyphs ───────────────────────────

// State glyphs match what the console renders for the same semantics.
// Kept as constants so consumers don't pass color names to indirectors.
const (
	GlyphMuted   = "·" // U+00B7 — inactive bullet
	GlyphFilled  = "●" // U+25CF — active, colored
	GlyphOutline = "○" // U+25CB — pending
	GlyphCheck   = "✔" // U+2714 — success
	GlyphCross   = "✖" // U+2716 — failure
	GlyphWarn    = "⚠" // U+26A0 — warning
	GlyphInfo    = "ℹ" // U+2139 — info
	GlyphRing    = "⊙" // U+2299 — concentric ring (logo sigil)
	GlyphRule    = "─" // U+2500 — hairline
	GlyphArrow   = "→" // U+2192 — flow / continuation
)

// ─────────────────────────── primitives ───────────────────────────

// Logo prints the stacked logo:
//
//	⊙
//	atlantis  v<version>
//
// Used by `tide version` and `tidectl version` and the first-run help
// banner. version may be empty.
func Logo(w io.Writer, name, version string) {
	if name == "" {
		name = "atlantis"
	}
	_, _ = fmt.Fprintln(w, "  "+Brass(GlyphRing))
	if version == "" {
		_, _ = fmt.Fprintln(w, "  "+Bold(name))
	} else {
		_, _ = fmt.Fprintln(w, "  "+Bold(name)+"  "+Mute(version))
	}
}

// LogoInline prints the logo on one line:
//
//	⊙  atlantis  v<version>
//
// Horizontal layout for help banners where vertical space is
// expensive. version may be empty.
func LogoInline(w io.Writer, name, version string) {
	if name == "" {
		name = "atlantis"
	}
	out := Brass(GlyphRing) + "  " + Bold(name)
	if version != "" {
		out += "  " + Mute(version)
	}
	_, _ = fmt.Fprintln(w, out)
}

// Header prints a section header in lowercase + a 22-col hairline
// rule. Mimics the console's section banners:
//
//	plan ─────────────────────
//
// Wide-column tabular output should use this instead of bare Bold().
func Header(w io.Writer, label string) {
	// 24 - len(label) - 1 trailing space; minimum 8 rule chars.
	rule := max(24-len(label)-1, 8)
	_, _ = fmt.Fprintln(w, Bold(label)+" "+Faint(strings.Repeat(GlyphRule, rule)))
}

// Rule prints a standalone hairline of n columns (default 24 when n<=0).
func Rule(w io.Writer, n int) {
	if n <= 0 {
		n = 24
	}
	_, _ = fmt.Fprintln(w, Faint(strings.Repeat(GlyphRule, n)))
}

// Field renders an aligned key/value pair indented under a Header:
//
//	plan_id    P-abc123de
//	class      additive
//
// key is left-padded into a 9-col mute column; val is rendered with
// the caller's own coloring (commonly via Sage/Brass/Coral semantics).
func Field(w io.Writer, key, val string) {
	const keyWidth = 9
	pad := max(keyWidth-len(key), 1)
	_, _ = fmt.Fprintln(w, "  "+Mute(key)+strings.Repeat(" ", pad)+val)
}

// Row renders a status row under a Header:
//
//	●  backend                  added field display_name
//	·  data-pipeline          (no impact)
//
// state controls the bullet: "active" → colored ●, "muted" → · in
// faint grey, "warn" → ●  in brass, "ok" → ●  in sage, "err" → ●  in
// coral. label is fixed-width 24 cols; meta is rendered in mute grey.
func Row(w io.Writer, state, label, meta string) {
	bullet := rowBullet(state)
	pad := max(24-displayWidth(label), 1)
	_, _ = fmt.Fprintln(w, "  "+bullet+"  "+label+strings.Repeat(" ", pad)+Mute(meta))
}

// SubRow renders a continuation row under a Row, indented past the
// bullet column so the eye still associates it with the parent.
//
//	●  timescaledb              will be auto-enabled
//	   audit.Event is a hypertable
//
// Used by extension auto-enable for the trigger note, by impact rows
// for breaking details, etc.
func SubRow(w io.Writer, text string) {
	_, _ = fmt.Fprintln(w, "     "+Faint(text))
}

func rowBullet(state string) string {
	switch state {
	case "ok", "sage", "additive":
		return Sage(GlyphFilled)
	case "warn", "brass", "backfill", "active":
		return Brass(GlyphFilled)
	case "err", "coral", "break", "breaking":
		return Coral(GlyphFilled)
	case "info", "slate":
		return Slate(GlyphFilled)
	case "muted", "skip", "":
		return Faint(GlyphMuted)
	case "pending", "wait":
		return Faint(GlyphOutline)
	default:
		return Faint(GlyphMuted)
	}
}

// displayWidth returns the visible column count of s, ignoring ANSI
// escapes so Row alignment stays correct when label is pre-colored.
func displayWidth(s string) int {
	w := 0
	in := false
	for _, r := range s {
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		if r == '\x1b' {
			in = true
			continue
		}
		w++
	}
	return w
}

// ─────────────────────────── level helpers ───────────────────────────

// Successf prints `✔ <msg>` to stdout in sage. printf-style.
func Successf(format string, args ...any) {
	fmt.Printf("%s %s\n", Sage(GlyphCheck), fmt.Sprintf(format, args...))
}

// Warnf prints `⚠ <msg>` to stdout in brass. printf-style.
func Warnf(format string, args ...any) {
	fmt.Printf("%s %s\n", Brass(GlyphWarn), fmt.Sprintf(format, args...))
}

// Errorf prints `✖ <msg>` to STDERR in coral. printf-style.
func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", Coral(GlyphCross), fmt.Sprintf(format, args...))
}

// Infof prints `ℹ <msg>` to stdout in slate. printf-style.
func Infof(format string, args ...any) {
	fmt.Printf("%s %s\n", Slate(GlyphInfo), fmt.Sprintf(format, args...))
}

// Banner draws a colored bullet + bold label as a one-shot section
// header. Kept for back-compat with existing call sites; new code
// should use Header() for sectioned reports.
//
// Accepted color names: "sage", "brass", "coral", "slate", and the
// legacy "green", "yellow", "red", "cyan".
func Banner(w io.Writer, color, label string) {
	var bullet string
	switch color {
	case "sage", "green":
		bullet = Sage(GlyphFilled)
	case "brass", "yellow":
		bullet = Brass(GlyphFilled)
	case "coral", "red":
		bullet = Coral(GlyphFilled)
	case "slate", "cyan", "blue":
		bullet = Slate(GlyphFilled)
	default:
		bullet = GlyphFilled
	}
	_, _ = fmt.Fprintf(w, "%s %s\n", bullet, Bold(label))
}

// ─────────────────────────── Spinner ───────────────────────────

// Spinner is a brass-tinted braille animation for long-running ops.
// Frame rate is fixed at 80ms, which feels mechanical rather than
// bouncy. The animation runs in its own goroutine; Stop() drains it
// and clears the line.
//
// When Enabled is false (NO_COLOR / non-TTY), Start() is a no-op and
// Stop() prints nothing — CI logs stay clean.
type Spinner struct {
	label string
	out   io.Writer
	stop  chan struct{}
	done  chan struct{}
	mu    sync.Mutex
	on    bool
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// NewSpinner constructs a spinner that prints to stdout. The label
// renders after the spinning brass dot:
//
//	⠋ booting embedded Postgres...
func NewSpinner(label string) *Spinner {
	return &Spinner{label: label, out: os.Stdout}
}

// Start kicks off the animation. Safe to call multiple times; only
// the first call wires the goroutine.
func (s *Spinner) Start() {
	if !Enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on {
		return
	}
	s.on = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run()
}

// Stop halts the animation and erases the spinner line (\r + clear-to-
// end-of-line). The caller is expected to print a final status row
// (Successf / Errorf) immediately after.
func (s *Spinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.on {
		return
	}
	close(s.stop)
	<-s.done
	s.on = false
	_, _ = fmt.Fprint(s.out, "\r\x1b[2K")
}

func (s *Spinner) run() {
	defer close(s.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	frame := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			_, _ = fmt.Fprintf(s.out, "\r%s %s", Brass(spinnerFrames[frame%len(spinnerFrames)]), s.label)
			frame++
		}
	}
}
