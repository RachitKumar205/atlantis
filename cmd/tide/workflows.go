package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliflag"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

func cmdWorkflow(args []string) int {
	if len(args) < 1 {
		printWorkflowUsage()
		return 2
	}
	switch args[0] {
	case "start":
		return cmdWorkflowStart(args[1:])
	case "status":
		return cmdWorkflowStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide workflow: unknown subcommand %q\n", args[0])
		printWorkflowUsage()
		return 2
	}
}

func printWorkflowUsage() {
	fmt.Fprintln(os.Stderr, "usage: tide workflow start <workflow-name> [--state=JSON]")
	fmt.Fprintln(os.Stderr, "       tide workflow status <workflow-id>")
}

func cmdWorkflowStart(args []string) int {
	fs := flag.NewFlagSet("workflow start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	stateJSON := fs.String("state", "{}", "State as a JSON object")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	pos, err := cliflag.Parse(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "tide workflow start: missing workflow-name")
		return 2
	}
	wfName := pos[0]

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	principal := os.Getenv("USER")
	if principal == "" {
		principal = "tide"
	}
	// Validate locally before the round trip. The old client typed this
	// json.RawMessage, and encoding/json refused to marshal invalid JSON, so a
	// typo failed here with a parse error. Sending the bytes raw would turn
	// that into a JSONB error from the database, several layers away from the
	// flag that caused it. cmdJobSubmit validates --args the same way.
	if !json.Valid([]byte(*stateJSON)) {
		fmt.Fprintf(os.Stderr, "tide workflow start: --state is not valid JSON: %s\n", *stateJSON)
		return 3
	}
	resp, err := client.StartWorkflow(ctx, &adminpb.StartWorkflowRequest{
		WorkflowName: wfName,
		State:        []byte(*stateJSON),
		SubmittedBy:  "cli:" + principal,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide workflow start:", err)
		return 3
	}
	cliout.Successf("started %s as workflow %s", cliout.Bold(wfName), cliout.Bold(resp.GetWorkflowId()))
	fmt.Printf("       monitor with: %s\n", cliout.Bold("tide workflow status "+resp.GetWorkflowId()))
	return 0
}

func cmdWorkflowStatus(args []string) int {
	fs := flag.NewFlagSet("workflow status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	pos, err := cliflag.Parse(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "tide workflow status: missing workflow-id")
		return 2
	}
	wfID := pos[0]

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetWorkflowStatus(ctx, &adminpb.GetWorkflowStatusRequest{WorkflowId: wfID})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide workflow status:", err)
		return 3
	}
	if !resp.GetFound() {
		cliout.Errorf("workflow %s not found", wfID)
		return 1
	}
	switch *format {
	case "json":
		if err := emitJSON(resp.GetWorkflow(), "state"); err != nil {
			fmt.Fprintln(os.Stderr, "tide workflow status:", err)
			return 3
		}
	case "table":
		printWorkflowStatusRow(resp.GetWorkflow())
	}
	if resp.GetWorkflow().GetStatus() == "failed" {
		return 1
	}
	return 0
}

func printWorkflowStatusRow(w *adminpb.WorkflowStatus) {
	fmt.Printf("%s       %s\n", cliout.Grey("workflow-id"), cliout.Bold(w.GetWorkflowId()))
	fmt.Printf("%s     %s\n", cliout.Grey("workflow-name"), w.GetWorkflowName())
	fmt.Printf("%s           %s\n", cliout.Grey("status"), colorWorkflowStatus(w.GetStatus()))
	if w.GetCurrentStep() != "" {
		fmt.Printf("%s     %s\n", cliout.Grey("current-step"), cliout.Cyan(w.GetCurrentStep()))
	}
	fmt.Printf("%s          %s\n", cliout.Grey("started"), w.GetStartedAt())
	if w.GetCompletedAt() != "" {
		fmt.Printf("%s        %s\n", cliout.Grey("completed"), w.GetCompletedAt())
	}
	if w.GetErrorMsg() != "" {
		fmt.Printf("%s            %s\n", cliout.Red("error"), cliout.Red(w.GetErrorMsg()))
	}
}

func colorWorkflowStatus(s string) string {
	switch s {
	case "complete":
		return cliout.Green(s)
	case "running", "completing", "compensating":
		return cliout.Yellow(s)
	case "failed", "cancelled":
		return cliout.Red(cliout.Bold(s))
	}
	return s
}
