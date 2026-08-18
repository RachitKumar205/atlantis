-- Each organisation gets its own atlantis, reached with its own credentials.
--
-- Until now the console held one client certificate and dialled one address for
-- everybody, so it could not serve two organisations at all — and the failure if
-- it tried was silent: the wrong endpoint returns the wrong organisation's
-- schema, plans and jobs, with no error anywhere.
--
-- Worse, nothing on the atlantis side would have refused a certificate issued
-- for one organisation and presented to another's server. All five layers pass:
-- the certificates chain to the same CA, both carry CN=atlantis-console, the
-- cert-binding check is skipped because ATL_CERT_BINDING_EXEMPT_CALLERS defaults
-- to exactly that name, and migration 0019 seeds that identity with
-- CAPABILITY_OPERATOR in every install.
--
-- The fix is a CA per organisation, which makes a mismatch fail inside the TLS
-- handshake, before any atlantis code runs. This migration is the console's half
-- of it: somewhere to keep each organisation's address and credentials.

-- Addresses. Not secrets, and useful to read when something is misrouted.
--
-- atl_health_addr is separate because the console reaches atlantis two ways:
-- the admin gRPC channel over mTLS, and three plain HTTP GETs against the
-- health listener for the Health page. The second is easy to overlook, because
-- it never goes through the gRPC client at all — and an unmoved health address
-- would have every organisation's Health page reporting one server's status.
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS atl_endpoint    TEXT;
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS atl_health_addr TEXT;

-- The trust root this organisation's atlantis presents, and the leaf the
-- console presents back. Both are public documents by construction: a
-- certificate is published to everyone who connects. Encrypting them would cost
-- an operator the ability to run `openssl x509 -text` against a row while
-- diagnosing a handshake failure, and would protect nothing.
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS ca_pem          TEXT;
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS client_cert_pem TEXT;

-- The private half, encrypted.
--
-- BYTEA rather than TEXT because it is ciphertext, not text — Tink's output is
-- binary and base64-ing it into a TEXT column would only add a decode step and
-- an invitation to read it as though it meant something.
--
-- Sealed by internal/console/secrets with the organisation name as additional
-- authenticated data. That binding is what stops somebody who can UPDATE this
-- table lifting one organisation's key onto another's row: the copy decrypts
-- under a different organisation and the open fails. Without it, the same
-- UPDATE hands over a genuine, working credential — and every check downstream
-- is satisfied, because the credential IS genuine. It is simply being used by
-- the wrong tenant.
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS client_key_ct   BYTEA;

-- When the row last changed, so a running console can notice a rotated
-- certificate without being restarted. The connection pool re-reads a row on a
-- timer and rebuilds its client when this moves.
ALTER TABLE console.orgs ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Every column is nullable, deliberately.
--
-- A row appears the moment somebody from that organisation signs in
-- (store.rememberOrg, called from the assertion exchange), which is before
-- anything has provisioned the organisation's stack. So "known but not yet
-- reachable" is a real and expected state, and it has to be representable.
--
-- The console refuses to serve such an organisation and says so. It does NOT
-- fall back to a shared endpoint, which is the whole point of the step: a
-- fallback is precisely the silent cross-organisation read this is preventing.
-- One missing row would otherwise route an unprovisioned organisation into
-- somebody else's atlantis, and everything would appear to work.
--
-- console.orgs stays unpoliced (see 0004 and internal/console/policyguard.go):
-- it is the registry, and one console process must read every row to serve
-- anyone. What changes here is that a row now holds something worth encrypting,
-- which is why the key is sealed rather than the table policed.
