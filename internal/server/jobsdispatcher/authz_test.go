package jobsdispatcher

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func newTestIR() *dsl.IR {
	return &dsl.IR{
		Jobs: []dsl.Job{
			{Namespace: "vendor", Name: "ShopifyImport", VisibleTo: "vendor"},
			{Namespace: "consumer", Name: "SweepExpired", VisibleTo: "*"},
			{Namespace: "shop", Name: "Reconcile"}, // empty visible_to == permissive
		},
	}
}

func TestCheckWorkerAuthz_MatchingCaller(t *testing.T) {
	err := CheckWorkerAuthz("vendor", []string{"vendor.ShopifyImport"}, newTestIR())
	if err != nil {
		t.Errorf("vendor handling its own job should pass: %v", err)
	}
}

func TestCheckWorkerAuthz_WildcardVisibleTo(t *testing.T) {
	// visible_to "*" lets any caller handle.
	err := CheckWorkerAuthz("backstage", []string{"consumer.SweepExpired"}, newTestIR())
	if err != nil {
		t.Errorf("wildcard visible_to should pass for any caller: %v", err)
	}
}

func TestCheckWorkerAuthz_EmptyVisibleTo(t *testing.T) {
	// Empty visible_to == same permissive default as wildcard.
	err := CheckWorkerAuthz("anyone", []string{"shop.Reconcile"}, newTestIR())
	if err != nil {
		t.Errorf("empty visible_to should pass for any caller: %v", err)
	}
}

func TestCheckWorkerAuthz_MismatchedCaller(t *testing.T) {
	err := CheckWorkerAuthz("backstage", []string{"vendor.ShopifyImport"}, newTestIR())
	if err == nil {
		t.Fatal("expected PermissionDenied for caller not in visible_to")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", status.Code(err))
	}
}

func TestCheckWorkerAuthz_UnknownJob(t *testing.T) {
	err := CheckWorkerAuthz("vendor", []string{"vendor.Nonexistent"}, newTestIR())
	if err == nil {
		t.Fatal("expected NotFound for unknown job")
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %v", status.Code(err))
	}
}

func TestCheckWorkerAuthz_MixedJobsRejectFirstMismatch(t *testing.T) {
	// One in-scope + one out-of-scope. The whole Open is rejected so the
	// worker never enters the dispatch rotation for any of them.
	err := CheckWorkerAuthz("vendor", []string{"vendor.ShopifyImport", "consumer.SweepExpired"}, newTestIR())
	if err != nil {
		// Both are visible-to-vendor (wildcard), so OK.
		t.Errorf("vendor + wildcard should pass: %v", err)
	}
	err = CheckWorkerAuthz("vendor", []string{"vendor.ShopifyImport", "shop.OtherUnknown"}, newTestIR())
	if err == nil {
		t.Fatal("expected error for mixed valid+invalid jobs")
	}
}

func TestCheckWorkerAuthz_NilIR(t *testing.T) {
	err := CheckWorkerAuthz("vendor", []string{"any"}, nil)
	if err == nil {
		t.Fatal("nil IR should fail-closed")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition for nil IR, got %v", status.Code(err))
	}
}

func TestCheckSingleAuthz_Wraps(t *testing.T) {
	// Plain-error variant used at dispatch time.
	err := CheckSingleAuthz("backstage", "vendor.ShopifyImport", newTestIR())
	if err == nil {
		t.Fatal("expected error for caller mismatch")
	}
	if !strings.Contains(err.Error(), "backstage") {
		t.Errorf("error should name the caller: %v", err)
	}
}
