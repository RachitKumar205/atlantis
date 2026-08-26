package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// cmdPromote moves every staged migration from <stage-dir> into <migrations-dir>.
//
// It re-runs no codegen and no diff. The staged directory ends empty and the
// migration is part of the committed history.
//
// Named `promote`, not `approve`. `approve` is the console's in-product
// approval of DDL against live data, and `tidectl approve` printing "approve
// ok" for a file move reads as a change cleared to apply while the request is
// still queued in the console.
//
// `approve` still works, hidden and with a warning, so a pipeline that uses it
// does not break on upgrade. Remove the alias one release from now.
func cmdPromote(args []string) int {
	fs := flagSet("promote")
	stageDir := fs.String("stage-dir", "migrations/tidectl/_staged", "Source directory holding staged migration")
	migrationsDir := fs.String("migrations-dir", "migrations/tidectl", "Target migrations directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	entries, err := os.ReadDir(*stageDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "promote:", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "promote: nothing staged")
		return 1
	}

	if err := os.MkdirAll(*migrationsDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "promote:", err)
		return 1
	}

	moved := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		from := filepath.Join(*stageDir, e.Name())
		to := filepath.Join(*migrationsDir, e.Name())
		if err := os.Rename(from, to); err != nil {
			fmt.Fprintf(os.Stderr, "promote: %s: %v\n", e.Name(), err)
			return 1
		}
		moved++
	}
	fmt.Printf("promote ok (%d files moved)\n", moved)
	return 0
}

// cmdApproveAlias is the deprecated spelling of promote.
//
// It warns on stderr. `approve` reads as clearing a change to run against
// production, and the command only moves staged files, so a silent alias leaves
// that reading intact.
func cmdApproveAlias(args []string) int {
	fmt.Fprintln(os.Stderr, "tidectl: `approve` is now `promote`; this alias will be removed in the next release.")
	fmt.Fprintln(os.Stderr, "         It moves staged files into migrations/. It does not approve a schema change —")
	fmt.Fprintln(os.Stderr, "         that happens in the console, against a plan the server is holding.")
	return cmdPromote(args)
}
