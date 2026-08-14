package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// cmdPromote moves every staged migration from <stage-dir> into <migrations-dir>.
//
// Promote is simpler than plan: it does not re-run codegen or re-diff. The
// operator who reviewed the staged SQL is the one promoting it, and the only
// thing this does is `mv staged migrations/`. Afterwards the staged dir is
// empty and the migration is part of the committed history.
//
// # It was called `approve`, and the rename is not cosmetic
//
// atlantis now has an in-product approval: a human decides, in the console,
// whether a schema change may apply to a production database. Leaving a CLI
// command named `approve` beside it meant one word covering two operations
// that share nothing — one moves files on disk in a repository, the other
// authorises DDL against live data.
//
// The failure that name produces is specific and bad. An operator told to
// "approve the migration" runs `tidectl approve`, sees "approve ok", and
// believes the change is cleared to apply. Nothing has been reviewed by
// anybody, and the request is still sitting in the console queue. The command's
// own help string already said "Promote a staged migration into migrations/" —
// the right name was written down before the collision existed.
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
// It warns on stderr rather than staying silent, because the whole point of
// the rename is that somebody currently believes `tidectl approve` means "this
// change is cleared to run against production". A silent alias would let them
// go on believing it.
func cmdApproveAlias(args []string) int {
	fmt.Fprintln(os.Stderr, "tidectl: `approve` is now `promote`; this alias will be removed in the next release.")
	fmt.Fprintln(os.Stderr, "         It moves staged files into migrations/. It does not approve a schema change —")
	fmt.Fprintln(os.Stderr, "         that happens in the console, against a plan the server is holding.")
	return cmdPromote(args)
}
