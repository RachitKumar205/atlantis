package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/rehearse"
	"github.com/rachitkumar205/atlantis/migrations"
)

// A rehearsal is clone → execute → destroy: the current schema and real rows
// copied into a disposable database, the plan's exact up_sql executed there
// inside a rolled-back transaction, and the verdict read off what Postgres
// did. Nothing here runs inside an apply's transaction — the gate consumes a
// verdict as one indexed row read, and the apply's own transactional DDL
// remains the final validator for anything that drifted since.

const (
	// rehearsalConsumeTTL bounds how old a verdict the gate accepts.
	rehearsalConsumeTTL = time.Hour

	// rehearsalRowTTL is how long a rehearsal stays listable.
	rehearsalRowTTL = 24 * time.Hour

	// rehearsalCloneTimeout caps one clone; the snapshot holds back vacuum
	// on the managed database for its duration.
	rehearsalCloneTimeout = 15 * time.Minute

	// rehearsalExecTimeout is the statement_timeout the up_sql runs under.
	rehearsalExecTimeout = 5 * time.Minute

	// rehearsalMaxBytes refuses cloning a managed database larger than this.
	rehearsalMaxBytes = 25 << 30

	// rehearsalOrgCap is the concurrent-clone ceiling across all callers.
	rehearsalOrgCap = 2

	// rehearsalClonePrefix names clone databases; the reaper sweeps by it.
	rehearsalClonePrefix = "atlantis_rehearsal_"
)

// rehearsalAAD binds the sealed target DSN to its purpose, like managedAAD.
const rehearsalAAD = "rehearsal_database"

// rehearsalTarget resolves where clones are created: the DSN configured at
// boot, else the sealed atlantis.rehearsal_database row, else the managed
// database's own cluster when its role may CREATE DATABASE.
func (s *Service) rehearsalTarget(ctx context.Context) (*pgx.ConnConfig, string, error) {
	if s.rehearsalTargetDSN != "" {
		cfg, err := pgx.ParseConfig(s.rehearsalTargetDSN)
		if err != nil {
			return nil, "", fmt.Errorf("admin: ATL_REHEARSAL_PG_URL: %w", err)
		}
		return cfg, "configured", nil
	}

	if s.keys != nil {
		var sealed []byte
		var source string
		err := s.pool.QueryRow(ctx, `
SELECT dsn_sealed, source FROM atlantis.rehearsal_database WHERE id = 1`).Scan(&sealed, &source)
		if err == nil {
			dsn, derr := s.keys.Decrypt(sealed, []byte(rehearsalAAD))
			if derr != nil {
				return nil, "", fmt.Errorf("admin: unseal rehearsal target: %w", derr)
			}
			cfg, perr := pgx.ParseConfig(string(dsn))
			if perr != nil {
				return nil, "", fmt.Errorf("admin: stored rehearsal target: %w", perr)
			}
			return cfg, source, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("admin: read rehearsal target: %w", err)
		}
	}

	// Same-cluster fallback. The managed pool's own connection settings,
	// pointed at its maintenance database, iff the role can create one —
	// otherwise the refusal names both remedies.
	managed, err := s.managedPool(ctx)
	if err != nil {
		return nil, "", err
	}
	var mayCreate bool
	if err := managed.QueryRow(ctx, `
SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&mayCreate); err != nil {
		return nil, "", fmt.Errorf("admin: check CREATEDB: %w", err)
	}
	if !mayCreate {
		return nil, "", status.Error(codes.FailedPrecondition,
			"admin: no rehearsal target is configured and the managed role cannot CREATE DATABASE. "+
				"Either grant CREATEDB to the managed role, or point rehearsals at a scratch "+
				"cluster (ATL_REHEARSAL_PG_URL, or the console's rehearsal target setting).")
	}
	return managed.Config().ConnConfig.Copy(), "managed", nil
}

// GetRehearsalDatabase reports where clones land, without the secret.
func (s *Service) GetRehearsalDatabase(ctx context.Context, _ *adminpb.GetRehearsalDatabaseRequest) (*adminpb.GetRehearsalDatabaseResponse, error) {
	out := &adminpb.GetRehearsalDatabaseResponse{PinnedByEnv: s.rehearsalTargetDSN != ""}
	if s.pool == nil {
		return out, nil
	}
	err := s.pool.QueryRow(ctx, `
SELECT source, version FROM atlantis.rehearsal_database WHERE id = 1`).
		Scan(&out.Source, &out.Version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read rehearsal target: %w", err)
	}
	return out, nil
}

// SetRehearsalDatabase seals and stores the target DSN, the
// SetManagedDatabase shape exactly.
func (s *Service) SetRehearsalDatabase(ctx context.Context, req *adminpb.SetRehearsalDatabaseRequest) (*adminpb.SetRehearsalDatabaseResponse, error) {
	dsn := strings.TrimSpace(req.GetDsn())
	if dsn == "" {
		return nil, errors.New("admin: a connection string is required")
	}
	source := strings.TrimSpace(req.GetSource())
	if source == "" {
		return nil, errors.New("admin: a source host is required")
	}
	if strings.Contains(source, "@") {
		return nil, errors.New("admin: source must be a host and port, not a connection string")
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return nil, fmt.Errorf("admin: the connection string does not parse: %w", err)
	}
	if s.keys == nil {
		return nil, errors.New("admin: no keyring is configured, so a rehearsal target DSN cannot be sealed")
	}
	sealed, err := s.keys.Encrypt([]byte(dsn), []byte(rehearsalAAD))
	if err != nil {
		return nil, fmt.Errorf("seal rehearsal target: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO atlantis.rehearsal_database (id, dsn_sealed, source, set_by)
VALUES (1, $1, $2, $3)
ON CONFLICT (id) DO UPDATE
   SET dsn_sealed = EXCLUDED.dsn_sealed,
       source     = EXCLUDED.source,
       set_by     = EXCLUDED.set_by,
       version    = atlantis.rehearsal_database.version + 1,
       set_at     = now()`, sealed, source, req.GetSetBy()); err != nil {
		return nil, fmt.Errorf("store rehearsal target: %w", err)
	}
	var version int64
	var storedSource string
	if err := s.pool.QueryRow(ctx, `
SELECT version, source FROM atlantis.rehearsal_database WHERE id = 1`).
		Scan(&version, &storedSource); err != nil {
		return nil, err
	}
	return &adminpb.SetRehearsalDatabaseResponse{Version: version, Source: storedSource}, nil
}

