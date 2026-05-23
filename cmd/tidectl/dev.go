package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

// cmdDev — local development loop in one command.
//
// Reads atlantis.dev.yaml (the same shape as atlantis.workspace.yaml but
// pointing at working-tree filesystem paths via `source: local`), runs
// the full codegen+build pipeline, and execs the resulting
// atlantis-server with stdio piped through to the operator's terminal.
//
// The default workflow:
//
//	$ cat atlantis.dev.yaml
//	version: 1
//	callers:
//	  - name: backend
//	    source: local
//	    path: ../backend
//	    paths: [internal]
//	  - name: vendor_platform
//	    source: local
//	    path: ../vendor-platform
//	    paths: [internal]
//
//	$ PG_URL=... MEMCACHED_ADDR=... ATL_ALLOW_APPLY_MUTATION=true tidectl dev
//
// dev re-runs codegen against the working-tree .atl files (no commit
// required), rebuilds cmd/server, and starts it. Edit a .atl, Ctrl+C
// the server, re-run tidectl dev — the loop is "edit, restart, repeat."
// Hot-reload-on-change is a follow-up.
//
// Production deployments use the prod workspace manifest with
// `source: git` and pinned refs — `tidectl dev` is dev-mode only.
func cmdDev(args []string) int {
	fs := flagSet("dev")
	workspaceFile := fs.String("workspace", "atlantis.dev.yaml", "Path to the dev workspace manifest.")
	workspaceCache := fs.String("workspace-cache", ".workspace-cache", "Directory the workspace resolver writes per-caller cache into.")
	binOut := fs.String("bin", "./bin/atlantis", "Output path for the atlantis-server binary.")
	skipBuild := fs.Bool("skip-build", false, "Skip codegen + build; exec the existing binary as-is.")
	skipBuf := fs.Bool("skip-buf", false, "Skip the buf lint + buf generate steps. Useful when the proto tree is already current.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*skipBuild {
		if _, err := os.Stat(*workspaceFile); err != nil {
			fmt.Fprintf(os.Stderr, "tidectl dev: %s not found\n", *workspaceFile)
			fmt.Fprintln(os.Stderr, "create one with:")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  version: 1")
			fmt.Fprintln(os.Stderr, "  callers:")
			fmt.Fprintln(os.Stderr, "    - name: backend")
			fmt.Fprintln(os.Stderr, "      source: local")
			fmt.Fprintln(os.Stderr, "      path: ../backend")
			fmt.Fprintln(os.Stderr, "      paths: [internal]")
			return 2
		}
		if rc := cmdCodegen([]string{
			"--workspace=" + *workspaceFile,
			"--workspace-cache=" + *workspaceCache,
		}); rc != 0 {
			return rc
		}
		// Re-run buf so generated pb / client / namespace protos reflect
		// the freshly-emitted .proto files. The codegen step above only
		// writes the .proto sources; the typed Go surface comes from
		// `buf generate`. Skip if the operator says so — e.g., they
		// already ran it manually and just want to rebuild the server.
		if !*skipBuf {
			if err := runPassthrough("buf", "lint"); err != nil {
				fmt.Fprintln(os.Stderr, "tidectl dev:", err)
				return 1
			}
			if err := runPassthrough("buf", "generate"); err != nil {
				fmt.Fprintln(os.Stderr, "tidectl dev:", err)
				return 1
			}
		}
		fmt.Fprintf(os.Stderr, "tidectl dev: go build -> %s\n", *binOut)
		if err := runPassthrough("go", "build", "-o", *binOut, "./cmd/server"); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl dev: build failed:", err)
			return 1
		}
	}

	if _, err := os.Stat(*binOut); err != nil {
		fmt.Fprintf(os.Stderr, "tidectl dev: %s missing (re-run without --skip-build)\n", *binOut)
		return 1
	}
	return execServer(*binOut)
}

// execServer launches the atlantis-server binary with stdio passthrough
// and forwards SIGINT / SIGTERM so Ctrl+C in the operator's terminal
// produces a clean graceful shutdown (the server's outbox-drain logic
// runs on SIGTERM). Returns the child's exit code; non-zero on error.
func execServer(bin string) int {
	abs, err := filepath.Abs(bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl dev: resolve bin path:", err)
		return 1
	}
	cmd := exec.Command(abs)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "tidectl dev: start server:", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case sig := <-sigs:
			// Forward the signal; the server's own handler runs the
			// graceful-shutdown path. We loop because a Ctrl+C followed
			// by an impatient second Ctrl+C should still pass through.
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		case err := <-done:
			if err == nil {
				return 0
			}
			// An exit code of N from the child shows up as ExitError;
			// surface it so wrappers (CI, supervisord) see the code.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "tidectl dev: server exited:", err)
			return 1
		}
	}
}

// runPassthrough runs an external command with the operator's terminal
// streams attached. We don't capture output — the build / buf logs are
// useful in real time, and a captured-and-replayed log would interleave
// poorly with the progress lines tidectl already prints.
func runPassthrough(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}
