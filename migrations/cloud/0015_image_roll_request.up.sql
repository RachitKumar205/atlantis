-- Let an operator ask for an organisation's images to be replaced.
--
-- A request, not a command, for the reason console_rotate_requested_at is one:
-- the workloads live in the organisation's namespace and only the provisioner
-- holds Kubernetes credentials, so `cloud org roll` marks the row and the
-- provisioner acts on its next reconcile pass.
--
-- No reconcile pass rolls an organisation. Ensure applies the configured images
-- when it builds one and reconcile never revisits them, so a ready organisation
-- stays on the image it was provisioned with until this column is set.
--
-- One path moves an image without a request: a provisioning attempt that failed
-- after the workloads existed retries through Ensure, which applies whatever is
-- configured at the time. That reaches organisations in backoff, not ready ones.
ALTER TABLE cloud.org_provisioning
    ADD COLUMN IF NOT EXISTS image_roll_requested_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS image_roll_kinds        TEXT[],
    ADD COLUMN IF NOT EXISTS image_roll_attempts     INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN cloud.org_provisioning.image_roll_requested_at IS
    'Set by `cloud org roll`. The provisioner re-applies the requested workloads '
    'on its next reconcile pass and clears this. NULL means no request is '
    'outstanding; nothing rolls an organisation without one.';

-- The kinds are stored rather than assumed, because they differ in cost.
--
-- Rolling server or signer replaces a Deployment's pod behind a readiness
-- probe. Rolling postgres changes a CloudNativePG imageName, which restarts a
-- single-instance database for about 110 seconds. An operator asking for one
-- must not get the other.
COMMENT ON COLUMN cloud.org_provisioning.image_roll_kinds IS
    'Which images the request covers: any of server, signer, postgres. Never '
    'NULL while image_roll_requested_at is set; the provisioner rolls exactly '
    'these and treats an empty set as nothing to do.';

-- How many passes have tried this request and failed.
--
-- The provisioner rolls a bounded number of organisations per pass, oldest
-- request first. Without this column a request that can never succeed — a
-- storage size the CloudNativePG webhook refuses to shrink, a namespace stuck
-- terminating — keeps the oldest timestamp, takes the only slot on every pass,
-- and no other organisation in the fleet is ever rolled. Ordering by attempts
-- first moves a failing request behind every request that has not been tried.
COMMENT ON COLUMN cloud.org_provisioning.image_roll_attempts IS
    'Passes that tried this request and failed. Ordering puts lower counts '
    'first, so one request that cannot succeed does not block the queue. Reset '
    'to 0 by a new request and by a successful roll.';

-- Both columns move together, so a request can never name kinds with no
-- timestamp or a timestamp with no kinds. The provisioner reads the pair and
-- would otherwise have to decide what half a request means.
ALTER TABLE cloud.org_provisioning
    ADD CONSTRAINT org_provisioning_image_roll_complete
    CHECK ((image_roll_requested_at IS NULL) = (image_roll_kinds IS NULL));

-- Partial index over outstanding requests only, as with
-- org_provisioning_console_rotate_idx: reconcile asks "which organisations have
-- asked for a roll" on every pass, for a column that is almost always NULL.
CREATE INDEX IF NOT EXISTS org_provisioning_image_roll_idx
    ON cloud.org_provisioning (org)
    WHERE image_roll_requested_at IS NOT NULL;
