-- caller_registrations: latest .atl file each caller has submitted.
-- ir_checkpoint: the last successfully-applied merged IR (single row).
-- caller is the mTLS CN (or x-caller header in dev).

CREATE TABLE IF NOT EXISTS atlantis.caller_registrations (
    caller       TEXT      NOT NULL,
    file_path    TEXT      NOT NULL,
    content      TEXT      NOT NULL,
    sha256       TEXT      NOT NULL,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (caller, file_path)
);

CREATE INDEX IF NOT EXISTS caller_registrations_caller_idx
    ON atlantis.caller_registrations (caller);

-- Singleton IR checkpoint (CHECK id=1). applied_at is used to reject
-- concurrent applies of an older plan.
CREATE TABLE IF NOT EXISTS atlantis.ir_checkpoint (
    id           INT       NOT NULL PRIMARY KEY DEFAULT 1,
    ir           JSONB     NOT NULL,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_by   TEXT      NOT NULL,
    CHECK (id = 1)
);
