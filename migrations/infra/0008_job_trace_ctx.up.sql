-- W3C traceparent propagation through the job runtime.
--
-- SubmitJob captures the active OTel span into trace_ctx as a JSON
-- object (`{"traceparent":"00-...","tracestate":"..."}`) so the
-- worker can resume the trace when it claims the row. The column is
-- NULLable: callers without OTel instrumentation (tests, CLI
-- submissions) leave it NULL and the worker starts a root span.
ALTER TABLE atlantis.jobs
    ADD COLUMN IF NOT EXISTS trace_ctx JSONB;
