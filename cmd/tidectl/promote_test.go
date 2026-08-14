package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promote moves staged files, and `approve` still reaches it.
//
// The alias is what keeps a pipeline working across the upgrade that renamed
// the command. A test for it looks like testing a redirect, and is not: the
// alias is the entire reason the rename is safe to ship, and it is exactly the
// kind of thing a later tidy-up deletes as obviously redundant.
func TestPromoteMovesStagedFilesAndApproveStillReachesIt(t *testing.T) {
	for _, name := range []string{"promote", "approve"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			staged := filepath.Join(dir, "_staged")
			target := filepath.Join(dir, "migrations")
			if err := os.MkdirAll(staged, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"0001_x.up.sql", "0001_x.down.sql"} {
				if err := os.WriteFile(filepath.Join(staged, f), []byte("-- sql\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			fn := cmdPromote
			if name == "approve" {
				fn = cmdApproveAlias
			}
			if code := fn([]string{"-stage-dir=" + staged, "-migrations-dir=" + target}); code != 0 {
				t.Fatalf("%s exited %d", name, code)
			}

			for _, f := range []string{"0001_x.up.sql", "0001_x.down.sql"} {
				if _, err := os.Stat(filepath.Join(target, f)); err != nil {
					t.Errorf("%s did not move %s: %v", name, f, err)
				}
			}
			left, err := os.ReadDir(staged)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Errorf("%s left %d files staged", name, len(left))
			}
		})
	}
}

// The old name stays reachable and stays out of the usage output.
//
// atlantis now has an in-product approval — a human clearing a schema change to
// run against a production database. An operator who reads `tidectl` and sees
// `approve` listed will reasonably believe that is how they do it, run it, see
// "ok", and conclude the change is cleared. It is not: the request is still
// waiting in the console, and all they have done is move two files.
//
// Asserted against the dispatch table and the rendered usage rather than
// against this package's source. A source scan would pin the formatting and go
// red on a reordering that changes nothing, while passing happily if the table
// were built correctly and printed wrongly.
func TestTheRetiredApproveNameIsReachableButUnadvertised(t *testing.T) {
	byName := map[string]command{}
	for _, c := range commands() {
		byName[c.name] = c
	}

	promote, ok := byName["promote"]
	if !ok {
		t.Fatal("promote is not registered")
	}
	if promote.hidden {
		t.Error("promote is hidden; it is the name operators are meant to learn")
	}

	alias, ok := byName["approve"]
	if !ok {
		t.Fatal("the approve alias was removed — a pipeline that still calls it breaks " +
			"on upgrade, which is the whole reason the alias exists")
	}
	if !alias.hidden {
		t.Error("approve is advertised again; an operator reading the usage will take " +
			"it for the in-product approval and believe a change is cleared to run")
	}

	var usage strings.Builder
	writeUsage(&usage, commands())
	if strings.Contains(usage.String(), "approve") {
		t.Errorf("the usage output still offers `approve`:\n%s", usage.String())
	}
	if !strings.Contains(usage.String(), "promote") {
		t.Error("the usage output does not mention promote")
	}
}
