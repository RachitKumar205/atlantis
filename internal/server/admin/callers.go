package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// GetCallers — list all known callers (registered identities ∪ has-files)
// ---------------------------------------------------------------------------

// CallerInfo summarises one caller's registration state.
//
// Registered carries the operator-recorded intent: true means the caller
// exists in caller_identities (either pre-registered by an operator or
// implicitly back-filled from the first apply). CanMutate reflects the
// caller's mutation permission as recorded in caller_identities; this is
// UNIONed with the static ATL_MUTATION_ALLOWED_CALLERS env var at the
// apply-time gate.
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

type GetCallersResponse struct {
	Callers []CallerInfo `json:"callers"`
}

// GetCallers lists every known caller — the UNION of caller_identities
// (operator-registered) and caller_registrations (anyone who has ever
// `tide apply`'d). A caller may appear with 0 file_count if they were
// registered through the console but have not yet pushed schema.
func (s *Service) GetCallers(ctx context.Context, _ GetCallersRequest) (*GetCallersResponse, error) {
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
    ci.cert_expires_at::text                AS cert_expires_at
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

	var out []CallerInfo
	for rows.Next() {
		var ci CallerInfo
		var lastAt *string
		var schemaVer *int64
		var certExp *string
		if err := rows.Scan(&ci.Caller, &ci.FileCount, &lastAt, &schemaVer, &ci.Registered, &ci.CanMutate, &certExp); err != nil {
			return nil, err
		}
		if lastAt != nil {
			ci.LastAppliedAt = *lastAt
		}
		if schemaVer != nil {
			ci.SchemaVersion = *schemaVer
		}
		if certExp != nil {
			ci.CertExpiresAt = *certExp
		}
		out = append(out, ci)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &GetCallersResponse{Callers: out}, nil
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
// Operator-only.
func (s *Service) RegisterCaller(ctx context.Context, req RegisterCallerRequest) (*RegisterCallerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if !validCallerName(req.Caller) {
		return nil, errors.New("admin: caller name must be 1-64 chars, lowercase alphanumeric + '-' (interior only)")
	}
	// Reject names atlantis reserves for its own infrastructure CNs.
	switch req.Caller {
	case "atlantis", "atlantis-console", "atlantis-signer", "anonymous":
		return nil, fmt.Errorf("admin: %q is reserved", req.Caller)
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
VALUES ($1, $2, $3)
ON CONFLICT (caller) DO UPDATE SET can_mutate = EXCLUDED.can_mutate`,
		req.Caller, req.CanMutate, req.CreatedBy)
	if err != nil {
		return nil, fmt.Errorf("register caller: %w", err)
	}
	return &RegisterCallerResponse{Caller: req.Caller, CanMutate: req.CanMutate}, nil
}

// isRegisteredCaller reports whether the named caller exists in
// caller_identities along with its can_mutate flag, so an operator can
// grant mutation permission without an env-var change and atlantis
// restart. A nil pool reports "not registered" without erroring — keeps
// the static gate branches exercisable in unit tests that don't stand
// up a Postgres.
func (s *Service) isRegisteredCaller(ctx context.Context, caller string) (registered, canMutate bool, err error) {
	if s.pool == nil {
		return false, false, nil
	}
	err = s.pool.QueryRow(ctx, `
SELECT can_mutate FROM atlantis.caller_identities WHERE caller = $1`, caller).Scan(&canMutate)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("lookup caller_identities: %w", err)
	}
	return true, canMutate, nil
}

// LookupCallerCertBinding returns the cert-binding state for a caller:
// exists reports whether a caller_identities row is present, and
// fingerprint is the 32-byte SHA-256 of its currently-active cert (nil
// when the row exists but no cert has been recorded yet — the
// back-compat case for callers minted before the binding column).
//
// This is the hot path for the cert-binding interceptor; callers
// should layer a TTL cache on top to avoid one DB read per RPC under
// burst. A nil pool returns (false, nil, nil) so tests that don't
// stand up Postgres can exercise the "no binding configured"
// branch.
func (s *Service) LookupCallerCertBinding(ctx context.Context, caller string) (exists bool, fingerprint []byte, err error) {
	if s.pool == nil {
		return false, nil, nil
	}
	err = s.pool.QueryRow(ctx, `
SELECT cert_fingerprint FROM atlantis.caller_identities WHERE caller = $1`, caller).Scan(&fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("lookup caller_identities.cert_fingerprint: %w", err)
	}
	return true, fingerprint, nil
}

// ---------------------------------------------------------------------------
// RecordCallerCertExpiry — persist NotAfter + fingerprint of a freshly issued cert
// ---------------------------------------------------------------------------

type RecordCallerCertExpiryRequest struct {
	Caller string `json:"caller"`
	// ExpiresAt is RFC3339. Required.
	ExpiresAt string `json:"expires_at"`
	// Fingerprint is the hex-encoded SHA-256 of the signed leaf cert's
	// DER bytes. Required for new code paths; optional during the
	// migration window so older console binaries (which only reported
	// expiry) keep working. Once set, every authenticated RPC from this
	// caller must present a cert whose fingerprint matches — that's how
	// rotation + revoke actually invalidate prior certs without a CRL.
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
func (s *Service) RecordCallerCertExpiry(ctx context.Context, req RecordCallerCertExpiryRequest) (*RecordCallerCertExpiryResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if !validCallerName(req.Caller) {
		return nil, errors.New("admin: invalid caller name")
	}
	if req.ExpiresAt == "" {
		return nil, errors.New("admin: expires_at is required")
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("admin: parse expires_at: %w", err)
	}

	// Fingerprint optional during migration window. When present must be
	// exactly 64 hex chars (SHA-256 = 32 bytes = 64 hex). A malformed
	// value would silently land as a non-matching fingerprint and lock
	// the caller out, so reject early at the boundary.
	var fp []byte
	if req.Fingerprint != "" {
		fp, err = hex.DecodeString(req.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("admin: parse fingerprint: %w", err)
		}
		if len(fp) != sha256.Size {
			return nil, fmt.Errorf("admin: fingerprint must be %d bytes (got %d)", sha256.Size, len(fp))
		}
	}

	// One UPDATE so expiry + fingerprint flip atomically. If fp is nil
	// (back-compat caller), COALESCE preserves whatever's already there
	// — we never *unset* a fingerprint from this path.
	tag, err := s.pool.Exec(ctx, `
UPDATE atlantis.caller_identities
   SET cert_expires_at  = $2,
       cert_fingerprint = COALESCE($3, cert_fingerprint)
 WHERE caller = $1`, req.Caller, exp.UTC(), fp)
	if err != nil {
		return nil, fmt.Errorf("update caller cert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("admin: caller %q is not registered", req.Caller)
	}
	return &RecordCallerCertExpiryResponse{Caller: req.Caller, ExpiresAt: exp.UTC().Format(time.RFC3339)}, nil
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

// RevokeCaller removes a caller from BOTH the identities table and the
// registrations table. The caller will no longer appear in GetCallers,
// can't apply schema, and can't be issued a new cert by the signer.
//
// Effect on existing certs: the cert-binding interceptor (cmd/server)
// reads caller_identities.cert_fingerprint on every authenticated RPC
// and rejects any caller whose row is missing. Deletion here makes the
// caller's row missing, which means every still-crypto-valid cert
// minted for this CN starts failing Unauthenticated within one cache
// TTL (~5s). This is the revocation mechanism — no CRL, no OCSP, just
// the row going away.
func (s *Service) RevokeCaller(ctx context.Context, req RevokeCallerRequest) (*RevokeCallerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if req.Caller == "" {
		return nil, fmt.Errorf("caller is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin revoke tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
DELETE FROM atlantis.caller_registrations WHERE caller = $1`, req.Caller)
	if err != nil {
		return nil, fmt.Errorf("revoke caller_registrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM atlantis.caller_identities WHERE caller = $1`, req.Caller); err != nil {
		return nil, fmt.Errorf("revoke caller_identities: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit revoke: %w", err)
	}
	return &RevokeCallerResponse{FilesRemoved: int(tag.RowsAffected())}, nil
}
