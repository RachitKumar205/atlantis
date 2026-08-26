package console

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Audit retention, against the real catalogue.
//
// This function had NO test and had never dropped a partition. It selected
// every relation with relispartition true whose name began audit_log_p, which
// matches the partitions' indexes as well as their tables, and scanned a NULL
// pg_get_expr into a string. The scan failed on the first index and threw away
// everything already read.
//
// The fixture below therefore contains three shapes a naive query gets wrong.
// Each is needed: a fixture with only a plain partition passes against code
// that is still broken.

// auditPartitions lists the audit_log partitions that currently exist.
func auditPartitions(t *testing.T, f *consoleFixture) map[string]bool {
	t.Helper()
	rows, err := f.srv.db.pool.Query(context.Background(), `
		SELECT c.relname
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		  JOIN pg_namespace n ON n.oid = p.relnamespace
		 WHERE n.nspname = 'console' AND p.relname = 'audit_log'`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = true
	}
	return out
}

func exec(t *testing.T, f *consoleFixture, sql string) {
	t.Helper()
	if _, err := f.srv.db.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// month names a partition the way ensureAuditPartition does.
func month(y int, m time.Month) string {
	return fmt.Sprintf("audit_log_p%04d%02d", y, m)
}

// A partition with an index on it is dropped.
//
// The index carries the case: it has relispartition true and relpartbound NULL,
// which is what a partition scan trips on, so a fixture without one passes
// against a broken query.
func TestAnOldPartitionIsDroppedDespiteItsIndexes(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	db := f.srv.db

	old := time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := db.ensureAuditPartition(ctx, old); err != nil {
		t.Fatalf("create the partition: %v", err)
	}
	name := month(2020, time.March)

	// The parent's indexes propagate to every partition automatically, which is
	// exactly why this bug bit every deployment: no partition can exist without
	// the index rows that broke the scan. Asserted rather than created, so the
	// fixture is verified to have the shape the bug needed.
	var indexes int
	if err := db.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'console' AND c.relkind = 'i'
		   AND c.relispartition = true AND c.relname LIKE $1`, name+"%").Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes == 0 {
		t.Fatal("the fixture has no partitioned index, so it cannot reproduce the bug")
	}

	dropped, err := db.dropAuditPartitionsOlderThan(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if len(dropped) == 0 {
		t.Fatal("the sweep reported nothing dropped")
	}
	if auditPartitions(t, f)[name] {
		t.Errorf("%s survived the sweep", name)
	}
}

// A partition that is itself partitioned is dropped, not skipped.
//
// relkind is 'p' for these, so filtering to 'r' — the obvious repair — would
// abandon it forever while dropping its children, whose bounds are sub-ranges
// the parent never had. DROP TABLE on a partitioned table takes its children
// with it without CASCADE.
func TestASubPartitionedPartitionIsDropped(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	parent := month(2020, time.April)
	exec(t, f, fmt.Sprintf(`
		CREATE TABLE console.%s PARTITION OF console.audit_log
		FOR VALUES FROM ('2020-04-01 00:00:00+00') TO ('2020-05-01 00:00:00+00')
		PARTITION BY RANGE (created_at)`, parent))
	exec(t, f, fmt.Sprintf(`
		CREATE TABLE console.%s_a PARTITION OF console.%s
		FOR VALUES FROM ('2020-04-01 00:00:00+00') TO ('2020-04-15 00:00:00+00')`, parent, parent))

	dropped, err := f.srv.db.dropAuditPartitionsOlderThan(ctx,
		time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}

	live := auditPartitions(t, f)
	if live[parent] {
		t.Errorf("the sub-partitioned %s survived — retention would abandon it forever", parent)
	}
	// And it was named as itself, not as its child. A sweep reporting a name
	// ensureAuditPartition never creates is a sweep working on the wrong level.
	var sawParent bool
	for _, d := range dropped {
		if d == parent {
			sawParent = true
		}
		if d == parent+"_a" {
			t.Errorf("the sweep dropped the grandchild %s directly", d)
		}
	}
	if !sawParent {
		t.Errorf("dropped = %v, want it to name %s", dropped, parent)
	}
}

// A partition inside the retention window is left alone.
//
// The control. Without it every test above passes against a sweep that drops
// everything it can see, which would delete the audit log.
func TestARecentPartitionSurvives(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	recent := time.Now().UTC().AddDate(0, -1, 0)
	if err := f.srv.db.ensureAuditPartition(ctx, recent); err != nil {
		t.Fatalf("create the partition: %v", err)
	}
	name := month(recent.Year(), recent.Month())

	// Cutoff a year ago: nothing recent may go.
	dropped, err := f.srv.db.dropAuditPartitionsOlderThan(ctx, time.Now().UTC().AddDate(-1, 0, 0))
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	for _, d := range dropped {
		if d == name {
			t.Fatalf("%s was dropped while inside the retention window", name)
		}
	}
	if !auditPartitions(t, f)[name] {
		t.Errorf("%s is gone", name)
	}
}

// The sweep works on a session that is not UTC/ISO.
//
// pg_get_expr renders the bound using the SESSION's TimeZone and DateStyle, so
// the version this replaces — which rendered the bound and parsed it back with
// two fixed Go layouts — matched neither `TO ('2026-09-01 05:30:00+05:30')` nor
// `TO ('01/09/2026 00:00:00 UTC')`. It then `continue`d, silently: no error, no
// log line, nothing dropped, forever.
//
// The test database defaults to UTC and ISO, so without this the bug is
// invisible. Comparing in SQL is what makes it impossible rather than untested.
func TestTheSweepIsNotAffectedByTheSessionsTimeZoneOrDateStyle(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	old := time.Date(2019, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := f.srv.db.ensureAuditPartition(ctx, old); err != nil {
		t.Fatalf("create the partition: %v", err)
	}
	name := month(2019, time.July)
	cutoff := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// One connection, held for the whole case, so the SET and the selection are
	// certainly the same session. ALTER DATABASE would cover the pool but needs
	// ownership the console role does not have — and this is the narrower
	// statement anyway: the extraction must not depend on how the bound is
	// rendered, whoever set that.
	for _, tc := range []struct{ name, set string }{
		{"a non-UTC timezone", "SET TimeZone = 'Asia/Kolkata'"},
		{"a non-ISO datestyle", "SET DateStyle = 'SQL, DMY'"},
		{"a German datestyle in Denver", "SET TimeZone = 'America/Denver'; SET DateStyle = 'German, DMY'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := f.srv.db.pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer conn.Release()

			if _, err := conn.Exec(ctx, tc.set); err != nil {
				t.Fatalf("%s: %v", tc.set, err)
			}
			// Reset before the connection returns to the pool, or the setting
			// leaks into whatever runs next.
			defer func() { _, _ = conn.Exec(ctx, "RESET ALL") }()

			// What the bound looks like under this session, for the failure
			// message — this is the string the old Go parse choked on.
			var rendered string
			if err := conn.QueryRow(ctx, `
				SELECT pg_get_expr(c.relpartbound, c.oid)
				  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = 'console' AND c.relname = $1`, name).Scan(&rendered); err != nil {
				t.Fatalf("read the bound: %v", err)
			}

			// The selection half of dropAuditPartitionsOlderThan, on this
			// session. If it finds the partition, the extraction is independent
			// of the rendering; the DROP needs no such proof.
			var found int
			if err := conn.QueryRow(ctx, `
				SELECT count(*)
				  FROM pg_inherits i
				  JOIN pg_class  c ON c.oid = i.inhrelid
				  JOIN pg_class  p ON p.oid = i.inhparent
				  JOIN pg_namespace n ON n.oid = p.relnamespace
				 WHERE n.nspname = 'console'
				   AND p.relname = 'audit_log'
				   AND c.relkind IN ('r', 'p')
				   AND c.relname = $1
				   AND (substring(pg_get_expr(c.relpartbound, c.oid) from $2))::timestamptz <= $3`,
				name, `TO \('([^']+)'\)`, cutoff).Scan(&found); err != nil {
				t.Fatalf("select: %v  (bound rendered as %s)", err, rendered)
			}
			if found != 1 {
				t.Errorf("the partition was not selected under %s\n  bound rendered as: %s",
					tc.set, rendered)
			}
		})
	}
}
