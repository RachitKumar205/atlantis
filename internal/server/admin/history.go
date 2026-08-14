package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ---------------------------------------------------------------------------
// GetSchemaHistory — paginated version list
// ---------------------------------------------------------------------------

// GetSchemaHistoryRequest asks for a page of schema version summaries,
// ordered newest-first. Before is a cursor (version number); only
// versions < Before are returned. Caller filters to a single caller.
type GetSchemaHistoryRequest struct {
	Limit  int32  `json:"limit,omitempty"`
	Before int64  `json:"before,omitempty"`
	Caller string `json:"caller,omitempty"`
}

// SchemaVersionSummary is the compact shape rendered by `tide history`.
type SchemaVersionSummary struct {
	Version     int64  `json:"version"`
	Caller      string `json:"caller"`
	PlanClass   string `json:"plan_class"`
	EventType   string `json:"event_type"`
	ChangeCount int    `json:"change_count"`
	CreatedAt   string `json:"created_at"`
	IRHash      string `json:"ir_hash"` // sha256 of the IR snapshot; git-style content address
}

// GetSchemaHistoryResponse carries one page of versions plus a flag
// indicating whether more rows exist beyond this page.
type GetSchemaHistoryResponse struct {
	Versions []SchemaVersionSummary `json:"versions"`
	HasMore  bool                   `json:"has_more"`
}

