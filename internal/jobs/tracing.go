package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traceCtxPayload is the JSON shape stored in atlantis.jobs.trace_ctx.
// Mirrors the W3C Trace Context headers so any OTel-compatible backend
// (Jaeger, Tempo, Datadog) can stitch the submit-side and worker-side
// spans into one distributed trace.
type traceCtxPayload struct {
	Traceparent string `json:"traceparent,omitempty"`
	Tracestate  string `json:"tracestate,omitempty"`
}

// CaptureTraceCtx extracts the active span's W3C traceparent +
// tracestate from ctx and returns it as JSON suitable for the
// trace_ctx column. Returns nil when no span is active (the caller
// has no OTel instrumentation) so the INSERT passes NULL.
func CaptureTraceCtx(ctx context.Context) json.RawMessage {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	tp := carrier.Get("traceparent")
	ts := carrier.Get("tracestate")
	if tp == "" {
		return nil
	}
	raw, err := json.Marshal(traceCtxPayload{Traceparent: tp, Tracestate: ts})
	if err != nil {
		return nil
	}
	return raw
}

// ResumeTraceCtx parses a trace_ctx JSON payload (from the claimed
// row) and returns a ctx carrying the remote parent span. The worker
// creates a child span under this parent so the handler's work shows
// as a continuation of the submit-side trace. Returns the original
// ctx unchanged when traceCtxJSON is nil or invalid (graceful
// degradation: the handler runs without trace linkage rather than
// failing).
func ResumeTraceCtx(ctx context.Context, traceCtxJSON []byte) context.Context {
	if len(traceCtxJSON) == 0 {
		return ctx
	}
	var payload traceCtxPayload
	if err := json.Unmarshal(traceCtxJSON, &payload); err != nil {
		return ctx
	}
	if payload.Traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{
		"traceparent": payload.Traceparent,
		"tracestate":  payload.Tracestate,
	}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}

// StartWorkerSpan creates a child span named "jobs.handle <jobName>"
// under the parent extracted by ResumeTraceCtx. Returns the wrapped
// ctx + a finish func the caller must defer. When no TracerProvider
// is configured (the common dev case), OTel's no-op tracer makes
// this free.
func StartWorkerSpan(ctx context.Context, jobName string) (context.Context, func()) {
	tracer := otel.Tracer("atlantis.jobs")
	ctx, span := tracer.Start(ctx, fmt.Sprintf("jobs.handle %s", jobName),
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	return ctx, func() { span.End() }
}
