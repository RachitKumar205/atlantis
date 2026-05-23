-- adopt_history records every successful `tidectl adopt` against this
-- server. An append-only audit so SREs can see which caller baselined
-- when, against which declared-IR hash, and whether `--allow-drift`
-- was used. When drift was accepted, the drift report is stored
-- verbatim so the lie is reviewable months later.
CREATE TABLE IF NOT EXISTS atlantis.adopt_history (
    id              BIGSERIAL PRIMARY KEY,
    caller          TEXT NOT NULL,
    declared_hash   TEXT NOT NULL,                   -- sha256 of submitted IR
    drift_count     INTEGER NOT NULL,
    drift_report    JSONB NOT NULL DEFAULT '[]'::jsonb,
    allow_drift     BOOLEAN NOT NULL,
    adopted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    adopted_by      TEXT NOT NULL                    -- principal that ran adopt
);

CREATE INDEX IF NOT EXISTS adopt_history_caller_idx
    ON atlantis.adopt_history (caller, adopted_at DESC);
