package codegen

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// The lock timeout must actually fire, not just appear in the text.
//
// # What it prevents
//
// ApplyMigration runs the whole script in one transaction. An ACCESS EXCLUSIVE
// request that cannot be granted QUEUES, and every later statement on that
// table — including plain SELECTs — queues behind the waiter. One unrelated
// long-running query then turns a millisecond migration into an outage for as
// long as it runs.
//
// So this holds a lock, runs the emitted migration against it, and asserts the
// migration gives up quickly instead of waiting. Asserting the SET LOCAL line
// is present would pass with the value set to a day.
func TestMigrationGivesUpWaitingForALock(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the migration lock timeout")
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.lt_doc CASCADE`) }
	drop()
	t.Cleanup(drop)
	if _, err := pool.Exec(ctx, `CREATE TABLE atlantis.lt_doc (id bigint primary key, body text)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// A holder that keeps ACCESS EXCLUSIVE for longer than the timeout.
	holder, err := pool.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE atlantis.lt_doc IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()

	// The emitted migration shape: the timeout, then DDL that needs the lock.
	var b sqlBuilder
	emitLockTimeout(&b)
	b.line(`ALTER TABLE atlantis.lt_doc ADD COLUMN IF NOT EXISTS extra text;`)

	tx, err := pool.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	start := time.Now()
	_, err = tx.Exec(ctx, b.String())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the migration acquired a lock that was held by someone else")
	}
	if !strings.Contains(err.Error(), "lock timeout") &&
		!strings.Contains(err.Error(), "canceling statement") {
		t.Fatalf("the migration failed for some other reason: %v", err)
	}
	// It must give up promptly. Without SET LOCAL lock_timeout this blocks for
	// as long as the holder runs, with every later statement queued behind it.
	if elapsed > 15*time.Second {
		t.Errorf("the migration waited %v for the lock; the timeout is %dms and "+
			"nothing bounds the queue behind it", elapsed, migrationLockTimeoutMS)
	}
	t.Logf("gave up after %v (timeout %dms)", elapsed, migrationLockTimeoutMS)
}

// Every script a caller runs inside a transaction must bound its lock wait.
//
// Needs no database, and sits beside the test above on purpose: the two are
// halves of one property. That one proves the timeout FIRES; this one proves it
// is PRESENT everywhere it has to be. Either alone is satisfied by a migration
// that still hangs.
//
// # The gap this closes
//
// emitLockTimeout went onto Up and Down, and the backfill path kept none. That
// is not a lesser path: PreBackfillUp carries ADD COLUMN and runs on the apply
// tx (internal/server/admin/backfill.go), PostBackfillUp carries ALTER COLUMN
// SET NOT NULL and runs on the worker's tx (internal/backfill/runner.go). Both
// take ACCESS EXCLUSIVE, so both could queue every reader behind one unrelated
// long-running query — the exact outage the timeout exists to prevent, reached
// by `tide apply --backfill` instead of `tide apply`.
//
// # Why reflection, and why the registry is not a list of what to check
//
// A test naming four scripts is satisfied when a fifth is added and forgotten.
// This walks the fields of SQLScripts and fails on any name it does not have a
// decision for, so a new script cannot ship without someone answering whether
// it runs in a transaction. The registry says which ANSWER each field got, not
// which fields to look at.
//
// The index scripts must NOT carry it, and that is asserted rather than
// skipped. SET LOCAL outside a transaction is accepted, warns, and does
// nothing — so copying the line there would read as protection and provide
// none, which is worse than the absence because it stops anybody looking again.
func TestEveryTransactionalScriptBoundsItsLockWait(t *testing.T) {
	type execKind int
	const (
		transactional execKind = iota // caller runs it inside a tx: must bound the wait
		outsideTx                     // runs outside a tx: SET LOCAL would be inert
		notAScript                    // not SQL at all
	)

	decided := map[string]execKind{
		"Up":                  transactional, // admin.ApplyMigration, one tx
		"Down":                transactional, // admin.RollbackSchema, one tx
		"PreBackfillUp":       transactional, // admin/backfill.go, the apply tx
		"PostBackfillUp":      transactional, // backfill/runner.go, the worker tx
		"PreBackfillIndexes":  outsideTx,     // CREATE INDEX CONCURRENTLY
		"PostBackfillIndexes": outsideTx,     // DROP INDEX CONCURRENTLY
		"BackfillFields":      notAScript,    // driver rows for the chunked UPDATE
	}

	typ := reflect.TypeOf(SQLScripts{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := decided[name]; !ok {
			t.Fatalf("SQLScripts.%s is new and no one has said whether it runs inside "+
				"a transaction. If it does it needs emitLockTimeout, or one unrelated "+
				"long-running query queues every reader on the table behind it. Add it "+
				"to this registry with the answer", name)
		}
	}

	// One plan that populates all six scripts at once. A backfilled NOT NULL
	// column is the only shape that fills the phase-split and the index scripts
	// together, which is why the fixture is this and not something smaller.
	oldIR := lower(t, `
entity User in ltc {
  id         bigint primary
  first_name text not null
  last_name  text not null
}
`)
	newIR := lower(t, `
entity User in ltc {
  id           bigint primary
  first_name   text not null
  last_name    text not null
  display_name text not null backfill "first_name || ' ' || last_name"
}
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	want := fmt.Sprintf("SET LOCAL lock_timeout = '%dms';", migrationLockTimeoutMS)
	val := reflect.ValueOf(scripts)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if decided[name] == notAScript {
			continue
		}
		body := val.Field(i).String()

		// Without this, an empty script passes the outsideTx arm for free and
		// fails the transactional arm for the wrong reason. Either way the
		// assertion below would be measuring the fixture, not the emitter.
		if body == "" {
			t.Errorf("SQLScripts.%s came out empty, so this fixture no longer "+
				"populates every script and the check below proves nothing about it",
				name)
			continue
		}

		switch got := strings.Contains(body, want); decided[name] {
		case transactional:
			if !got {
				t.Errorf("SQLScripts.%s runs inside a transaction and does not bound "+
					"its lock wait. Its DDL takes ACCESS EXCLUSIVE, and a request that "+
					"cannot be granted queues every later statement on the table behind "+
					"it — including plain SELECTs. Call emitLockTimeout when building it",
					name)
			}
		case outsideTx:
			if got {
				t.Errorf("SQLScripts.%s runs OUTSIDE a transaction, where SET LOCAL is "+
					"accepted, warns, and changes nothing. The line reads as a bound and "+
					"is not one. Drop it, or the next person to check will believe it",
					name)
			}
		}
	}
}
