package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The file init writes must parse as the config every other command reads —
// a scaffold that loadPCConfig refuses is worse than none.
func TestInitWritesAConfigTideCanRead(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if code := cmdInit([]string{"--caller", "api"}); code != 0 {
		t.Fatalf("init exited %d", code)
	}
	cfg, err := parseTideConfig("tide.yaml")
	if err != nil {
		t.Fatalf("the scaffold does not parse: %v", err)
	}
	if cfg.Caller != "api" {
		t.Errorf("caller = %q", cfg.Caller)
	}
	if len(cfg.SchemaPaths) != 1 || cfg.SchemaPaths[0] != "." {
		t.Errorf("schema_paths = %v", cfg.SchemaPaths)
	}
	// The optional half is guidance, not configuration, until asked for.
	if cfg.OutputDir != "" || len(cfg.Generate) != 0 {
		t.Errorf("a bare init configured generation: %q %v", cfg.OutputDir, cfg.Generate)
	}

	if code := cmdInit([]string{"--caller", "api"}); code == 0 {
		t.Error("init overwrote an existing tide.yaml")
	}
}

func TestInitWithGenerationFillsBothHalves(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if code := cmdInit([]string{"--caller", "api", "--org", "acme",
		"--output-dir", "internal/atlantis", "--generate", "shop, billing"}); code != 0 {
		t.Fatalf("init exited %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tide.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg tideConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Org != "acme" || cfg.OutputDir != "internal/atlantis" {
		t.Errorf("org=%q output_dir=%q", cfg.Org, cfg.OutputDir)
	}
	if strings.Join(cfg.Generate, ",") != "shop,billing" {
		t.Errorf("generate = %v", cfg.Generate)
	}
}

func TestInitRefusesABadCallerName(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	_ = os.Chdir(dir)

	for _, bad := range []string{"", "Has Upper", "trailing-", "../escape"} {
		if code := cmdInit([]string{"--caller", bad}); code == 0 {
			t.Errorf("caller %q was accepted", bad)
		}
	}
}
