-- Per-caller enrolment policy: whether a viewer may enrol their own machine
-- as this caller through `tide login`.
--
-- A caller certificate authenticates as the caller — which plans and applies
-- schema — not as the person holding it, so viewer self-enrolment is an
-- opt-in per caller rather than a property of the role. Absent row or false:
-- admins only, which is the posture token minting has always had.
--
-- In the console's database rather than atlantis's because enrolment policy
-- lives where enrolment lives: the tokens, the issued certificates and the
-- listener are all here, and the org server never sees an enrolment.
CREATE TABLE IF NOT EXISTS console.caller_enrollment (
    org                   TEXT        NOT NULL,
    caller                TEXT        NOT NULL,
    developers_may_enroll BOOLEAN     NOT NULL DEFAULT false,
    updated_by            TEXT        NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org, caller)
);

-- Organisation isolation, the 0007 shape: a RESTRICTIVE boundary plus the
-- permissive grant. The enrolment endpoint has no session; the handler binds
-- the organisation the request names, and this policy compares it to the row.
ALTER TABLE console.caller_enrollment ENABLE ROW LEVEL SECURITY;
ALTER TABLE console.caller_enrollment FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS caller_enrollment_org_isolation ON console.caller_enrollment;
CREATE POLICY caller_enrollment_org_isolation ON console.caller_enrollment
    AS RESTRICTIVE
    USING (org = console.current_org())
    WITH CHECK (org = console.current_org());

DROP POLICY IF EXISTS caller_enrollment_default_access ON console.caller_enrollment;
CREATE POLICY caller_enrollment_default_access ON console.caller_enrollment
    AS PERMISSIVE USING (true) WITH CHECK (true);
