package jobs

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCaptureTraceCtx_NoSpan(t *testing.T) {
	raw := CaptureTraceCtx(context.Background())
	if raw != nil {
		t.Fatalf("expected nil for context without span, got %s", string(raw))
	}
}

func TestResumeTraceCtx_Nil(t *testing.T) {
	ctx := ResumeTraceCtx(context.Background(), nil)
	if ctx == nil {
		t.Fatal("expected non-nil ctx")
	}
}

func TestResumeTraceCtx_InvalidJSON(t *testing.T) {
	ctx := ResumeTraceCtx(context.Background(), []byte("not json"))
	if ctx == nil {
		t.Fatal("expected non-nil ctx on invalid JSON")
	}
}

func TestResumeTraceCtx_ValidPayload(t *testing.T) {
	payload := traceCtxPayload{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "congo=t61rcWkgMzE",
	}
	raw, _ := json.Marshal(payload)
	ctx := ResumeTraceCtx(context.Background(), raw)
	if ctx == nil {
		t.Fatal("expected non-nil ctx")
	}
}

func TestStartWorkerSpan_NoProvider(t *testing.T) {
	ctx, end := StartWorkerSpan(context.Background(), "test.Job")
	defer end()
	if ctx == nil {
		t.Fatal("expected non-nil ctx")
	}
}
