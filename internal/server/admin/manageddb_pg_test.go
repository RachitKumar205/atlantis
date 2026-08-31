package admin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/secrets"
)

// The stored managed-database DSN, against a real database.
//
// The DSN carries a password, so what matters is that it goes in sealed, comes
// back only through the pool, and that a new one takes effect without a
// restart.

func managedFixture(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the managed-database store")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM atlantis.managed_database`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM atlantis.managed_database`)
	})

	keyset, err := secrets.NewKeyset()
	if err != nil {
		t.Fatalf("keyset: %v", err)
	}
	keys, err := secrets.FromEnvKeyset(keyset)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return New(pool, Config{Keys: keys}), pool
}

// Nothing stored means the server manages its own database, which is what
// every organisation that never adopted one does.
func TestManagedPoolFallsBackToControl(t *testing.T) {
	s, pool := managedFixture(t)

	got, err := s.managedPool(context.Background())
	if err != nil {
		t.Fatalf("managedPool: %v", err)
	}
	if got != pool {
		t.Error("with no row stored the managed pool is not the control pool; " +
			"a deployment that adopted nothing would plan against somewhere else")
	}
}

// The DSN is sealed. A password readable in the column is a password in every
// backup and every operator's psql session.
func TestManagedDSNIsSealedAtRest(t *testing.T) {
	s, pool := managedFixture(t)
	ctx := context.Background()

	const dsn = "postgres://u:hunter2@db.example.com:5432/app?sslmode=require"
	if err := s.setManagedDatabase(ctx, dsn, "db.example.com:5432", "tester"); err != nil {
		t.Fatalf("set: %v", err)
	}

	var sealed []byte
	var source string
	if err := pool.QueryRow(ctx,
		`SELECT dsn_sealed, source FROM atlantis.managed_database WHERE id = 1`).
		Scan(&sealed, &source); err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, leak := range []string{"hunter2", "postgres://", dsn} {
		if strings.Contains(string(sealed), leak) {
			t.Errorf("the stored value contains %q in clear", leak)
		}
	}
	if source != "db.example.com:5432" {
		t.Errorf("source = %q, want the host and port", source)
	}
}

// The column refuses a connection string, so a caller passing the DSN as the
// source writes no password anywhere readable.
func TestManagedSourceRefusesAConnectionString(t *testing.T) {
	s, _ := managedFixture(t)
	ctx := context.Background()

	const dsn = "postgres://u:p@db.example.com:5432/app"
	if err := s.setManagedDatabase(ctx, dsn, dsn, "tester"); err == nil {
		t.Error("a connection string was accepted as the source")
	}
}

// A new DSN takes effect without a restart: the version moves and the cached
// pool is rebuilt against it.
func TestSettingANewDSNBumpsTheVersion(t *testing.T) {
	s, pool := managedFixture(t)
	ctx := context.Background()

	if err := s.setManagedDatabase(ctx, "postgres://a@x:5432/d", "x:5432", "t"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	first, err := s.managedDatabaseRef(ctx)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}

	if err := s.setManagedDatabase(ctx, "postgres://a@y:5432/d", "y:5432", "t"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	second, err := s.managedDatabaseRef(ctx)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}

	if second.Version <= first.Version {
		t.Errorf("version went %d -> %d; a cached pool would keep pointing at "+
			"the old database", first.Version, second.Version)
	}
	if second.Source != "y:5432" {
		t.Errorf("source = %q, want y:5432", second.Source)
	}
	// Still one row: a second managed database would need a rule for which wins.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlantis.managed_database`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

// An environment DSN pins the managed database and is never reopened from the
// table, so a stored row cannot redirect a deployment that pinned one.
func TestEnvironmentDSNWinsOverTheStoredOne(t *testing.T) {
	s, pool := managedFixture(t)
	ctx := context.Background()

	pinned := poolSentinel()
	s.managed = pinned
	s.managedFromEnv = true

	if err := s.setManagedDatabase(ctx, "postgres://a@elsewhere:5432/d", "elsewhere:5432", "t"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.managedPool(ctx)
	if err != nil {
		t.Fatalf("managedPool: %v", err)
	}
	if got != pinned {
		t.Error("a stored DSN overrode ATL_MANAGED_PG_URL")
	}
	if got == pool {
		t.Error("the pinned pool was replaced by the control pool")
	}
}
