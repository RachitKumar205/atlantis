package dsl

import (
	"strings"
	"testing"
)

func TestParse_Enqueue_BasicStep(t *testing.T) {
	f := mustParse(t, `
job BulkImport in vendor {
  args { vendor_id varchar(7) not null }
}
entity Vendor in vendor { id varchar(7) primary }
procedure TriggerImport for vendor.Vendor {
  input { vendor_id: varchar(7) }
  steps {
    enqueue vendor.BulkImport(vendor_id: $vendor_id)
  }
}
`)
	if len(f.Decls) != 3 {
		t.Fatalf("expected 3 decls, got %d", len(f.Decls))
	}
	pd, ok := f.Decls[2].(*ProcedureDecl)
	if !ok {
		t.Fatalf("expected ProcedureDecl, got %T", f.Decls[2])
	}
	if len(pd.Steps) != 1 || pd.Steps[0].Enqueue == nil {
		t.Fatalf("expected one enqueue step, got %+v", pd.Steps)
	}
	eq := pd.Steps[0].Enqueue
	if eq.Target.Namespace != "vendor" || eq.Target.Name != "BulkImport" {
		t.Errorf("target = %+v", eq.Target)
	}
	if len(eq.Args) != 1 || eq.Args[0].Name != "vendor_id" {
		t.Errorf("args = %+v", eq.Args)
	}
}

func TestParse_Enqueue_NoArgs(t *testing.T) {
	f := mustParse(t, `
job Sweep in vendor {}
entity Vendor in vendor { id varchar(7) primary }
procedure DoSweep for vendor.Vendor {
  input { }
  steps {
    enqueue vendor.Sweep()
  }
}
`)
	pd := f.Decls[2].(*ProcedureDecl)
	if pd.Steps[0].Enqueue == nil || len(pd.Steps[0].Enqueue.Args) != 0 {
		t.Fatalf("expected empty-arg enqueue, got %+v", pd.Steps[0])
	}
}

func TestLower_Enqueue_Resolves(t *testing.T) {
	f := mustParse(t, `
job BulkImport in vendor {
  args { vendor_id varchar(7) not null }
  retries 3
  timeout 30m
  queue "ingestion"
}
entity Vendor in vendor { id varchar(7) primary }
procedure TriggerImport for vendor.Vendor {
  input { vendor_id: varchar(7) }
  steps {
    enqueue vendor.BulkImport(vendor_id: $vendor_id)
  }
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(ir.Procedures) != 1 {
		t.Fatalf("expected 1 procedure")
	}
	step := ir.Procedures[0].Steps[0]
	if step.Enqueue == nil {
		t.Fatalf("expected enqueue step")
	}
	if step.Enqueue.TargetJobID != "vendor.BulkImport" {
		t.Errorf("target = %s", step.Enqueue.TargetJobID)
	}
	if step.Enqueue.Queue != "ingestion" {
		t.Errorf("queue = %s", step.Enqueue.Queue)
	}
	if step.Enqueue.MaxRetries != 3 {
		t.Errorf("retries = %d", step.Enqueue.MaxRetries)
	}
	if step.Enqueue.TimeoutMS != 1_800_000 {
		t.Errorf("timeout_ms = %d", step.Enqueue.TimeoutMS)
	}
	if len(step.Enqueue.Args) != 1 || step.Enqueue.Args[0].Name != "vendor_id" {
		t.Errorf("args = %+v", step.Enqueue.Args)
	}
	if step.Enqueue.Args[0].Value == nil || step.Enqueue.Args[0].Value.Kind != ExprArg || step.Enqueue.Args[0].Value.ArgName != "vendor_id" {
		t.Errorf("arg value = %+v", step.Enqueue.Args[0].Value)
	}
}

func TestLower_Enqueue_RejectsUnknownJob(t *testing.T) {
	f := mustParse(t, `
entity Vendor in vendor { id varchar(7) primary }
procedure P for vendor.Vendor {
  input { }
  steps { enqueue vendor.DoesNotExist() }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "unknown job vendor.DoesNotExist") {
		t.Fatalf("expected unknown-job error, got: %v", err)
	}
}

func TestLower_Enqueue_RejectsUnknownArg(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
entity Vendor in vendor { id varchar(7) primary }
procedure P for vendor.Vendor {
  input { y: int }
  steps { enqueue vendor.J(y: $y) }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), `arg "y" is not declared on job vendor.J`) {
		t.Fatalf("expected unknown-arg error, got: %v", err)
	}
}

func TestLower_Enqueue_RejectsMissingRequiredArg(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
entity Vendor in vendor { id varchar(7) primary }
procedure P for vendor.Vendor {
  input { }
  steps { enqueue vendor.J() }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), `missing required arg "x"`) {
		t.Fatalf("expected missing-arg error, got: %v", err)
	}
}

func TestLower_Enqueue_AllowsMissingOptionalArg(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int default 0 } }
entity Vendor in vendor { id varchar(7) primary }
procedure P for vendor.Vendor {
  input { }
  steps { enqueue vendor.J() }
}
`)
	_, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("expected nullable+default arg to be optional, got: %v", err)
	}
}

func TestLower_Enqueue_RejectsDuplicateArg(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
entity Vendor in vendor { id varchar(7) primary }
procedure P for vendor.Vendor {
  input { a: int, b: int }
  steps { enqueue vendor.J(x: $a, x: $b) }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), `duplicate enqueue arg "x"`) {
		t.Fatalf("expected duplicate-arg error, got: %v", err)
	}
}

func TestLower_Enqueue_RejectsFieldRef(t *testing.T) {
	f := mustParse(t, `
job J in vendor { args { x int not null } }
entity Vendor in vendor {
  id varchar(7) primary
  x  int not null
}
procedure P for vendor.Vendor {
  input { }
  steps { enqueue vendor.J(x: x) }
}
`)
	_, err := Lower([]*File{f})
	if err == nil || !strings.Contains(err.Error(), "cannot reference a row field") {
		t.Fatalf("expected field-ref rejection, got: %v", err)
	}
}
