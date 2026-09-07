// Package cliflag parses a subcommand's arguments with flags and positional
// arguments in any order.
package cliflag

import (
	"flag"
	"fmt"
	"strings"
)

// Parse parses args into fs and returns the positional arguments in order.
//
// Flags may appear before, between or after positional arguments. A "--"
// in argument position ends flag parsing and every argument after it is
// positional; one in value position ("--args --") is the flag's value. A
// lone "-" is positional. An argument that begins with "-" and a digit is a
// flag, as it is for flag.FlagSet.
//
// flag.FlagSet.Parse stops at the first positional argument, so
// `tide job submit reindex --args='{"id":7}'` parsed with it submits the job
// with the default arguments and reports nothing.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			return append(positional, args[1:]...), nil
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			args = args[1:]
			continue
		}
		// One flag at a time. A flag without "=" that is not boolean takes
		// the next argument as its value; fs reports a flag it does not know
		// or a missing value itself.
		n := 1
		if !strings.Contains(a, "=") && !isBoolFlag(fs, a) && len(args) > 1 {
			n = 2
		}
		if err := fs.Parse(args[:n]); err != nil {
			return nil, err
		}
		args = args[n:]
	}
	return positional, nil
}

// ParseNoArgs parses args into fs and reports a positional argument as an
// error, printed to fs.Output() with the usage the way fs reports its own.
func ParseNoArgs(fs *flag.FlagSet, args []string) error {
	pos, err := Parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return nil
	}
	err = fmt.Errorf("unexpected argument %q", pos[0])
	_, _ = fmt.Fprintln(fs.Output(), err)
	if fs.Usage != nil {
		fs.Usage()
	} else {
		_, _ = fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())
		fs.PrintDefaults()
	}
	return err
}

func isBoolFlag(fs *flag.FlagSet, arg string) bool {
	f := fs.Lookup(strings.TrimLeft(arg, "-"))
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
