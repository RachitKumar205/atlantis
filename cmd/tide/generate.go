package main

import (
	"context"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	atlantiscommon "github.com/rachitkumar205/atlantis/atlantis/common"
	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// cmdGenerate is `tide generate`: fetch the canonical IR from the server,
// scope it to the namespaces this caller consumes, and emit a typed Go
// client SDK into the caller's own module (output_dir). The generated code
// belongs to the caller's repo — there is no shared central SDK and no
// dependency on a checkout of the atlantis repo.
func cmdGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	check := fs.Bool("check", false, "Verify the committed client against its manifest; write nothing")
	againstServer := fs.Bool("against-server", false, "With --check, also compare against the server's current schema")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *againstServer && !*check {
		fmt.Fprintln(os.Stderr, "tide generate: --against-server is only meaningful with --check")
		return 3
	}

	// --check reads files already on disk, so it parses tide.yaml without
	// consulting the credential store. A pipeline holding no enrolled identity
	// can still run it; see parseTideConfig.
	load := loadPCConfig
	if *check && !*againstServer {
		load = parseTideConfig
	}
	cfg, err := load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	if len(cfg.Generate) == 0 {
		fmt.Fprintln(os.Stderr, "tide generate: `generate:` in tide.yaml must list at least one namespace")
		return 3
	}
	outDir, err := validateOutputDir(cfg.OutputDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}

	modulePath, err := callerModulePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	modulePrefix := modulePath + "/" + filepath.ToSlash(outDir)

	if *check {
		return runCheck(cfg, outDir, modulePrefix, *againstServer, *timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ir, hash, err := fetchCanonicalIR(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	scoped := codegen.FilterIR(ir, cfg.Generate)
	if len(scoped.Entities) == 0 && len(scoped.Queries) == 0 && len(scoped.Procedures) == 0 {
		fmt.Fprintf(os.Stderr, "tide generate: no declarations found for namespaces %v\n", cfg.Generate)
		return 3
	}

	// Refuse a tree another caller generated. Two callers sharing an
	// output_dir accreted both their namespaces while generate only wrote;
	// now that it clears what it owns, the second would delete the first.
	if prev, err := readManifest(outDir); err == nil && prev.Caller != "" && prev.Caller != cfg.Caller {
		fmt.Fprintf(os.Stderr, "tide generate: %s holds a client generated for caller %q, "+
			"and this is %q. Give each caller its own output_dir.\n",
			outDir, prev.Caller, cfg.Caller)
		return 3
	}

	if _, err := generateSDK(scoped, outDir, modulePrefix); err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}

	// The manifest is built from what is on disk rather than from the plan,
	// so it records the .pb.go files buf produced as well.
	hashes, err := hashOwnedTree(outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	m := buildManifest(cfg.Caller, modulePrefix, version, cfg.Generate, hashes)
	if err := writeManifest(outDir, m); err != nil {
		fmt.Fprintln(os.Stderr, "tide generate:", err)
		return 3
	}
	recordGenerateProvenance(hash)

	fmt.Printf("tide: ✓ generated client for %s under %s\n",
		strings.Join(cfg.Generate, ", "), outDir)
	return 0
}

// fetchCanonicalIR returns the server's checkpoint IR and the hash it is
// stored under.
//
// The hash covers every caller's schema merged together, so it moves when an
// unrelated caller applies. It is provenance only — nothing compares it to
// decide whether a client is current.
func fetchCanonicalIR(ctx context.Context, cfg *tideConfig) (*dsl.IR, string, error) {
	client, err := dial(cfg)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetCanonicalIR(ctx, &adminpb.GetCanonicalIRRequest{})
	if err != nil {
		return nil, "", err
	}
	// The IR travels as opaque bytes rather than a structured proto message: it
	// is the input to a content hash, and google.protobuf.Struct reorders keys,
	// changing that hash for an unedited schema.
	if len(resp.GetIr()) == 0 || string(resp.GetIr()) == "null" {
		return nil, "", fmt.Errorf("server has no schema yet — run `tide apply` first")
	}
	ir, err := dsl.DecodeJSONIR(resp.GetIr())
	if err != nil {
		return nil, "", err
	}
	return ir, resp.GetContentHash(), nil
}

// planSDKFiles renders every file generate owns, keyed by a slash-separated
// path relative to output_dir.
//
// Pure apart from the embedded common protos: no server, no buf, no
// filesystem. generate writes the result; --check hashes it and compares
// against the manifest, which is why both must come from here rather than
// from two descriptions of the same thing.
//
// The .pb.go files are absent. They are produced by buf from the .proto files
// below, and a .pb.go is a function of its .proto plus the plugin versions
// pinned in buf.gen.yaml — so proto equality implies pb equality, and --check
// need not invoke buf to know it.
func planSDKFiles(ir *dsl.IR, modulePrefix string) (map[string]string, error) {
	out := map[string]string{}

	protoFiles, err := codegen.EmitProto(ir)
	if err != nil {
		return nil, fmt.Errorf("emit proto: %w", err)
	}
	customProto, err := codegen.EmitCustomProto(ir)
	if err != nil {
		return nil, fmt.Errorf("emit custom proto: %w", err)
	}
	for _, pf := range append(protoFiles, customProto...) {
		out[filepath.ToSlash(pf.Path)] = pf.Content
	}

	if err := commonProtos(out); err != nil {
		return nil, err
	}

	cfg := codegen.GenConfig{ModulePrefix: modulePrefix}
	clientFiles, err := codegen.EmitGoClient(ir, cfg)
	if err != nil {
		return nil, fmt.Errorf("emit go client: %w", err)
	}
	customClient, err := codegen.EmitCustomClient(ir, cfg)
	if err != nil {
		return nil, fmt.Errorf("emit custom client: %w", err)
	}
	for _, gf := range append(clientFiles, customClient...) {
		// Emitter paths are repo-relative (clients/go/client/<ns>/...);
		// remap onto the caller's output dir as client/<ns>/...
		rel := filepath.ToSlash(strings.TrimPrefix(gf.Path, "clients/go/"))
		src, err := format.Source([]byte(gf.Content))
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", rel, err)
		}
		out[rel] = string(src)
	}

	bufGen, bufYAML := bufConfigs(modulePrefix)
	out["buf.gen.yaml"] = bufGen
	out["buf.yaml"] = bufYAML
	return out, nil
}

// sweepOwned removes the trees generate rewrites, so an entity dropped from
// the schema does not leave its client and proto behind.
//
// Scoped to ownedRoots rather than output_dir itself: a caller may keep
// anything else in that directory. outDir must already have passed
// validateOutputDir — this deletes.
func sweepOwned(outDir string) error {
	for _, root := range ownedRoots() {
		if err := os.RemoveAll(filepath.Join(outDir, root)); err != nil {
			return fmt.Errorf("clear %s: %w", root, err)
		}
	}
	return nil
}

// generateSDK writes the planned files into outDir, then runs buf for the
// wire types. Layout under outDir: atlantis/<ns>/v1/*.proto (sources),
// pb/atlantis/<ns>/v1/*.pb.go (buf output), client/<ns>/*.go (wrappers).
func generateSDK(ir *dsl.IR, outDir, modulePrefix string) (map[string]string, error) {
	planned, err := planSDKFiles(ir, modulePrefix)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := sweepOwned(outDir); err != nil {
		return nil, err
	}
	for rel, content := range planned {
		if err := writeFile(filepath.Join(outDir, filepath.FromSlash(rel)), content); err != nil {
			return nil, err
		}
	}
	if err := runBuf(outDir); err != nil {
		return nil, err
	}
	return planned, nil
}

// commonProtos adds the embedded atlantis/common/v1 protos so
// `import "atlantis/common/v1/...";` resolves inside the caller's tree.
func commonProtos(out map[string]string) error {
	return fs.WalkDir(atlantiscommon.Protos, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := atlantiscommon.Protos.ReadFile(path)
		if err != nil {
			return err
		}
		// Embedded paths are "v1/<file>.proto"; land them under
		// atlantis/common/ to match the proto package atlantis.common.v1.
		out["atlantis/common/"+filepath.ToSlash(path)] = string(content)
		return nil
	})
}

// bufConfigs renders the single-prefix buf.gen.yaml and minimal buf.yaml so
// `buf generate` emits every proto in outDir (caller namespaces + common)
// under one Go module prefix.
//
// Plugin versions are pinned here. They decide the content of every .pb.go,
// which is why --check can compare protos alone.
func bufConfigs(modulePrefix string) (bufGen, bufYAML string) {
	bufGen = fmt.Sprintf(`# Generated by tide generate. DO NOT EDIT.
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: %s/pb
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.6
    out: pb
    opt:
      - paths=source_relative
  - remote: buf.build/grpc/go:v1.5.1
    out: pb
    opt:
      - paths=source_relative
      - require_unimplemented_servers=true
`, modulePrefix)
	return bufGen, "version: v2\n"
}

func runBuf(outDir string) error {
	if _, err := exec.LookPath("buf"); err != nil {
		return fmt.Errorf("buf not found on PATH — install it (https://buf.build/docs/installation) and re-run")
	}
	cmd := exec.Command("buf", "generate")
	cmd.Dir = outDir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buf generate: %w", err)
	}
	return nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// callerModulePath reads the `module` line from the go.mod in the current
// working directory (the caller repo root).
func callerModulePath() (string, error) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		return "", fmt.Errorf("read go.mod (run from the caller repo root): %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no module directive in go.mod")
}
