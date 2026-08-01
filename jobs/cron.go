package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A five-field cron parser, written here rather than taken as a dependency.
//
// The parsed surface is small and completely specified — minute, hour,
// day-of-month, month, day-of-week, plus the @-macros — and it is the only
// thing standing between a schedule row and a job that never fires. Cron
// specs come from operators editing atlantis.job_schedules and, in time, from
// the DSL's `schedule "..."` modifier, so a spec this cannot parse must be a
// loud error rather than a silent no-fire.
//
// The one rule that surprises people, and the reason a hand-rolled parser has
// to be deliberate: when BOTH day-of-month and day-of-week are restricted, a
// fire happens when EITHER matches, not both. `0 0 13 * 5` is "midnight on the
// 13th, and also every Friday" — not "Friday the 13th". This matches Vixie
// cron and every implementation that followed it.

type cronSchedule struct {
	minute, hour, dom, month, dow uint64

	// Whether each of the two day fields was restricted, which decides
	// between OR and AND for them. See the note above.
	domRestricted, dowRestricted bool
}

// cronMacros expand to equivalent five-field specs.
var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

func parseCron(spec string) (*cronSchedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty cron spec")
	}
	if strings.HasPrefix(spec, "@") {
		expanded, ok := cronMacros[strings.ToLower(spec)]
		if !ok {
			return nil, fmt.Errorf("unknown cron macro %q", spec)
		}
		spec = expanded
	}

	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron spec %q has %d fields, want 5 "+
			"(minute hour day-of-month month day-of-week)", spec, len(fields))
	}

	c := &cronSchedule{}
	var err error
	if c.minute, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.hour, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.month, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.dow, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	// Both 0 and 7 mean Sunday; normalise onto 0 so matching can use
	// time.Weekday directly.
	if c.dow&(1<<7) != 0 {
		c.dow |= 1 << 0
		c.dow &^= 1 << 7
	}

	c.domRestricted = strings.TrimSpace(fields[2]) != "*"
	c.dowRestricted = strings.TrimSpace(fields[4]) != "*"
	return c, nil
}

// parseCronField turns one field into a bitmask over [min,max].
func parseCronField(field string, min, max int) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("empty element in %q", field)
		}

		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(stepStr))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q in %q", stepStr, field)
			}
			step = n
			part = strings.TrimSpace(base)
		}

		lo, hi := min, max
		switch {
		case part == "*":
			// full range
		case strings.Contains(part, "-"):
			loStr, hiStr, _ := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(strings.TrimSpace(loStr)); err != nil {
				return 0, fmt.Errorf("bad range start %q in %q", loStr, field)
			}
			if hi, err = strconv.Atoi(strings.TrimSpace(hiStr)); err != nil {
				return 0, fmt.Errorf("bad range end %q in %q", hiStr, field)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, fmt.Errorf("bad value %q in %q", part, field)
			}
			lo, hi = n, n
			// A bare value with a step means "from here to the end of the
			// range", as in the common `17/5`.
			if step > 1 {
				hi = max
			}
		}

		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("%d-%d is outside %d-%d in %q", lo, hi, min, max, field)
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	if mask == 0 {
		return 0, fmt.Errorf("%q matches nothing", field)
	}
	return mask, nil
}

// next returns the first firing strictly after t.
//
// It steps rather than scanning minute by minute — a spec like `0 0 1 1 *`
// would otherwise walk half a million minutes — and gives up after four years,
// which is longer than any satisfiable spec needs. February 30th is not
// satisfiable and must terminate rather than loop.
func (c *cronSchedule) next(t time.Time) (time.Time, error) {
	cur := t.Truncate(time.Minute).Add(time.Minute)
	limit := cur.AddDate(4, 0, 0)

	for cur.Before(limit) {
		if c.month&(1<<uint(int(cur.Month()))) == 0 {
			// First minute of the next month.
			cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, cur.Location()).
				AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatches(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location()).
				AddDate(0, 0, 1)
			continue
		}
		if c.hour&(1<<uint(cur.Hour())) == 0 {
			cur = cur.Truncate(time.Hour).Add(time.Hour)
			continue
		}
		if c.minute&(1<<uint(cur.Minute())) == 0 {
			cur = cur.Add(time.Minute)
			continue
		}
		return cur, nil
	}
	return time.Time{}, fmt.Errorf("no firing within four years; the spec is unsatisfiable")
}

func (c *cronSchedule) dayMatches(t time.Time) bool {
	domHit := c.dom&(1<<uint(t.Day())) != 0
	dowHit := c.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case c.domRestricted && c.dowRestricted:
		return domHit || dowHit
	case c.domRestricted:
		return domHit
	case c.dowRestricted:
		return dowHit
	default:
		return true
	}
}
