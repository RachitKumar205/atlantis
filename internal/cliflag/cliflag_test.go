package cliflag

import (
	"bytes"
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
)

func newSet() (*flag.FlagSet, *string, *bool, *bytes.Buffer) {
	fs := flag.NewFlagSet("job submit", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	args := fs.String("args", "{}", "Args as a JSON object")
	dry := fs.Bool("dry-run", false, "Do nothing")
	return fs, args, dry, &out
}

func TestParse(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantPos  []string
		wantArgs string
		wantDry  bool
	}{
		{"flag after positional", []string{"reindex", `--args={"id":7}`}, []string{"reindex"}, `{"id":7}`, false},
		{"flag before positional", []string{`--args={"id":7}`, "reindex"}, []string{"reindex"}, `{"id":7}`, false},
		{"space-separated value after positional", []string{"reindex", "--args", `{"id":7}`}, []string{"reindex"}, `{"id":7}`, false},
		{"bool flag between positionals", []string{"a", "--dry-run", "b"}, []string{"a", "b"}, "{}", true},
		{"terminator", []string{"a", "--", "--args=x", "-b"}, []string{"a", "--args=x", "-b"}, "{}", false},
		{"lone dash is positional", []string{"-", "--dry-run"}, []string{"-"}, "{}", true},
		{"no arguments", nil, nil, "{}", false},
		{"only flags", []string{"--dry-run"}, nil, "{}", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, args, dry, _ := newSet()
			pos, err := Parse(fs, c.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(pos, c.wantPos) {
				t.Errorf("positional = %q, want %q", pos, c.wantPos)
			}
			if *args != c.wantArgs {
				t.Errorf("--args = %q, want %q", *args, c.wantArgs)
			}
			if *dry != c.wantDry {
				t.Errorf("--dry-run = %v, want %v", *dry, c.wantDry)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"unknown flag after positional", []string{"reindex", "--bogus"}, "bogus"},
		{"missing value", []string{"reindex", "--args"}, "needs an argument"},
		{"bad syntax", []string{"---args=x"}, "bad flag syntax"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, _, _, out := newSet()
			if _, err := Parse(fs, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("nothing printed to the FlagSet's output:\n%s", out.String())
			}
		})
	}
	fs, _, _, _ := newSet()
	if _, err := Parse(fs, []string{"reindex", "-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h after a positional: err = %v, want flag.ErrHelp", err)
	}
}

func TestParseNoArgs(t *testing.T) {
	fs, args, _, _ := newSet()
	if err := ParseNoArgs(fs, []string{"--args=x"}); err != nil {
		t.Fatalf("flags only: %v", err)
	}
	if *args != "x" {
		t.Errorf("--args = %q, want x", *args)
	}
	fs, _, _, out := newSet()
	err := ParseNoArgs(fs, []string{"--dry-run", "stray"})
	if err == nil || !strings.Contains(err.Error(), `unexpected argument "stray"`) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{`unexpected argument "stray"`, "Usage of job submit", "-args"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
