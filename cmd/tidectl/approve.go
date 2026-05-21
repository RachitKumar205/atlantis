package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// cmdApprove moves every staged migration from <stage-dir> into <migrations-dir>.
//
// Approve is simpler than plan: it does not re-run codegen or
// re-diff. The operator who reviewed the staged SQL is the one approving
// it, and the only thing approve does is `mv staged migrations/`. After
// approve, the staged dir is empty and the migration is part of the
// committed history.
func cmdApprove(args []string) int {
	fs := flagSet("approve")
	stageDir := fs.String("stage-dir", "migrations/tidectl/_staged", "Source directory holding staged migration")
	migrationsDir := fs.String("migrations-dir", "migrations/tidectl", "Target migrations directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	entries, err := os.ReadDir(*stageDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approve:", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "approve: nothing staged")
		return 1
	}

	if err := os.MkdirAll(*migrationsDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "approve:", err)
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
			fmt.Fprintf(os.Stderr, "approve: %s: %v\n", e.Name(), err)
			return 1
		}
		moved++
	}
	fmt.Printf("approve ok (%d files moved)\n", moved)
	return 0
}
