package dsl

import "testing"

func TestParse_Job_TimeoutNone(t *testing.T) {
	f := mustParse(t, `
job LongRun in vendor {
  args { x int not null }
  timeout none
}
`)
	jd := f.Decls[0].(*JobDecl)
	if jd.Timeout == nil || jd.Timeout.Duration != "none" {
		t.Fatalf("expected timeout=none AST, got %+v", jd.Timeout)
	}
}

func TestLower_Job_TimeoutNone(t *testing.T) {
	f := mustParse(t, `
job LongRun in vendor {
  args { x int not null }
  timeout none
}
`)
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if ir.Jobs[0].TimeoutMS != 0 || !ir.Jobs[0].TimeoutNone {
		t.Errorf("expected TimeoutNone=true TimeoutMS=0, got TimeoutMS=%d TimeoutNone=%v", ir.Jobs[0].TimeoutMS, ir.Jobs[0].TimeoutNone)
	}
}

func TestLower_Job_TimeoutDuration(t *testing.T) {
	f := mustParse(t, `
job Quick in vendor {
  args { x int not null }
  timeout 30m
}
`)
	ir, _ := Lower([]*File{f})
	if ir.Jobs[0].TimeoutNone {
		t.Errorf("expected TimeoutNone=false for declared duration")
	}
	if ir.Jobs[0].TimeoutMS != 1_800_000 {
		t.Errorf("expected timeout_ms=1800000, got %d", ir.Jobs[0].TimeoutMS)
	}
}
