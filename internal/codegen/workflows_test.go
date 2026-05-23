package codegen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestEmitWorkflows_Empty(t *testing.T) {
	files, err := EmitWorkflows(parseToIR(t, ``))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestEmitWorkflows_OneWorkflow(t *testing.T) {
	ir := parseToIR(t, `
job ShopifyImport in vendor { args { vendor_id varchar(7) not null } }
job Cleanup in vendor { args { vendor_id varchar(7) not null } }
workflow Onboard in vendor {
  state { vendor_id varchar(7) not null }
  step setup { job ShopifyImport args { vendor_id: $vendor_id } }
  compensate setup { job Cleanup args { vendor_id: $vendor_id } }
}
`)
	files, err := EmitWorkflows(ir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	c := files[0].Content
	for _, want := range []string{
		"type OnboardState struct",
		"VendorId",
		`const OnboardWorkflowName = "vendor.Onboard"`,
		`OnboardStepSetup = "setup"`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestEmitWorkflows_ParsesAsGo(t *testing.T) {
	ir := parseToIR(t, `
job J in vendor { args { x int not null } }
workflow W in vendor {
  state { x int not null }
  step s1 { job J args { x: $x } }
}
`)
	files, err := EmitWorkflows(ir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, f.Path, f.Content, parser.AllErrors); err != nil {
			t.Errorf("Go parse of %s failed: %v\n%s", f.Path, err, f.Content)
		}
	}
}
