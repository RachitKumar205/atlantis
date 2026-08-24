package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
)

// ---------------------------------------------------------------------------
// GetCallers — list all known callers (registered identities ∪ has-files)
// ---------------------------------------------------------------------------

// CallerInfo summarises one caller's registration state.
//
// Registered carries the operator-recorded intent: true means the caller
// exists in caller_identities (either pre-registered by an operator or
// implicitly back-filled from the first apply). CanMutate reports the
// caller_identities flag, which is the input RegisterCaller translates into
// capability grants — it is not itself read at apply time. The authoritative
// answer to "may this caller apply" is a CAPABILITY_SCHEMA_APPLY row in
// caller_capabilities, which an operator can grant or revoke independently.
type CallerInfo struct {
	Caller        string `json:"caller"`
	FileCount     int    `json:"file_count"`
	LastAppliedAt string `json:"last_applied_at,omitempty"` // RFC3339; empty if never applied
	SchemaVersion int64  `json:"schema_version,omitempty"`
	Registered    bool   `json:"registered"`
	CanMutate     bool   `json:"can_mutate"`
	CertExpiresAt string `json:"cert_expires_at,omitempty"` // RFC3339; empty when no cert was issued through the console
}

type GetCallersRequest struct{}

// GetCallers lists every known caller — the UNION of caller_identities
// (operator-registered) and caller_registrations (anyone who has ever
// `tide apply`'d). A caller may appear with 0 file_count if they were
// registered through the console but have not yet pushed schema.
//
// Revoked callers are listed too, carrying RevokedAt. They keep their row since
// migration 0033, so filtering them out here would leave an operator with no way
// to see that a caller exists but is cut off — and no way to find it again in
// order to restore it. This is the one read of caller_identities that
// deliberately does not go through the active_* views.
func (s *Service) GetCallers(ctx context.Context, _ *adminpb.GetCallersRequest) (*adminpb.GetCallersResponse, error) {
	// No-PG test path, matching the other methods in this package. Without it
	// this is the one migrated RPC that segfaults rather than returning.
	if s.pool == nil {
		return &adminpb.GetCallersResponse{}, nil
	}
	// FULL OUTER JOIN against an aggregated registrations subquery + the
	// identities table so each side fills in for the other:
	//   - identities-only: registered=true, file_count=0
	//   - registrations-only: registered=false (shouldn't happen post-
	//     migration backfill, but defensive)
	//   - both: file_count + identity flags
	rows, err := s.pool.Query(ctx, `
SELECT
    COALESCE(ci.caller, agg.caller)         AS caller,
    COALESCE(agg.file_count, 0)             AS file_count,
    agg.last_applied_at::text               AS last_applied_at,
    agg.schema_version                      AS schema_version,
    ci.caller IS NOT NULL                   AS registered,
    COALESCE(ci.can_mutate, false)          AS can_mutate,
    ci.cert_expires_at::text                AS cert_expires_at,
    ci.revoked_at::text                     AS revoked_at
FROM atlantis.caller_identities ci
FULL OUTER JOIN (
    SELECT
        cr.caller,
        COUNT(*)                AS file_count,
        MAX(sv.created_at)      AS last_applied_at,
        MAX(sv.version)         AS schema_version
    FROM atlantis.caller_registrations cr
    LEFT JOIN atlantis.schema_versions sv ON sv.caller = cr.caller
    GROUP BY cr.caller
) agg ON agg.caller = ci.caller
ORDER BY caller`)
	if err != nil {
		return nil, fmt.Errorf("list callers: %w", err)
	}
	defer rows.Close()

	var out []*adminpb.CallerInfo
	for rows.Next() {
		ci := &adminpb.CallerInfo{}
		var fileCount int64
		var lastAt *string
		var schemaVer *int64
		var certExp *string
		var revokedAt *string
		if err := rows.Scan(&ci.Caller, &fileCount, &lastAt, &schemaVer, &ci.Registered, &ci.CanMutate, &certExp, &revokedAt); err != nil {
			return nil, err
		}
		// file_count is a COUNT(*), so Postgres returns int8 while the wire
		// field is int32. Unreachable in practice — it would take two billion
		// .atl files for one caller — but report rather than clamp: a clamped
		// 2147483647 is indistinguishable from a real count, whereas the
		// pre-migration scan into `int` surfaced an out-of-range bigint as an
		// error on 32-bit builds. Keep the louder behaviour.
		if fileCount > math.MaxInt32 {
			return nil, fmt.Errorf("admin: file_count %d for caller %q exceeds the wire field's range", fileCount, ci.Caller)
		}
		ci.FileCount = int32(fileCount)
		if lastAt != nil {
			ci.LastAppliedAt = *lastAt
		}
		if schemaVer != nil {
			ci.SchemaVersion = *schemaVer
		}
		if certExp != nil {
			ci.CertExpiresAt = *certExp
		}
		if revokedAt != nil {
			ci.RevokedAt = *revokedAt
		}
		out = append(out, ci)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &adminpb.GetCallersResponse{Callers: out}, nil
}

// ---------------------------------------------------------------------------
// RegisterCaller — pre-register a caller cert-CN before first apply
// ---------------------------------------------------------------------------

type RegisterCallerRequest struct {
	Caller    string `json:"caller"`
	CanMutate bool   `json:"can_mutate"`
	CreatedBy string `json:"created_by,omitempty"` // operator email; logged for audit
}

type RegisterCallerResponse struct {
	Caller    string `json:"caller"`
	CanMutate bool   `json:"can_mutate"`
}

// validCallerName enforces a conservative grammar for caller names so the
// signer can rely on it as a SQL-safe identifier and so accidental
// whitespace / shell metacharacters don't slip into cert CNs.
func validCallerName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// RegisterCaller idempotently records the given cert-CN as a known
// identity. Subsequent calls update can_mutate but leave created_at /
// created_by intact — the original onboarding event is the authoritative
// audit record; mutation-bit toggles after the fact are captured in the
// console's audit_log instead.
//
// Registration also reconciles the caller's default capability grants, in the
// same transaction as the identity row. Before caller_capabilities existed,
// can_mutate was read directly by the gates, so writing the row was the whole
// of registration. Now the gates read grants, and an identity with no grants
// can authenticate and do nothing — so a registration that wrote only the
// identity would produce a caller that looks correct in GetCallers and is
// refused by every RPC. See authz.DefaultCapabilities for the mapping and
// authz.ManagedCapabilities for what this deliberately does not touch.
//
// Operator-only.
func (s *Service) RegisterCaller(ctx context.Context, req *adminpb.RegisterCallerRequest) (*adminpb.RegisterCallerResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if !validCallerName(req.GetCaller()) {
		return nil, errors.New("admin: caller name must be 1-64 chars, lowercase alphanumeric + '-' (interior only)")
	}
	// Reject names atlantis reserves for its own infrastructure CNs.
	switch req.GetCaller() {
	case "atlantis", "atlantis-console", "atlantis-signer", "anonymous":
		return nil, fmt.Errorf("admin: %q is reserved", req.GetCaller())
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin register tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// A revoked caller is not re-registered back into service.
	//
	// The upsert below does not clear revoked_at, so without this the command
	// would report success while the caller stayed cut off — the operator learns
	// otherwise from a support ticket. Refusing is also the safer of the two
	// directions: registration is routine and revocation is deliberate, so a
	// routine act must not quietly undo a deliberate one.
	var revoked bool
	if err := tx.QueryRow(ctx, `
SELECT revoked_at IS NOT NULL FROM atlantis.caller_identities WHERE caller = $1`,
		req.GetCaller()).Scan(&revoked); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("check whether %q is revoked: %w", req.GetCaller(), err)
	}
	if revoked {
		return nil, fmt.Errorf("admin: caller %q is revoked; restore it explicitly "+
			"rather than re-registering, which would leave it revoked and report success",
			req.GetCaller())
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
VALUES ($1, $2, $3)
ON CONFLICT (caller) DO UPDATE SET can_mutate = EXCLUDED.can_mutate`,
		req.GetCaller(), req.GetCanMutate(), req.GetCreatedBy()); err != nil {
		return nil, fmt.Errorf("register caller: %w", err)
	}

	if err := syncDefaultCapabilities(ctx, tx, req.GetCaller(), req.GetCanMutate(), req.GetCreatedBy()); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit register: %w", err)
	}
	return &adminpb.RegisterCallerResponse{Caller: req.GetCaller(), CanMutate: req.GetCanMutate()}, nil
}

// syncDefaultCapabilities reconciles the managed grant bundle for one caller.
//
// Revoke-then-grant, not grant-only: flipping can_mutate from true to false has
// to actually remove SCHEMA_APPLY, or demotion would be cosmetic. The DELETE is
// scoped to the managed set so an out-of-band CAPABILITY_OPERATOR survives
// re-registration — an operator correcting a typo in created_by should not
// silently strip authority they granted on purpose.
//
// The INSERT uses ON CONFLICT DO NOTHING rather than an upsert so granted_at
// keeps recording when the caller first received the capability. Re-registering
// is not a new grant, and rewriting the timestamp would erase the only record
// of when the authority actually began.
func syncDefaultCapabilities(ctx context.Context, tx pgx.Tx, caller string, canMutate bool, grantedBy string) error {
	granted := authz.Names(authz.DefaultCapabilities(canMutate))

	if _, err := tx.Exec(ctx, `
DELETE FROM atlantis.caller_capabilities
 WHERE caller = $1
   AND capability = ANY($2)
   AND capability <> ALL($3)`,
		caller, authz.Names(authz.ManagedCapabilities()), granted); err != nil {
		return fmt.Errorf("revoke stale capabilities for %q: %w", caller, err)
	}

	if grantedBy == "" {
		grantedBy = "<register-caller>"
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
SELECT $1, capability, $3 FROM unnest($2::text[]) AS capability
ON CONFLICT DO NOTHING`,
		caller, granted, grantedBy); err != nil {
		return fmt.Errorf("grant default capabilities to %q: %w", caller, err)
	}
	return nil
}

// LookupCallerCertBinding reports whether a caller still has an identity row.
//
// That is the whole of it. It used to return which certificate authenticated as
// the caller and which one it had replaced, with a deadline on the latter so a
// renewal whose response was lost did not lock the machine out (migration 0031).
// Migration 0032 removed all three columns along with pinning.
//
// What survives is the fastest revocation path in the system: RevokeCaller
// deletes the row, and the interceptor's five-second cache means the caller
// stops authenticating within five seconds — against thirty for the auth
// allowlist's refresher.
//
// This is the hot path for the cert-binding interceptor; callers
// should layer a TTL cache on top to avoid one DB read per RPC under
// burst. A nil pool returns an empty binding so tests that don't stand
// up Postgres can exercise the "no binding configured" branch.
func (s *Service) LookupCallerCertBinding(ctx context.Context, caller string) (interceptors.CertBinding, error) {
	if s.pool == nil {
		return interceptors.CertBinding{}, nil
	}
	// Existence, and nothing else. This selected three fingerprint columns
	// until migration 0032; see it for why a seven-day certificate makes them
	// unnecessary.
	//
	// Against the view, not the table: a revoked caller keeps its row now
	// (migration 0033), so the table would answer "yes, it exists" for exactly
	// the caller this check is meant to refuse.
	//
	// SELECT 1 rather than a column, so this query does not have to be revisited
	// the next time the table's shape changes.
	var one int
	err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM atlantis.active_caller_identities WHERE caller = $1`, caller).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return interceptors.CertBinding{}, nil
	}
	if err != nil {
		return interceptors.CertBinding{}, fmt.Errorf("lookup caller_identities: %w", err)
	}
	return interceptors.CertBinding{Exists: true}, nil
}

// ---------------------------------------------------------------------------
// RecordCallerCertExpiry — persist NotAfter + fingerprint of a freshly issued cert
// ---------------------------------------------------------------------------

type RecordCallerCertExpiryRequest struct {
	Caller string `json:"caller"`
	// ExpiresAt is RFC3339. Required.
	ExpiresAt string `json:"expires_at"`
	// Fingerprint is the hex-encoded SHA-256 of the signed leaf cert's DER
	// bytes.
	//
	// Accepted and validated for length, then discarded. Nothing stores or
	// compares it since migration 0032 dropped the columns — it stays on the
	// request so an older console binary that still sends one is not an error.
	//
	// This said it was what made "rotation + revoke actually invalidate prior
	// certs without a CRL", which stopped being true when the columns went and
	// is the kind of claim that gets believed during an incident. Revocation is
	// deleting the identity row; expiry is what supersedes a particular
	// certificate.
	Fingerprint string `json:"fingerprint,omitempty"`
}

type RecordCallerCertExpiryResponse struct {
	Caller    string `json:"caller"`
	ExpiresAt string `json:"expires_at"`
}

// RecordCallerCertExpiry stores the NotAfter and fingerprint of the
// caller's most recently console-issued cert. Operator-only — invoked
// by the BFF after a successful signer issuance.
//
// The fingerprint write is the load-bearing one: the cert-binding
// interceptor in cmd/server reads this column on every authenticated
// RPC and rejects any peer cert whose SHA-256 doesn't match. So a
// successful UPDATE here is what flips an old cert from "still
// crypto-valid" to "superseded — won't authenticate." A failed write
// leaves the old fingerprint in place; the caller will keep working
// with the old cert until a successful re-record (operationally we
// surface the BFF error and the operator retries).
func (s *Service) RecordCallerCertExpiry(ctx context.Context, req *adminpb.RecordCallerCertExpiryRequest) (*adminpb.RecordCallerCertExpiryResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if !validCallerName(req.GetCaller()) {
		return nil, errors.New("admin: invalid caller name")
	}
	if req.GetExpiresAt() == "" {
		return nil, errors.New("admin: expires_at is required")
	}
	exp, err := time.Parse(time.RFC3339, req.GetExpiresAt())
	if err != nil {
		return nil, fmt.Errorf("admin: parse expires_at: %w", err)
	}

	// Fingerprint optional during migration window. When present must be
	// exactly 64 hex chars (SHA-256 = 32 bytes = 64 hex). A malformed
	// value would silently land as a non-matching fingerprint and lock
	// the caller out, so reject early at the boundary.
	var fp []byte
	if req.GetFingerprint() != "" {
		fp, err = hex.DecodeString(req.GetFingerprint())
		if err != nil {
			return nil, fmt.Errorf("admin: parse fingerprint: %w", err)
		}
		if len(fp) != sha256.Size {
			return nil, fmt.Errorf("admin: fingerprint must be %d bytes (got %d)", sha256.Size, len(fp))
		}
	}

	// Records when this caller's certificate runs out, and nothing else.
	//
	// It used to maintain three fingerprint columns and shift one into a
	// previous slot with a deadline, so a renewal whose response was lost did
	// not lock the machine out. Migration 0032 removed all of that with pinning;
	// a seven-day certificate is what makes the overlap unnecessary.
	//
	// cert_expires_at survives because GetCallers reports it and the console
	// shows it. "When does this run out" is worth answering whether or not
	// anything enforces a particular certificate.
	tag, err := s.pool.Exec(ctx, `
UPDATE atlantis.caller_identities
   SET cert_expires_at = $2
 WHERE caller = $1`, req.GetCaller(), exp.UTC())
	if err != nil {
		return nil, fmt.Errorf("update caller cert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("admin: caller %q is not registered", req.GetCaller())
	}
	return &adminpb.RecordCallerCertExpiryResponse{Caller: req.GetCaller(), ExpiresAt: exp.UTC().Format(time.RFC3339)}, nil
}

// ---------------------------------------------------------------------------
// RevokeCaller — remove a caller's registrations
// ---------------------------------------------------------------------------

type RevokeCallerRequest struct {
	Caller string `json:"caller"`
}

type RevokeCallerResponse struct {
	FilesRemoved int `json:"files_removed"`
}

// RevokeCaller stops a caller authenticating: it marks the identity row revoked
// and removes the caller's schema registrations.
//
// Effect on existing certs: the cert-binding interceptor reads
// atlantis.active_caller_identities on every authenticated RPC, and that view
// excludes revoked rows. So every still-crypto-valid certificate minted for this
// CN starts failing Unauthenticated within one cache TTL (~5s). This is the
// revocation mechanism — no CRL, no OCSP.
//
// # Why the identity row is marked rather than deleted
//
// caller_capabilities references it ON DELETE CASCADE, so deleting it destroyed
// every grant the caller held — including CAPABILITY_OPERATOR and
// CAPABILITY_LOGS_READ, which registration deliberately does not manage so that
// re-registering cannot strip what an operator granted by hand. Revoking stripped
// it anyway, and the loss only showed up later as a PermissionDenied on one page.
//
// It also made revocation one-way for a reserved CN. RegisterCaller refuses
// 'atlantis-console', so a deleted console identity had no supported way back —
// and the console is the caller that most needs revoking, holding an admin
// credential for every organisation. See migration 0033.
//
// # Why a caller with no identity row still gets one
//
// The upsert writes a tombstone for a caller that only ever existed in
// caller_registrations. Without it, deleting the registrations would cut the
// caller off until its next `tide apply` wrote them back — revocation undone by
// the very thing being revoked.
func (s *Service) RevokeCaller(ctx context.Context, req *adminpb.RevokeCallerRequest) (*adminpb.RevokeCallerResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if req.GetCaller() == "" {
		return nil, fmt.Errorf("caller is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin revoke tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
DELETE FROM atlantis.caller_registrations WHERE caller = $1`, req.GetCaller())
	if err != nil {
		return nil, fmt.Errorf("revoke caller_registrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by, revoked_at)
VALUES ($1, false, '<revoked>', NOW())
ON CONFLICT (caller) DO UPDATE SET revoked_at = NOW()`, req.GetCaller()); err != nil {
		return nil, fmt.Errorf("revoke caller_identities: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit revoke: %w", err)
	}
	// RowsAffected is int64, the wire field int32. See the note in GetCallers
	// on reporting rather than clamping.
	removed := tag.RowsAffected()
	if removed > math.MaxInt32 {
		return nil, fmt.Errorf("admin: revoked %d rows for caller %q, which exceeds the wire field's range", removed, req.GetCaller())
	}
	return &adminpb.RevokeCallerResponse{FilesRemoved: int32(removed)}, nil
}
