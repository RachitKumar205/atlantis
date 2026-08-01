package jobs

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) *cronSchedule {
	t.Helper()
	c, err := parseCron(spec)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", spec, err)
	}
	return c
}

func at(s string) time.Time {
	ts, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return ts.UTC()
}

func TestCronNext(t *testing.T) {
	for _, tc := range []struct{ spec, from, want string }{
		{"* * * * *", "2026-03-01 10:00", "2026-03-01 10:01"},
		{"17 * * * *", "2026-03-01 10:00", "2026-03-01 10:17"},
		{"17 * * * *", "2026-03-01 10:17", "2026-03-01 11:17"},
		{"0 3 * * *", "2026-03-01 10:00", "2026-03-02 03:00"},
		{"*/15 * * * *", "2026-03-01 10:07", "2026-03-01 10:15"},
		{"*/15 * * * *", "2026-03-01 10:45", "2026-03-01 11:00"},
		{"0 0 1 * *", "2026-03-15 10:00", "2026-04-01 00:00"},
		{"@hourly", "2026-03-01 10:30", "2026-03-01 11:00"},
		{"@daily", "2026-03-01 10:30", "2026-03-02 00:00"},
		{"@weekly", "2026-03-04 10:30", "2026-03-08 00:00"},    // 2026-03-08 is a Sunday
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"}, // next leap year
		{"30 9 * * 1", "2026-03-04 10:00", "2026-03-09 09:30"}, // next Monday
		{"0 12 1,15 * *", "2026-03-02 00:00", "2026-03-15 12:00"},
		{"0 0 * * *", "2026-12-31 12:00", "2027-01-01 00:00"}, // year rollover
	} {
		got, err := mustParse(t, tc.spec).next(at(tc.from))
		if err != nil {
			t.Errorf("%q from %s: %v", tc.spec, tc.from, err)
			continue
		}
		if want := at(tc.want); !got.Equal(want) {
			t.Errorf("%q from %s = %s, want %s", tc.spec, tc.from,
				got.Format("2006-01-02 15:04"), tc.want)
		}
	}
}

// Both 0 and 7 are Sunday. A spec using 7 that silently matched nothing would
// be a job that never fires.
func TestCronSundayIsZeroAndSeven(t *testing.T) {
	zero, _ := mustParse(t, "0 0 * * 0").next(at("2026-03-04 00:00"))
	seven, _ := mustParse(t, "0 0 * * 7").next(at("2026-03-04 00:00"))
	if !zero.Equal(seven) {
		t.Errorf("dow 0 gives %s but dow 7 gives %s; both mean Sunday", zero, seven)
	}
	if zero.Weekday() != time.Sunday {
		t.Errorf("dow 0 resolved to a %s", zero.Weekday())
	}
}

// When BOTH day fields are restricted, cron fires when EITHER matches. This is
// the rule people get wrong, and getting it wrong means a job fires far more
// or far less often than its author intended.
func TestCronDayOfMonthOrDayOfWeek(t *testing.T) {
	// The 13th, and every Friday — not "Friday the 13th".
	c := mustParse(t, "0 0 13 * 5")

	// 2026-03-13 IS a Friday, so start after it to separate the two rules.
	// 2026-03-20 is a Friday and not the 13th: it must still match.
	got, err := c.next(at("2026-03-14 00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if want := at("2026-03-20 00:00"); !got.Equal(want) {
		t.Errorf("next = %s, want %s. With both day fields restricted the match "+
			"is dom OR dow; requiring both would skip every Friday that is not "+
			"the 13th", got.Format("2006-01-02 15:04"), want.Format("2006-01-02 15:04"))
	}

	// And the 13th of a month whose 13th is not a Friday must also match.
	got, err = c.next(at("2026-04-01 00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Day() != 3 && got.Day() != 10 && got.Day() != 13 {
		t.Errorf("next after 2026-04-01 = %s; expected a Friday or the 13th",
			got.Format("2006-01-02"))
	}

	// With only one restricted, it is that one alone.
	domOnly := mustParse(t, "0 0 13 * *")
	got, _ = domOnly.next(at("2026-03-14 00:00"))
	if got.Day() != 13 {
		t.Errorf("dom-only next = %s, want the 13th", got.Format("2006-01-02"))
	}
}

// A spec the parser cannot understand must be an error, never a schedule that
// silently matches nothing.
func TestCronRejectsBadSpecs(t *testing.T) {
	for _, spec := range []string{
		"",
		"not a cron spec at all",
		"* * * *",     // four fields
		"* * * * * *", // six fields (quartz-style seconds)
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day-of-month is 1-based
		"* * 32 * *",  // day-of-month out of range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // day-of-week out of range
		"*/0 * * * *", // zero step
		"5-1 * * * *", // inverted range
		"@never",      // unknown macro
		"a * * * *",   // non-numeric
	} {
		if c, err := parseCron(spec); err == nil {
			t.Errorf("parseCron(%q) accepted the spec and produced %+v. An "+
				"unparseable spec must be rejected loudly, because the "+
				"alternative is a schedule row that exists and never fires", spec, c)
		}
	}
}

// An unsatisfiable-but-parseable spec must terminate rather than search
// forever.
func TestCronUnsatisfiableSpecTerminates(t *testing.T) {
	// February 30th never happens.
	c := mustParse(t, "0 0 30 2 *")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.next(at("2026-03-01 00:00")); err == nil {
			t.Error("an unsatisfiable spec reported a firing time")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("next() did not terminate on an unsatisfiable spec; the scheduler " +
			"would hang on a single bad row and stop evaluating every other schedule")
	}
}

// The built-in schedules must parse. A built-in with a typo'd spec is a job
// that is registered, scheduled, and silently never runs.
func TestBuiltinCronSpecsParse(t *testing.T) {
	for _, b := range Builtins() {
		c, err := parseCron(b.CronSpec)
		if err != nil {
			t.Errorf("%s has cron spec %q, which the scheduler cannot parse, so "+
				"it will never fire: %v", b.Name, b.CronSpec, err)
			continue
		}
		if _, err := c.next(time.Now().UTC()); err != nil {
			t.Errorf("%s (%q) has no next firing time: %v", b.Name, b.CronSpec, err)
		}
	}
}