func (s *Service) GetSchemaHistory(ctx context.Context, req *adminpb.GetSchemaHistoryRequest) (*adminpb.GetSchemaHistoryResponse, error) {
	// No-PG path, matching the rest of the package.
	if s.pool == nil {
		return &adminpb.GetSchemaHistoryResponse{}, nil
	}
	limit := int32(25)
	if req.GetLimit() > 0 && req.GetLimit() <= 100 {
		limit = req.GetLimit()
	}
	// Fetch limit+1 so we can detect whether there are more rows.
	fetchLimit := limit + 1

	var rows pgx.Rows
	var err error
	if req.GetCaller() != "" && req.GetBefore() > 0 {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at, ir_hash
FROM atlantis.schema_versions
WHERE version < $1 AND caller = $2
ORDER BY version DESC
LIMIT $3`, req.GetBefore(), req.GetCaller(), fetchLimit)
	} else if req.GetCaller() != "" {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at, ir_hash
FROM atlantis.schema_versions
WHERE caller = $1
ORDER BY version DESC
LIMIT $2`, req.GetCaller(), fetchLimit)
	} else if req.GetBefore() > 0 {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at, ir_hash
FROM atlantis.schema_versions
WHERE version < $1
ORDER BY version DESC
LIMIT $2`, req.GetBefore(), fetchLimit)
	} else {
		rows, err = s.pool.Query(ctx, `
SELECT version, caller, plan_class, event_type, diff, created_at, ir_hash
FROM atlantis.schema_versions
ORDER BY version DESC
LIMIT $1`, fetchLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("query schema_versions: %w", err)
	}
	defer rows.Close()

	var versions []SchemaVersionSummary
	for rows.Next() {
		var v SchemaVersionSummary
		var diffJSON []byte
		var createdAt interface{}
		if err := rows.Scan(&v.Version, &v.Caller, &v.PlanClass, &v.EventType, &diffJSON, &createdAt, &v.IRHash); err != nil {
			return nil, err
		}
		v.CreatedAt = fmt.Sprintf("%v", createdAt)
		v.ChangeCount = countDiffChanges(diffJSON)
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := int32(len(versions)) > limit
	if hasMore {
		versions = versions[:limit]
	}

	return &adminpb.GetSchemaHistoryResponse{
		Versions: schemaVersionSummariesToPB(versions),
		HasMore:  hasMore,
	}, nil
}

// countDiffChanges unmarshals a STORED diff just enough to count its changes.
// Tolerant of malformed JSON — returns 0.
//
// This decodes into codegen.Diff rather than a local struct listing the buckets
// by name. The local struct is how a bucket gets missed: adding Destructive left
// it silently counting three of four, so a stored plan would report fewer
// changes than it held and the omitted kind would be the one that destroys data.
// Decoding into the real type means a new bucket is counted the day it exists.
func countDiffChanges(raw []byte) int {
	var d codegen.Diff
	if json.Unmarshal(raw, &d) != nil {
		return 0
	}
	return d.Len()
}

// ---------------------------------------------------------------------------
// GetSchemaVersion — full data for one version
// ---------------------------------------------------------------------------

type GetSchemaVersionRequest struct {
	Version int64 `json:"version"`
}

type GetSchemaVersionResponse struct {
	Version    int64           `json:"version"`
	Caller     string          `json:"caller"`
	PlanClass  string          `json:"plan_class"`
	EventType  string          `json:"event_type"`
	Diff       json.RawMessage `json:"diff"`
	UpSQL      string          `json:"up_sql"`
	DownSQL    string          `json:"down_sql"`
	IRSnapshot json.RawMessage `json:"ir_snapshot"`
	CreatedAt  string          `json:"created_at"`
	ParentVer  *int64          `json:"parent_version,omitempty"`
	IRHash     string          `json:"ir_hash"`
}

func (s *Service) GetSchemaVersion(ctx context.Context, req *adminpb.GetSchemaVersionRequest) (*adminpb.GetSchemaVersionResponse, error) {
	if req.GetVersion() <= 0 {
		return nil, errors.New("admin: version must be a positive integer")
	}
	var resp GetSchemaVersionResponse
	var createdAt interface{}
	err := s.pool.QueryRow(ctx, `
SELECT version, caller, plan_class, event_type, diff, up_sql, down_sql,
       ir_snapshot, created_at, parent_version, ir_hash
FROM atlantis.schema_versions
WHERE version = $1`, req.GetVersion()).Scan(
		&resp.Version, &resp.Caller, &resp.PlanClass, &resp.EventType,
		&resp.Diff, &resp.UpSQL, &resp.DownSQL,
		&resp.IRSnapshot, &createdAt, &resp.ParentVer, &resp.IRHash,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: schema version %d not found", req.GetVersion())
		}
		return nil, fmt.Errorf("query schema_versions: %w", err)
	}
	resp.CreatedAt = fmt.Sprintf("%v", createdAt)
	return schemaVersionToPB(&resp), nil
}

// ---------------------------------------------------------------------------
// DiffSchemaVersions — compute diff between two historical versions
// ---------------------------------------------------------------------------

type DiffSchemaVersionsRequest struct {
	FromVersion int64 `json:"from_version"`
	ToVersion   int64 `json:"to_version"`
}

type DiffSchemaVersionsResponse struct {
	FromVersion int64           `json:"from_version"`
	ToVersion   int64           `json:"to_version"`
	Diff        json.RawMessage `json:"diff"`
	FromIR      json.RawMessage `json:"from_ir,omitempty"`
	ToIR        json.RawMessage `json:"to_ir,omitempty"`
}

func (s *Service) DiffSchemaVersions(ctx context.Context, req *adminpb.DiffSchemaVersionsRequest) (*adminpb.DiffSchemaVersionsResponse, error) {
	if req.GetFromVersion() <= 0 || req.GetToVersion() <= 0 {
		return nil, errors.New("admin: both from_version and to_version must be positive integers")
	}

	loadSnapshot := func(ver int64) ([]byte, *dsl.IR, error) {
		var raw []byte
		err := s.pool.QueryRow(ctx, `
SELECT ir_snapshot FROM atlantis.schema_versions WHERE version = $1`, ver).Scan(&raw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, fmt.Errorf("admin: schema version %d not found", ver)
			}
			return nil, nil, err
		}
		ir, err := dsl.DecodeJSONIR(raw)
		return raw, ir, err
	}

	fromRaw, fromIR, err := loadSnapshot(req.GetFromVersion())
	if err != nil {
		return nil, err
	}
	toRaw, toIR, err := loadSnapshot(req.GetToVersion())
	if err != nil {
		return nil, err
	}

	d := codegen.ComputeDiff(fromIR, toIR)
	diffJSON, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal diff: %w", err)
	}

	return &adminpb.DiffSchemaVersionsResponse{
		FromVersion: req.GetFromVersion(),
		ToVersion:   req.GetToVersion(),
		Diff:        diffJSON,
		FromIr:      fromRaw,
		ToIr:        toRaw,
	}, nil
}

// ---------------------------------------------------------------------------
// GetEntityLineage — blame for one entity
// ---------------------------------------------------------------------------

type GetEntityLineageRequest struct {
	EntityID string `json:"entity_id"`
}

type EntityLineageEntry struct {
	EntityID       string `json:"entity_id"`
	FieldName      string `json:"field_name"`
	IntroducedBy   string `json:"introduced_by"`
	IntroducedAt   int64  `json:"introduced_at"`
	LastModifiedBy string `json:"last_modified_by"`
	LastModifiedAt int64  `json:"last_modified_at"`
	RemovedAt      *int64 `json:"removed_at,omitempty"`
}

type GetEntityLineageResponse struct {
	Entries []EntityLineageEntry `json:"entries"`
}

func (s *Service) GetEntityLineage(ctx context.Context, req *adminpb.GetEntityLineageRequest) (*adminpb.GetEntityLineageResponse, error) {
	if req.GetEntityId() == "" {
		return nil, errors.New("admin: entity_id is required")
	}

	rows, err := s.pool.Query(ctx, `
SELECT entity_id, field_name, introduced_by, introduced_at,
       last_modified_by, last_modified_at, removed_at
FROM atlantis.entity_lineage
WHERE entity_id = $1
ORDER BY field_name`, req.GetEntityId())
	if err != nil {
		return nil, fmt.Errorf("query entity_lineage: %w", err)
	}
	defer rows.Close()

	var entries []EntityLineageEntry
	for rows.Next() {
		var e EntityLineageEntry
		if err := rows.Scan(&e.EntityID, &e.FieldName, &e.IntroducedBy, &e.IntroducedAt,
			&e.LastModifiedBy, &e.LastModifiedAt, &e.RemovedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &adminpb.GetEntityLineageResponse{Entries: lineageToPB(entries)}, nil
}

// ---------------------------------------------------------------------------
// GetEntityOwners — entity -> caller map
// ---------------------------------------------------------------------------

type GetEntityOwnersRequest struct{}

type EntityOwnerEntry struct {
	EntityID     string `json:"entity_id"`
	IntroducedBy string `json:"introduced_by"`
	IntroducedAt int64  `json:"introduced_at"`
	FieldCount   int    `json:"field_count"`
}

type GetEntityOwnersResponse struct {
	Owners []EntityOwnerEntry `json:"owners"`
}

func (s *Service) GetEntityOwners(ctx context.Context, _ *adminpb.GetEntityOwnersRequest) (*adminpb.GetEntityOwnersResponse, error) {
	// No-PG path, matching the rest of the package.
	if s.pool == nil {
		return &adminpb.GetEntityOwnersResponse{}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT e.entity_id, e.introduced_by, e.introduced_at,
       COALESCE(f.cnt, 0) AS field_count
FROM atlantis.entity_lineage e
LEFT JOIN (
    SELECT entity_id, COUNT(*) AS cnt
    FROM atlantis.entity_lineage
    WHERE field_name != '' AND removed_at IS NULL
    GROUP BY entity_id
) f ON f.entity_id = e.entity_id
WHERE e.field_name = '' AND e.removed_at IS NULL
ORDER BY e.entity_id`)
	if err != nil {
		return nil, fmt.Errorf("query entity_lineage owners: %w", err)
	}
	defer rows.Close()

	var owners []EntityOwnerEntry
	for rows.Next() {
		var o EntityOwnerEntry
		if err := rows.Scan(&o.EntityID, &o.IntroducedBy, &o.IntroducedAt, &o.FieldCount); err != nil {
			return nil, err
		}
		owners = append(owners, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &adminpb.GetEntityOwnersResponse{Owners: ownersToPB(owners)}, nil
}

// ---------------------------------------------------------------------------
// RollbackSchema — revert to a prior version
// ---------------------------------------------------------------------------

type RollbackSchemaRequest struct {
	ToVersion int64  `json:"to_version"`
	Caller    string `json:"caller"`
}

type RollbackSchemaResponse struct {
	NewVersion int64  `json:"new_version"`
	UpSQL      string `json:"up_sql"`
}

func (s *Service) RollbackSchema(ctx context.Context, req *adminpb.RollbackSchemaRequest) (*adminpb.RollbackSchemaResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if req.GetToVersion() <= 0 {
		return nil, errors.New("admin: to_version must be a positive integer")
	}
	if req.GetCaller() == "" {
		return nil, errors.New("admin: caller identity is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same advisory lock as apply/adopt so rollback can't race.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	// Load target version's IR snapshot.
	var targetIRRaw []byte
	err = tx.QueryRow(ctx, `
SELECT ir_snapshot FROM atlantis.schema_versions WHERE version = $1`, req.GetToVersion()).Scan(&targetIRRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: schema version %d not found", req.GetToVersion())
		}
		return nil, fmt.Errorf("load target version: %w", err)
	}
	targetIR, err := dsl.DecodeJSONIR(targetIRRaw)
	if err != nil {
		return nil, fmt.Errorf("decode target IR: %w", err)
	}

	// Load current IR from ir_checkpoint.
	currentIR, err := s.loadCheckpointTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("load current checkpoint: %w", err)
	}
	if currentIR == nil {
		return nil, errors.New("admin: no current checkpoint to rollback from")
	}

	// Compute diff from current to target (so the SQL takes us from
	// current state to target state).
	codegen.AssignProtoNumbers(currentIR, targetIR)
	d := codegen.ComputeDiff(currentIR, targetIR)
	scripts, err := codegen.EmitSQL(currentIR, targetIR, d)
	if err != nil {
		return nil, fmt.Errorf("emit rollback sql: %w", err)
	}

	// Execute the rollback SQL.
	if scripts.Up != "" {
		if _, err := tx.Exec(ctx, scripts.Up); err != nil {
			return nil, fmt.Errorf("rollback apply: %w", err)
		}
	}

	parentVer := req.GetToVersion()
	version, err := s.persistCheckpoint(ctx, tx, targetIR, versionMeta{
		Caller:    req.GetCaller(),
		PlanClass: d.HighestClass().String(),
		Diff:      d,
		UpSQL:     scripts.Up,
		DownSQL:   scripts.Down,
		EventType: "rollback",
		ParentVer: &parentVer,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &adminpb.RollbackSchemaResponse{
		NewVersion: version,
		UpSql:      scripts.Up,
	}, nil
}

// ---------------------------------------------------------------------------
// PreviewRollback — compute the SQL a rollback would execute, without
//                   executing or persisting anything
// ---------------------------------------------------------------------------

type PreviewRollbackRequest struct {
	ToVersion int64 `json:"to_version"`
}

type PreviewRollbackResponse struct {
	TargetVersion  int64  `json:"target_version"`
	CurrentVersion int64  `json:"current_version"`
	UpSQL          string `json:"up_sql"` // SQL that would run on execute
	// Spelled the way codegen.ChangeClass.String() spells it, which is
	// HYPHENATED: additive / backfill-required / cross-caller-breaking /
	// destructive, plus "adopt" and "seed" for the two non-plan events. Note
	// this is not the underscored spelling the proto enum lowercases to —
	// /api/history proxies this column through untouched, so anything matching
	// on it has to accept the hyphens.
	PlanClass   string `json:"plan_class"`
	ChangeCount int    `json:"change_count"`
}

// PreviewRollback returns the SQL a RollbackSchema call would execute,
// plus its plan class and change count, without taking the advisory lock
// or writing anything. Same auth as RollbackSchema (operator-only) since
// the response reveals schema structure that's already operator-gated
// via every other admin RPC.
//
// Read race: a concurrent apply or rollback between this call and the
// real one will produce different SQL at execution time. Acceptable —
// the user clicks Execute after reviewing, and the real RPC recomputes
// from a fresh consistent snapshot inside its own transaction. The
// preview is informational; the executing call is authoritative.
func (s *Service) PreviewRollback(ctx context.Context, req *adminpb.PreviewRollbackRequest) (*adminpb.PreviewRollbackResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if req.GetToVersion() <= 0 {
		return nil, errors.New("admin: to_version must be a positive integer")
	}

	var targetIRRaw []byte
	err := s.pool.QueryRow(ctx, `
SELECT ir_snapshot FROM atlantis.schema_versions WHERE version = $1`, req.GetToVersion()).Scan(&targetIRRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("admin: schema version %d not found", req.GetToVersion())
		}
		return nil, fmt.Errorf("load target version: %w", err)
	}
	targetIR, err := dsl.DecodeJSONIR(targetIRRaw)
	if err != nil {
		return nil, fmt.Errorf("decode target IR: %w", err)
	}

	currentIR, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load current checkpoint: %w", err)
	}
	if currentIR == nil {
		return nil, errors.New("admin: no current checkpoint to rollback from")
	}

	var currentVersion int64
	_ = s.pool.QueryRow(ctx,
		`SELECT MAX(version) FROM atlantis.schema_versions`).Scan(&currentVersion)

	codegen.AssignProtoNumbers(currentIR, targetIR)
	d := codegen.ComputeDiff(currentIR, targetIR)
	scripts, err := codegen.EmitSQL(currentIR, targetIR, d)
	if err != nil {
		return nil, fmt.Errorf("emit rollback sql: %w", err)
	}

	return &adminpb.PreviewRollbackResponse{
		TargetVersion:  req.GetToVersion(),
		CurrentVersion: currentVersion,
		UpSql:          scripts.Up,
		PlanClass:      d.HighestClass().String(),
		ChangeCount:    int32(d.Len()),
	}, nil
}

// --- Wire conversion ---
//
// Two nullable columns cross as proto3 `optional`: parent_version is absent
// for the root schema version, and removed_at is absent while a field is still
// present. Both are *int64 on the JSON side with omitempty, so absent means
// the key disappears — mapping them to a plain 0 would invent a parent version
// 0 and a removal at the Unix epoch.

func schemaVersionSummariesToPB(in []SchemaVersionSummary) []*adminpb.SchemaVersionSummary {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.SchemaVersionSummary, 0, len(in))
	for _, v := range in {
		out = append(out, &adminpb.SchemaVersionSummary{
			Version: v.Version, Caller: v.Caller, PlanClass: v.PlanClass,
			EventType: v.EventType, ChangeCount: int32(v.ChangeCount),
			CreatedAt: v.CreatedAt, IrHash: v.IRHash,
		})
	}
	return out
}

