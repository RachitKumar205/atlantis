package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The database this server manages, when it is not the one it keeps its own
// tables in.
//
// An organisation that adopted an existing database has the tables already;
// what it needs is atlantis reading and planning against them rather than
// creating them somewhere else. The DSN is stored sealed in
// atlantis.managed_database and the pool is opened from it on first use.
//
// Cached and versioned rather than reopened per call: a pool per request would
// exhaust the managed database's connections, and a pool opened once would
// keep pointing at the old database after the DSN changed.

// managedAAD is the associated data the DSN is sealed with. Binding the
// ciphertext to its purpose stops a value lifted from another sealed column
// decrypting here.
const managedAAD = "managed_database"

// errNoManagedDatabase reports that this server manages its own database.
var errNoManagedDatabase = errors.New("admin: no managed database is configured")

// managedRef is what the table holds, without the secret.
type managedRef struct {
	Source  string
	Version int64
}

// managedState is the cached pool and the version it was opened at.
type managedState struct {
	mu      sync.Mutex
	pool    *pgxpool.Pool
	version int64
}

// managedPool returns the pool for the managed database, opening or reopening
// it as the stored DSN changes.
//
// Falls back to the control pool when nothing is stored and none was
// configured at boot, which is the single-database deployment.
func (s *Service) managedPool(ctx context.Context) (*pgxpool.Pool, error) {
	// A DSN in the environment wins and is never reopened: it is how a
	// deployment pins the managed database outside the database, and how the
	// tests point at one without a keyring.
	if s.managedFromEnv {
		return s.managed, nil
	}
	if s.pool == nil || s.keys == nil {
		return s.managed, nil
	}

	var sealed []byte
	var version int64
	err := s.pool.QueryRow(ctx, `
		SELECT dsn_sealed, version FROM atlantis.managed_database WHERE id = 1
	`).Scan(&sealed, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.pool, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed database: %w", err)
	}

	s.managedState.mu.Lock()
	defer s.managedState.mu.Unlock()
	if s.managedState.pool != nil && s.managedState.version == version {
		return s.managedState.pool, nil
	}

	dsn, err := s.keys.Decrypt(sealed, []byte(managedAAD))
	if err != nil {
		return nil, fmt.Errorf("unseal managed database dsn: %w", err)
	}
	pool, err := pgxpool.New(ctx, string(dsn))
	if err != nil {
		return nil, fmt.Errorf("open managed database: %w", err)
	}

	// Closed after the swap, so a request holding the old pool finishes on it.
	if old := s.managedState.pool; old != nil {
		go old.Close()
	}
	s.managedState.pool = pool
	s.managedState.version = version
	return pool, nil
}

// setManagedDatabase stores the DSN this server manages against.
//
// source must be host and port. The column refuses a connection string, so a
// caller passing the DSN twice fails the insert rather than writing a password
// somewhere readable.
func (s *Service) setManagedDatabase(ctx context.Context, dsn, source, setBy string) error {
	if s.keys == nil {
		return errors.New("admin: no keyring is configured, so a managed database DSN cannot be sealed")
	}
	sealed, err := s.keys.Encrypt([]byte(dsn), []byte(managedAAD))
	if err != nil {
		return fmt.Errorf("seal managed database dsn: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO atlantis.managed_database (id, dsn_sealed, source, set_by)
		VALUES (1, $1, $2, $3)
		ON CONFLICT (id) DO UPDATE
		   SET dsn_sealed = EXCLUDED.dsn_sealed,
		       source     = EXCLUDED.source,
		       set_by     = EXCLUDED.set_by,
		       version    = atlantis.managed_database.version + 1,
		       updated_at = now()
	`, sealed, source, setBy)
	if err != nil {
		return fmt.Errorf("store managed database: %w", err)
	}
	return nil
}

// managedDatabaseRef reports which database is managed, without the secret.
func (s *Service) managedDatabaseRef(ctx context.Context) (managedRef, error) {
	var r managedRef
	err := s.pool.QueryRow(ctx, `
		SELECT source, version FROM atlantis.managed_database WHERE id = 1
	`).Scan(&r.Source, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errNoManagedDatabase
	}
	return r, err
}

// SetManagedDatabase points this server at the database whose schema it
// manages.
//
// The DSN is sealed on arrival and never returned: the response carries the
// version and the host, which is what a console needs to say which database is
// managed without holding the credential.
func (s *Service) SetManagedDatabase(ctx context.Context, req *adminpb.SetManagedDatabaseRequest) (*adminpb.SetManagedDatabaseResponse, error) {
	dsn := strings.TrimSpace(req.GetDsn())
	if dsn == "" {
		return nil, errors.New("admin: a connection string is required")
	}
	source := strings.TrimSpace(req.GetSource())
	if source == "" {
		return nil, errors.New("admin: a source host is required")
	}
	// The column refuses a credential, and reaching it with one would be a
	// password in every backup. Checked here too so the refusal names the
	// field rather than surfacing as a constraint violation.
	if strings.Contains(source, "@") {
		return nil, errors.New("admin: source must be a host and port, not a connection string")
	}

	if err := s.setManagedDatabase(ctx, dsn, source, req.GetSetBy()); err != nil {
		return nil, err
	}
	ref, err := s.managedDatabaseRef(ctx)
	if err != nil {
		return nil, err
	}
	return &adminpb.SetManagedDatabaseResponse{Version: ref.Version, Source: ref.Source}, nil
}

// liveTx returns the connection the managed schema is read from, and a
// function to release it.
//
// ctl is the control transaction, returned unchanged when this server manages
// its own database — the single-database deployment, where one transaction
// reads both the atlantis schema and the tables it describes.
//
// Read-only: every caller of this introspects and writes nothing to the
// managed database, and Postgres enforces that rather than a reviewer.
func (s *Service) liveTx(ctx context.Context, ctl pgx.Tx) (pgx.Tx, func(), error) {
	pool, err := s.managedPool(ctx)
	if err != nil {
		return nil, nil, err
	}
	if pool == s.pool {
		return ctl, func() {}, nil
	}
	m, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin on the managed database: %w", err)
	}
	return m, func() { _ = m.Rollback(context.Background()) }, nil
}
