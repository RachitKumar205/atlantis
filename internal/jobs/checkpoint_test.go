package jobs

import (
	"context"
	"testing"
)

func TestCheckpoint_NoOpWithoutContext(t *testing.T) {
	// Checkpoint called with a bare context (no checkpointer attached)
	// must succeed silently. This is the "running in a unit test"
	// path; the worker installs a real checkpointer in production.
	if err := Checkpoint(context.Background(), 50, "halfway"); err != nil {
		t.Fatalf("expected nil error for no-op Checkpoint, got: %v", err)
	}
}

func TestCheckpoint_ClampOutOfRange(t *testing.T) {
	// Checkpointer.Report clamps pct out of range so a sloppy handler
	// call doesn't fail an otherwise-healthy job. We can't easily
	// exercise the SQL path without a live pool, so just confirm the
	// clamping logic doesn't crash on extreme values via the no-op
	// path (which still runs the clamp before short-circuiting).
	for _, pct := range []int{-100, 0, 50, 100, 200} {
		if err := Checkpoint(context.Background(), pct, "test"); err != nil {
			t.Errorf("pct=%d unexpected error: %v", pct, err)
		}
	}
}
