package main

import (
	"fmt"
	"os"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// cmdLint parses and lowers every .atl file in -schema-dir. Exits 0 iff
// every file parses, the union lowers, and validation passes. Used by CI
// to gate merges and by developers running `tidectl lint` in their editor.
func cmdLint(args []string) int {
	fs := flagSet("lint")
	schemaDir := fs.String("schema-dir", "schema", "Directory containing .atl files")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	files, err := loadATLFiles(*schemaDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint:", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "lint: no .atl files under %s\n", *schemaDir)
		return 1
	}
	if _, err := dsl.Lower(files); err != nil {
		fmt.Fprintln(os.Stderr, "lint:", err)
		return 1
	}
	fmt.Printf("lint ok (%d files)\n", len(files))
	return 0
}