// rehearsalSetup builds the clone's schema in two layers: the embedded infra
// migration tree, then the DDL an empty-to-current emit produces — the same
// trick the embedded sandbox uses, so the entity schema is byte-equivalent
// to what applies built.
//
// The migrations come first because emitted SQL references what they create:
// a destructive change records into atlantis.parked_objects, and a
// `partition by` policy calls atlantis.current_partition(). Without them the
// rehearsal reports structural failures the real apply cannot produce.
func rehearsalSetup(prior *dsl.IR) (func(context.Context, *pgx.Conn) error, error) {
	if prior == nil {
		prior = &dsl.IR{}
	}
	empty := &dsl.IR{}
	codegen.AssignProtoNumbers(empty, prior)
	d := codegen.ComputeDiff(empty, prior)
	scripts, err := codegen.EmitSQL(empty, prior, d)
	if err != nil {
		return nil, fmt.Errorf("admin: emit clone schema: %w", err)
	}
	ddl := rehearsalSchemaPreamble(prior) + scripts.Up

	steps, err := infraMigrationSteps()
	if err != nil {
		return nil, err
	}

	ir := prior
	return func(ctx context.Context, conn *pgx.Conn) error {
		for _, step := range steps {
			if _, err := conn.Exec(ctx, step.sql); err != nil {
				return fmt.Errorf("bootstrap %s: %w", step.name, err)
			}
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := prepareExtensions(ctx, tx, ir); err != nil {
			return err
		}
		if strings.TrimSpace(ddl) != "" {
			if _, err := tx.Exec(ctx, ddl); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}, nil
}

type migrationStep struct{ name, sql string }

// infraMigrationSteps reads the embedded infra up-migrations in order. The
// clone is disposable, so no version table is kept — the whole tree runs on
// an empty database, which is exactly the state CI's migrate check proves
// the tree handles.
func infraMigrationSteps() ([]migrationStep, error) {
	entries, err := migrations.Infra.ReadDir("infra")
	if err != nil {
		return nil, fmt.Errorf("admin: read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]migrationStep, 0, len(names))
	for _, name := range names {
		raw, err := migrations.Infra.ReadFile("infra/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migrationStep{name: name, sql: string(raw)})
	}
	return out, nil
}

// rehearsalSchemaPreamble emits CREATE SCHEMA IF NOT EXISTS for every schema
// the IR references. EmitSQL elides "atlantis", which production bootstraps
// and a fresh clone does not.
func rehearsalSchemaPreamble(ir *dsl.IR) string {
	seen := map[string]bool{"atlantis": true}
	for i := range ir.Entities {
		schema := "atlantis"
		if tn := ir.Entities[i].TableName; tn != "" {
			if j := strings.IndexByte(tn, '.'); j > 0 {
				schema = tn[:j]
			}
		}
		seen[schema] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %q;\n", n)
	}
	return b.String()
}

// rehearsalContent is the change under rehearsal, resolved from either mode.
type rehearsalContent struct {
	Caller    string
	PlanID    string
	FilesHash string
	BaseHash  string
	UpSQL     string
}

// RehearseMigration clones the managed database, executes the change's
// up_sql there, records the verdict bound to the plan id, and destroys the
// clone. See RehearseMigrationRequest for the two addressing modes.
func (s *Service) RehearseMigration(ctx context.Context, req *adminpb.RehearseMigrationRequest) (*adminpb.RehearseMigrationResponse, error) {
	if err := s.requireMutablePlane("rehearsal"); err != nil {
		return nil, err
	}

	var content rehearsalContent
	switch {
	case req.GetPlanId() != "":
		plan, found, err := loadSchemaPlan(ctx, s.pool, req.GetPlanId())
		if err != nil {
			return nil, fmt.Errorf("load plan %s: %w", req.GetPlanId(), err)
		}
		if !found {
			return nil, status.Errorf(codes.NotFound, "admin: no plan %s", req.GetPlanId())
		}
		content = rehearsalContent{
			Caller: plan.Caller, PlanID: plan.PlanID,
			FilesHash: plan.FilesHash, BaseHash: plan.BaseHash, UpSQL: plan.UpSQL,
		}

	case len(req.GetFiles()) > 0:
		if req.GetCaller() == "" {
			return nil, status.Error(codes.InvalidArgument, "admin: caller is required with files")
		}
		// The submitting identity rehearses its own schema, the same binding
		// apply enforces.
		if err := s.bindCallerIdentity(ctx, req.GetCaller()); err != nil {
			return nil, err
		}
		resp, err := s.PlanSchema(ctx, &adminpb.PlanSchemaRequest{
			Caller: req.GetCaller(), Files: req.GetFiles(),
		})
		if err != nil {
			return nil, err
		}
		if len(resp.GetParseErrors()) > 0 || len(resp.GetCustomSqlErrors()) > 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"admin: the schema does not plan, so there is nothing to rehearse: %s",
				strings.Join(append(resp.GetParseErrors(), resp.GetCustomSqlErrors()...), "; "))
		}
		content = rehearsalContent{
			Caller: req.GetCaller(), PlanID: resp.GetPlanId(),
			FilesHash: filesHash(submittedFilesFromPB(req.GetFiles())),
			UpSQL:     resp.GetUpSql(),
		}

	default:
		return nil, status.Error(codes.InvalidArgument,
			"admin: rehearse either files or a plan_id")
	}

	if strings.TrimSpace(content.UpSQL) == "" {
		return nil, status.Error(codes.InvalidArgument,
			"admin: the change emits no SQL, so there is nothing to rehearse")
	}

	// Refusals from here on are verdicts, not errors: quota pressure and an
	// unusable target must never fabricate a pass, and must never look like
	// the server being broken to a pipeline branching on codes.
	unverified := func(reason string) (*adminpb.RehearseMigrationResponse, error) {
		id, err := s.recordRehearsal(ctx, content, rehearsalRecord{Verdict: "unverified", Reason: reason})
		if err != nil {
			return nil, err
		}
		return &adminpb.RehearseMigrationResponse{
			RehearsalId: id, Verdict: "unverified", Reason: reason,
		}, nil
	}

	target, targetSource, err := s.rehearsalTarget(ctx)
	if err != nil {
		return nil, err
	}
	managed, err := s.managedPool(ctx)
	if err != nil {
		return nil, err
	}
	if targetSource != "managed" {
		if err := rehearse.CheckTarget(ctx, managed, target); err != nil {
			return unverified("target_mismatch: " + err.Error())
		}
	}

	// The org-wide cap, enforced through the registry: the row is written
	// before the database exists, so the count covers clones mid-build.
	cloneID, err := mintRehearsalID()
	if err != nil {
		return nil, err
	}
	dbName := rehearsalClonePrefix + cloneID
	var inFlight int
	if err := s.pool.QueryRow(ctx, `
WITH ins AS (
    INSERT INTO atlantis.rehearsal_clones (clone_id, db_name, caller, target_source, expires_at)
    SELECT $1, $2, $3, $4, now() + $5::interval
    WHERE (SELECT count(*) FROM atlantis.rehearsal_clones WHERE state = 'creating') < $6
    RETURNING 1
) SELECT count(*) FROM ins`,
		cloneID, dbName, content.Caller, targetSource,
		fmt.Sprintf("%d seconds", int((2*rehearsalCloneTimeout).Seconds())),
		rehearsalOrgCap).Scan(&inFlight); err != nil {
		return nil, fmt.Errorf("register clone: %w", err)
	}
	if inFlight == 0 {
		return unverified("quota: the deployment's concurrent-rehearsal cap is in use")
	}
	dropClone := func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := rehearse.Drop(dctx, target, dbName); err == nil {
			_, _ = s.pool.Exec(dctx,
				`UPDATE atlantis.rehearsal_clones SET state = 'dropped' WHERE clone_id = $1`, cloneID)
		}
	}
	defer dropClone()

	prior, err := s.loadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	setup, err := rehearsalSetup(prior)
	if err != nil {
		return nil, err
	}

	cloneStart := time.Now()
	err = rehearse.Clone(ctx, managed, target, dbName, setup, rehearse.Options{
		CloneTimeout: rehearsalCloneTimeout,
		MaxBytes:     rehearsalMaxBytes,
	})
	cloneMs := time.Since(cloneStart).Milliseconds()
	if err != nil {
		switch {
		case errors.Is(err, rehearse.ErrTooLarge):
			return unverified("too_large: " + err.Error())
		case errors.Is(err, context.DeadlineExceeded):
			return unverified("clone_timeout")
		default:
			return unverified("clone_failed: " + err.Error())
		}
	}

	execStart := time.Now()
	out, err := rehearse.Execute(ctx, target, dbName, content.UpSQL, rehearsalExecTimeout)
	execMs := time.Since(execStart).Milliseconds()
	if err != nil {
		return unverified("execute_failed: " + err.Error())
	}

	rec := verdictFromOutcome(out)
	rec.CloneMs, rec.ExecuteMs = cloneMs, execMs
	if rec.Verdict == "fail_data" && out.PgErr != nil {
		rec.Diagnostics, rec.Remediation = s.rehearsalDiagnostics(ctx, target, dbName, out)
	}

	id, err := s.recordRehearsal(ctx, content, rec)
	if err != nil {
		return nil, err
	}
	return &adminpb.RehearseMigrationResponse{
		RehearsalId: id,
		Verdict:     rec.Verdict,
		Reason:      rec.Reason,
		Sqlstate:    rec.SQLState,
		Error:       rec.ErrorMessage,
		Diagnostics: rec.Diagnostics,
		Remediation: rec.Remediation,
		CloneMs:     cloneMs,
		ExecuteMs:   execMs,
	}, nil
}

// rehearsalRecord is what one rehearsal writes.
type rehearsalRecord struct {
	Verdict      string
	Reason       string
	SQLState     string
	ErrorMessage string
	ErrorDetail  string
	Diagnostics  map[string]int64
	Remediation  string
	CloneMs      int64
	ExecuteMs    int64
}

// verdictFromOutcome maps what Postgres did to the five-verdict model.
//
// unverified never collapses into pass: a timeout or resource failure says
// the rehearsal could not answer, and an auto tier treats that as "no".
func verdictFromOutcome(out rehearse.Outcome) rehearsalRecord {
	if out.Err == nil {
		return rehearsalRecord{Verdict: "pass"}
	}
	rec := rehearsalRecord{}
	if out.PgErr == nil {
		rec.Verdict, rec.Reason = "unverified", "execute_failed: "+out.Err.Error()
		return rec
	}
	rec.SQLState = out.PgErr.Code
	rec.ErrorMessage = redactedPgError(out.PgErr)
	rec.ErrorDetail = strings.TrimSpace(strings.TrimSpace(out.PgErr.Detail) + "\n" + strings.TrimSpace(out.PgErr.Hint))
	switch {
	case out.TimedOut:
		rec.Verdict, rec.Reason = "unverified", "execute_timeout"
	case strings.HasPrefix(out.PgErr.Code, "23"), strings.HasPrefix(out.PgErr.Code, "22"):
		rec.Verdict = "fail_data"
	case strings.HasPrefix(out.PgErr.Code, "53"), strings.HasPrefix(out.PgErr.Code, "57"):
		rec.Verdict, rec.Reason = "unverified", "resources: "+out.PgErr.Code
	default:
		// 42xxx and everything else structural: the SQL itself does not run.
		rec.Verdict = "fail_structural"
	}
	return rec
}

// redactedPgError keeps SQLSTATE, message and the object names, and drops
// DETAIL and HINT — the lines that embed row values.
func redactedPgError(e *pgconn.PgError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SQLSTATE %s: %s", e.Code, e.Message)
	if e.ConstraintName != "" {
		fmt.Fprintf(&b, " (constraint %s)", e.ConstraintName)
	}
	if e.TableName != "" {
		b.WriteString(" on " + e.TableName)
		if e.ColumnName != "" {
			b.WriteString("." + e.ColumnName)
		}
	}
	return b.String()
}

// rehearsalDiagnostics counts the violating rows for the failures worth
// counting, on the clone's pre-migration data, and renders the remediation
// template the server knows.
func (s *Service) rehearsalDiagnostics(ctx context.Context, target *pgx.ConnConfig, dbName string, out rehearse.Outcome) (map[string]int64, string) {
	e := out.PgErr
	if e == nil || e.TableName == "" || e.ColumnName == "" {
		return nil, ""
	}
	rel := pgx.Identifier{e.SchemaName, e.TableName}.Sanitize()
	col := pgx.Identifier{e.ColumnName}.Sanitize()
	switch e.Code {
	case "23502": // not_null_violation
		n, err := rehearse.CountWhere(ctx, target, dbName, rel, col+" IS NULL")
		if err != nil {
			return nil, ""
		}
		key := e.TableName + "." + e.ColumnName + " null"
		remediation := fmt.Sprintf(
			"%d rows hold NULL in %s.%s. Add a `default`, or a backfill: `%s <type> not null backfill \"<expression>\"`.",
			n, e.TableName, e.ColumnName, e.ColumnName)
		return map[string]int64{key: n}, remediation
	}
	return nil, ""
}

// recordRehearsal writes the verdict row and denormalizes it onto the plan.
func (s *Service) recordRehearsal(ctx context.Context, c rehearsalContent, rec rehearsalRecord) (string, error) {
	id, err := mintRehearsalID()
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO atlantis.rehearsals
    (rehearsal_id, plan_id, caller, files_hash, base_dependency_hash, verdict, reason,
     sqlstate, error_message, error_detail, diagnostics, remediation, clone_ms, execute_ms, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, '{}'::jsonb), $12, $13, $14, now() + $15::interval)`,
		id, c.PlanID, c.Caller, c.FilesHash, c.BaseHash, rec.Verdict, rec.Reason,
		rec.SQLState, rec.ErrorMessage, rec.ErrorDetail, rec.Diagnostics, rec.Remediation,
		rec.CloneMs, rec.ExecuteMs,
		fmt.Sprintf("%d seconds", int(rehearsalRowTTL.Seconds()))); err != nil {
		return "", fmt.Errorf("record rehearsal: %w", err)
	}
	_, _ = s.pool.Exec(ctx, `
UPDATE atlantis.schema_plans SET rehearsal_id = $2, verdict = $3 WHERE plan_id = $1`,
		c.PlanID, id, rec.Verdict)
	return id, nil
}

func mintRehearsalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// markRehearsalOutcome reconciles the newest consumable verdict with what
// the apply actually did — the record the plan calls honest.
func (s *Service) markRehearsalOutcome(ctx context.Context, planID, outcome string) {
	_, _ = s.pool.Exec(ctx, `
UPDATE atlantis.rehearsals SET outcome = $2
 WHERE rehearsal_id = (
     SELECT rehearsal_id FROM atlantis.rehearsals
      WHERE plan_id = $1 AND created_at > now() - $3::interval
      ORDER BY created_at DESC LIMIT 1)`,
		planID, outcome, fmt.Sprintf("%d seconds", int(rehearsalConsumeTTL.Seconds())))
}

func (s *Service) ListRehearsals(ctx context.Context, req *adminpb.ListRehearsalsRequest) (*adminpb.ListRehearsalsResponse, error) {
	if s.pool == nil {
		return &adminpb.ListRehearsalsResponse{}, nil
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT rehearsal_id, caller, files_hash, verdict, reason, sqlstate, error_message,
       outcome, created_at, expires_at, clone_ms, execute_ms
FROM atlantis.rehearsals
WHERE ($1 = '' OR caller = $1)
ORDER BY created_at DESC LIMIT $2`, req.GetCaller(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &adminpb.ListRehearsalsResponse{}
	for rows.Next() {
		r, err := scanRehearsalSummary(rows)
		if err != nil {
			return nil, err
		}
		out.Rehearsals = append(out.Rehearsals, r)
	}
	return out, rows.Err()
}

func (s *Service) GetRehearsal(ctx context.Context, req *adminpb.GetRehearsalRequest) (*adminpb.GetRehearsalResponse, error) {
	if s.pool == nil {
		return nil, status.Error(codes.NotFound, "admin: no rehearsals")
	}
	rows, err := s.pool.Query(ctx, `
SELECT rehearsal_id, caller, files_hash, verdict, reason, sqlstate, error_message,
       outcome, created_at, expires_at, clone_ms, execute_ms
FROM atlantis.rehearsals WHERE rehearsal_id = $1`, req.GetRehearsalId())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, status.Errorf(codes.NotFound, "admin: no rehearsal %s", req.GetRehearsalId())
	}
	summary, err := scanRehearsalSummary(rows)
	if err != nil {
		return nil, err
	}
	rows.Close()

	var detail, remediation string
	var diagnostics map[string]int64
	if err := s.pool.QueryRow(ctx, `
SELECT error_detail, remediation, diagnostics FROM atlantis.rehearsals WHERE rehearsal_id = $1`,
		req.GetRehearsalId()).Scan(&detail, &remediation, &diagnostics); err != nil {
		return nil, err
	}
	return &adminpb.GetRehearsalResponse{
		Rehearsal:   summary,
		ErrorDetail: detail,
		Remediation: remediation,
		Diagnostics: diagnostics,
	}, nil
}

func scanRehearsalSummary(rows pgx.Rows) (*adminpb.RehearsalSummary, error) {
	r := &adminpb.RehearsalSummary{}
	var createdAt, expiresAt time.Time
	if err := rows.Scan(&r.RehearsalId, &r.Caller, &r.FilesHash, &r.Verdict, &r.Reason,
		&r.Sqlstate, &r.Error, &r.Outcome, &createdAt, &expiresAt,
		&r.CloneMs, &r.ExecuteMs); err != nil {
		return nil, err
	}
	r.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	r.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	return r, nil
}

// StartRehearsalReaper runs the clone reaper until ctx ends: expired or
// orphaned registry rows are dropped, and any database wearing the clone
// prefix without a live registry row goes too — the case a control database
// restored from backup no longer remembers. Correctness never depends on
// this loop; a leaked clone costs disk.
func (s *Service) StartRehearsalReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapRehearsalClones(ctx)
			}
		}
	}()
}

func (s *Service) reapRehearsalClones(ctx context.Context) {
	target, _, err := s.rehearsalTarget(ctx)
	if err != nil {
		return
	}
	rows, err := s.pool.Query(ctx, `
SELECT clone_id, db_name FROM atlantis.rehearsal_clones
WHERE state = 'creating' AND expires_at < now()`)
	if err != nil {
		return
	}
	type c struct{ id, db string }
	var expired []c
	for rows.Next() {
		var e c
		if rows.Scan(&e.id, &e.db) == nil {
			expired = append(expired, e)
		}
	}
	rows.Close()
	for _, e := range expired {
		if rehearse.Drop(ctx, target, e.db) == nil {
			_, _ = s.pool.Exec(ctx,
				`UPDATE atlantis.rehearsal_clones SET state = 'dropped' WHERE clone_id = $1`, e.id)
		}
	}

	names, err := rehearse.ListClones(ctx, target, rehearsalClonePrefix)
	if err != nil {
		return
	}
	for _, name := range names {
		var live bool
		if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM atlantis.rehearsal_clones WHERE db_name = $1 AND state = 'creating')`,
			name).Scan(&live); err != nil || live {
			continue
		}
		_ = rehearse.Drop(ctx, target, name)
	}
}
