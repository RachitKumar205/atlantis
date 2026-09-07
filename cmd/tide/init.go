package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/cliflag"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdInit is `tide init`: write the tide.yaml a caller repository needs.
//
// The file it writes is the whole of what a repository configures — the
// caller name, which files are schema, and where a generated client goes.
// Everything else (the address, the credential) comes from `tide login`.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	caller := fs.String("caller", "", "this repository's caller name (required)")
	org := fs.String("org", "", "organisation, needed only when more than one is enrolled")
	schemaPath := fs.String("schema-path", ".", "where the .atl files live")
	outputDir := fs.String("output-dir", "", "where `tide generate` writes the typed client")
	generate := fs.String("generate", "", "comma-separated namespaces `tide generate` covers (defaults to the caller name when --output-dir is set)")
	language := fs.String("language", "", "client language: go (default) or python")
	if err := cliflag.ParseNoArgs(fs, args); err != nil {
		return 3
	}
	lang, err := resolveLanguage(*language)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide init:", err)
		return 2
	}
	if *caller == "" {
		fmt.Fprintln(os.Stderr, "tide init: --caller is required — it names this repository at atlantis")
		return 2
	}
	if !validStoreName(*caller) {
		fmt.Fprintf(os.Stderr, "tide init: %q is not a valid caller name "+
			"(lowercase letters, digits and interior hyphens)\n", *caller)
		return 2
	}
	if *org != "" && !validStoreName(*org) {
		fmt.Fprintf(os.Stderr, "tide init: %q is not a valid organisation name\n", *org)
		return 2
	}
	if _, err := os.Stat("tide.yaml"); err == nil {
		fmt.Fprintln(os.Stderr, "tide init: tide.yaml already exists; edit it rather than regenerating it")
		return 2
	}

	content := renderTideYAML(*caller, *org, *schemaPath, *outputDir, *generate, lang)
	if err := os.WriteFile("tide.yaml", []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "tide init:", err)
		return 3
	}

	cliout.Header(os.Stdout, "tide.yaml")
	cliout.Row(os.Stdout, "brass", *caller, "this repository's caller")
	cliout.Row(os.Stdout, "muted", "schema", *schemaPath)
	if *outputDir != "" {
		cliout.Row(os.Stdout, "muted", "client", *outputDir)
		cliout.Row(os.Stdout, "muted", "language", string(lang))
	}
	fmt.Println()
	if lang == langPython && *outputDir != "" {
		printPythonSourceRootNote(*outputDir)
	}
	fmt.Println("Next: `tide login`, then declare an entity and `tide plan`.")
	return 0
}

// printPythonSourceRootNote says what output_dir has to be for Python.
//
// protoc derives a module path from the proto path, so the generated
// `atlantis` package is importable only when output_dir is itself on the
// Python path. There is no flag that changes this, and the failure without it
// is a ModuleNotFoundError in the caller's own code rather than anything
// `tide generate` reports — so it is said once, here, while the path is being
// chosen.
func printPythonSourceRootNote(outputDir string) {
	fmt.Printf("%s must be a Python source root: the generated code imports\n", outputDir)
	fmt.Println("`atlantis.<namespace>.v1`, and protoc fixes that path. In pyproject.toml:")
	fmt.Println()
	fmt.Println("    [tool.hatch.build.targets.wheel]")
	fmt.Printf("    packages = [\"%s/atlantis\"]\n", outputDir)
	fmt.Println()
	fmt.Println("Install the runtime with: pip install atlantis-client")
	fmt.Println()
}

// renderTideYAML builds the file. Optional fields are written as commented
// guidance rather than omitted, so the file itself says what else it can
// carry.
func renderTideYAML(caller, org, schemaPath, outputDir, generate string, lang sdkLanguage) string {
	var b strings.Builder
	b.WriteString("# This repository, as atlantis sees it. The address and the credential are\n")
	b.WriteString("# not configured here — `tide login` collects both.\n")
	fmt.Fprintf(&b, "caller: %s\n", caller)
	if org != "" {
		fmt.Fprintf(&b, "org: %s\n", org)
	} else {
		b.WriteString("# org: acme            # needed only when more than one organisation is enrolled\n")
	}
	b.WriteString("schema_paths:\n")
	fmt.Fprintf(&b, "  - %s\n", schemaPath)

	if outputDir == "" {
		b.WriteString("\n# Where `tide generate` writes the typed client, and which namespaces it\n")
		b.WriteString("# covers. Uncomment when you generate one; commit the result.\n")
		b.WriteString("# output_dir: internal/atlantis\n")
		b.WriteString("# generate:\n")
		fmt.Fprintf(&b, "#   - %s\n", caller)
		b.WriteString("# language: go         # or python\n")
		return b.String()
	}

	fmt.Fprintf(&b, "output_dir: %s\n", outputDir)
	// Written only for Python. Go is the default, so spelling it out would put
	// a line in every existing shape of this file that changes nothing.
	if lang != defaultLanguage {
		fmt.Fprintf(&b, "language: %s\n", lang)
	}
	b.WriteString("generate:\n")
	namespaces := strings.Split(generate, ",")
	if generate == "" {
		namespaces = []string{caller}
	}
	for _, ns := range namespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			fmt.Fprintf(&b, "  - %s\n", ns)
		}
	}
	return b.String()
}
