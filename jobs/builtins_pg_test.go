package jobs

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RegisterBuiltins must do both halves: register the handler AND install the
// schedule.
//
// Neither half had a test. builtins_test.go reads the LIST via the AST and
// builtinjobs_test.go reads the CALL SITE via the AST; the function between
// them was unexamined, and replacing its whole body with `return nil` passed
// every test in the repo. That is the same shape as the bug it was written
// about — the sweeper's RegisterSweeper was also correct and also never had
// its effect checked.
func TestRegisterBuiltinsRegistersAndSchedules(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise built-in registration")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	names := make([]string, 0, len(Builtins()))
	for _, b := range Builtins() {
		names = append(names, b.Name)
	}
	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = ANY($1)`, names)
	}
	clean()
	t.Cleanup(clean)

	reg := NewRegistry()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := RegisterBuiltins(ctx, reg, pool, logger); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}

	for _, b := range Builtins() {
		if h := reg.Lookup(b.Name); h == nil {
			t.Errorf("%s is not in the registry after RegisterBuiltins. A worker "+
				"that claimed the job would fail with handler-not-registered", b.Name)
		}
		var cron string
		if err := pool.QueryRow(ctx,
			`SELECT cron_spec FROM atlantis.job_schedules WHERE job_name = $1`,
			b.Name).Scan(&cron); err != nil {
			t.Errorf("%s has no schedule row, so it is registered and will never "+
				"fire: %v", b.Name, err)
			continue
		}
		if cron != b.CronSpec {
			t.Errorf("%s scheduled as %q, want %q", b.Name, cron, b.CronSpec)
		}
	}

	// An operator who retuned the cadence or disabled the job must not have it
	// silently reset on the next deploy.
	if _, err := pool.Exec(ctx,
		`UPDATE atlantis.job_schedules SET cron_spec = '@yearly', enabled = false
		  WHERE job_name = $1`, ReapParkedJobName); err != nil {
		t.Fatalf("operator edit: %v", err)
	}
	if err := RegisterBuiltins(ctx, NewRegistry(), pool, logger); err != nil {
		t.Fatalf("second RegisterBuiltins: %v", err)
	}
	var cron string
	var enabled bool
	if err := pool.QueryRow(ctx,
		`SELECT cron_spec, enabled FROM atlantis.job_schedules WHERE job_name = $1`,
		ReapParkedJobName).Scan(&cron, &enabled); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if cron != "@yearly" || enabled {
		t.Errorf("a deploy overwrote an operator's schedule edit (cron=%q enabled=%v). "+
			"Whoever disabled the job did so on purpose, and a reset that looks "+
			"exactly like the job never having been touched is how it comes back "+
			"without anybody deciding it should", cron, enabled)
	}
}
