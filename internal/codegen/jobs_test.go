package codegen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func parseToIR(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("dsl.Parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("dsl.Lower: %v", err)
	}
	return ir
}

func TestEmitJobsHandlers_EmptyIR(t *testing.T) {
	files, err := EmitJobsHandlers(&dsl.IR{})
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected zero files for empty IR, got %d", len(files))
	}
}

func TestEmitJobsHandlers_OneJob(t *testing.T) {
	ir := parseToIR(t, `
job BulkImport in vendor {
  args {
    vendor_id       varchar(7) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries 3
  timeout 30m
}
`)
	files, err := EmitJobsHandlers(ir)
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	// "vendor" collides with go module name; the codegen remaps to
	// "vendorpkg" so the package directory doesn't conflict with the
	// caller's go-modules vendor dir.
	if f.Path != "gen/go/server/vendorpkg/jobs.go" {
		t.Errorf("path = %q", f.Path)
	}
	c := f.Content
	for _, want := range []string{
		"package vendorpkg",
		"type BulkImportArgs struct",
		"VendorId",
		"ImportStrategy",
		"`json:\"vendor_id\"`",
		"`json:\"import_strategy\"`",
		"type BulkImportHandler interface",
		"Handle(ctx context.Context, args BulkImportArgs) error",
		`const BulkImportJobName = "vendor.BulkImport"`,
		"func RegisterBulkImport(reg *jobs.Registry, h BulkImportHandler)",
		"jobs.HandlerFunc",
		"json.Unmarshal(argsJSON, &args)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing substring %q in output:\n%s", want, c)
		}
	}
}

func TestEmitJobsHandlers_ParsesAsGo(t *testing.T) {
	ir := parseToIR(t, `
job BulkImport in vendor {
  args {
    vendor_id varchar(7) not null
  }
}
job CleanCart in consumer {
  args {
    cart_id varchar(8) not null
  }
}
`)
	files, err := EmitJobsHandlers(ir)
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	for _, f := range files {
		fset := token.NewFileSet()
		_, err := parser.ParseFile(fset, f.Path, f.Content, parser.AllErrors)
		if err != nil {
			t.Errorf("Go parse of %s failed: %v\n---\n%s", f.Path, err, f.Content)
		}
	}
}

func TestEmitJobsHandlers_NoArgs(t *testing.T) {
	// Zero-arg jobs are legal — the args block can be omitted entirely.
	// The emitted Args struct must still be present (callers may use
	// it for type inference) and the unmarshal path must accept empty
	// JSON without erroring.
	ir := parseToIR(t, `job Heartbeat in atlantis {}`)
	files, err := EmitJobsHandlers(ir)
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	c := files[0].Content
	if !strings.Contains(c, "type HeartbeatArgs struct {") {
		t.Errorf("expected empty Args struct in output, got:\n%s", c)
	}
	if !strings.Contains(c, "if len(argsJSON) > 0 {") {
		t.Errorf("expected zero-arg guard in registration wrapper")
	}
}

func TestEmitJobsHandlers_TimeImport(t *testing.T) {
	ir := parseToIR(t, `
job AuditPurge in atlantis {
  args {
    cutoff timestamptz not null
  }
}
`)
	files, err := EmitJobsHandlers(ir)
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	c := files[0].Content
	if !strings.Contains(c, `"time"`) {
		t.Errorf("expected time import in output (timestamptz arg)")
	}
	if !strings.Contains(c, "Cutoff") {
		t.Errorf("expected Cutoff field in output")
	}
}

func TestEmitJobsHandlers_PerNamespaceStableOrdering(t *testing.T) {
	// Verifies that two declarations in the same namespace land in
	// sorted order so the emitted file is stable across reruns.
	ir := parseToIR(t, `
job ZetaJob in vendor { args { x int not null } }
job AlphaJob in vendor { args { y int not null } }
`)
	files, err := EmitJobsHandlers(ir)
	if err != nil {
		t.Fatalf("EmitJobsHandlers: %v", err)
	}
	c := files[0].Content
	alphaIdx := strings.Index(c, "type AlphaJobArgs")
	zetaIdx := strings.Index(c, "type ZetaJobArgs")
	if alphaIdx < 0 || zetaIdx < 0 || alphaIdx >= zetaIdx {
		t.Errorf("expected AlphaJob before ZetaJob in output (got alpha=%d zeta=%d)", alphaIdx, zetaIdx)
	}
}

func TestPascalCaseJobField(t *testing.T) {
	cases := map[string]string{
		"vendor_id":              "VendorId",
		"import_strategy":        "ImportStrategy",
		"x":                      "X",
		"vendor_fulfillment_id": "VendorFulfillmentId",
	}
	for in, want := range cases {
		got := pascalCaseJobField(in)
		if got != want {
			t.Errorf("pascalCaseJobField(%q) = %q, want %q", in, got, want)
		}
	}
}