func schemaVersionToPB(v *GetSchemaVersionResponse) *adminpb.GetSchemaVersionResponse {
	out := &adminpb.GetSchemaVersionResponse{
		Version: v.Version, Caller: v.Caller, PlanClass: v.PlanClass,
		EventType: v.EventType, Diff: v.Diff, UpSql: v.UpSQL, DownSql: v.DownSQL,
		IrSnapshot: v.IRSnapshot, CreatedAt: v.CreatedAt, IrHash: v.IRHash,
	}
	// Copy rather than alias, matching the FromPB direction. Assigning a nil
	// *int64 is a no-op, so a guard here would protect nothing.
	if v.ParentVer != nil {
		pv := *v.ParentVer
		out.ParentVersion = &pv
	}
	return out
}

func lineageToPB(in []EntityLineageEntry) []*adminpb.EntityLineageEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.EntityLineageEntry, 0, len(in))
	for _, e := range in {
		pb := &adminpb.EntityLineageEntry{
			EntityId: e.EntityID, FieldName: e.FieldName,
			IntroducedBy: e.IntroducedBy, IntroducedAt: e.IntroducedAt,
			LastModifiedBy: e.LastModifiedBy, LastModifiedAt: e.LastModifiedAt,
		}
		if e.RemovedAt != nil {
			ra := *e.RemovedAt
			pb.RemovedAt = &ra
		}
		out = append(out, pb)
	}
	return out
}

func ownersToPB(in []EntityOwnerEntry) []*adminpb.EntityOwnerEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.EntityOwnerEntry, 0, len(in))
	for _, o := range in {
		out = append(out, &adminpb.EntityOwnerEntry{
			EntityId: o.EntityID, IntroducedBy: o.IntroducedBy,
			IntroducedAt: o.IntroducedAt, FieldCount: int32(o.FieldCount),
		})
	}
	return out
}
