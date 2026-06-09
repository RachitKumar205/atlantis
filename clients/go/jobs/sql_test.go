package jobs

import (
	"strings"
	"testing"
)

// TestBuildClaimSQL_StampsProvenanceColumns pins the contract that
// the claim CTE writes both worker_kind and worker_session_id. A
// regression here would silently land jobs without provenance and
// confuse the dispatcher's session-disconnect cleanup (which relies
// on the partial index keyed on worker_session_id).
func TestBuildClaimSQL_StampsProvenanceColumns(t *testing.T) {
	for _, tc := range []struct {
		name           string
		jobNamesFilter bool
	}{
		{"no_jobname_filter", false},
		{"with_jobname_filter", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := buildClaimSQL(tc.jobNamesFilter)
			for _, needle := range []string{
				"worker_kind       = $5",
				"worker_session_id = NULLIF($6, '')",
				"FOR UPDATE SKIP LOCKED",
				"status            = 'running'",
				"attempts          = j.attempts + 1",
			} {
				if !strings.Contains(sql, needle) {
					t.Errorf("claim SQL missing %q", needle)
				}
			}
		})
	}
}

// TestBuildClaimSQL_JobNamesFilter pins that the optional job_name
// allowlist is rendered as a bound parameter (not interpolated) and
// only when requested. The dispatcher relies on the filter form;
// the direct-PG worker relies on the unfiltered form.
func TestBuildClaimSQL_JobNamesFilter(t *testing.T) {
	with := buildClaimSQL(true)
	without := buildClaimSQL(false)
	if !strings.Contains(with, "job_name = ANY($7::text[])") {
		t.Errorf("filtered claim SQL missing job_name allowlist clause")
	}
	if strings.Contains(without, "$7") {
		t.Errorf("unfiltered claim SQL should not reference $7")
	}
	if strings.Contains(without, "job_name = ANY") {
		t.Errorf("unfiltered claim SQL should not reference job_name allowlist")
	}
}

// TestWorkerKindConstants pins the two stamp values so a typo on
// either side (SDK Worker vs server dispatcher) shows up at build
// time. PR 2 will add a mirror assertion on the dispatcher side.
func TestWorkerKindConstants(t *testing.T) {
	if WorkerKindDirectPG != "direct_pg" {
		t.Errorf("WorkerKindDirectPG drifted: got %q", WorkerKindDirectPG)
	}
	if WorkerKindDispatched != "dispatched" {
		t.Errorf("WorkerKindDispatched drifted: got %q", WorkerKindDispatched)
	}
}

// TestIsSafeIdent guards the LISTEN identifier validator that
// PgListen uses. Any allowed character set widening must add a
// matching test here to document the intent.
func TestIsSafeIdent(t *testing.T) {
	good := []string{"atl_jobs", "atl", "Atl_Jobs_42", "a", strings.Repeat("a", 63)}
	bad := []string{"", "atl jobs", "atl-jobs", "atl;DROP", "atl'", strings.Repeat("a", 64)}
	for _, s := range good {
		if !isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = true, want false", s)
		}
	}
}
