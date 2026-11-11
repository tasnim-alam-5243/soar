package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression (minute hour day-of-month month
// day-of-week), evaluated in UTC. It also accepts @hourly, @daily, @weekly,
// @monthly and @every <duration>.
type Cron struct {
	min, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar           bool
	every                      time.Duration
}

var cronNames = map[string]string{
	"jan": "1", "feb": "2", "mar": "3", "apr": "4", "may": "5", "jun": "6",
	"jul": "7", "aug": "8", "sep": "9", "oct": "10", "nov": "11", "dec": "12",
	"sun": "0", "mon": "1", "tue": "2", "wed": "3", "thu": "4", "fri": "5", "sat": "6",
}

// ParseCron parses a cron expression.
func ParseCron(s string) (*Cron, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "@hourly":
		s = "0 * * * *"
	case "@daily", "@midnight":
		s = "0 0 * * *"
	case "@weekly":
		s = "0 0 * * 0"
	case "@monthly":
		s = "0 0 1 * *"
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		d, err := ParseDuration(rest)
		if err != nil || d < time.Minute {
			return nil, fmt.Errorf("@every needs a duration of at least 1m")
		}
		return &Cron{every: d}, nil
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron needs 5 fields (minute hour day month weekday), got %d", len(f))
	}
	c := &Cron{domStar: f[2] == "*", dowStar: f[4] == "*"}
	var err error
	specs := []struct {
		dst       *uint64
		lo, hi    int
		name      string
		wrapSeven bool
	}{
		{&c.min, 0, 59, "minute", false},
		{&c.hour, 0, 23, "hour", false},
		{&c.dom, 1, 31, "day", false},
		{&c.month, 1, 12, "month", false},
		{&c.dow, 0, 7, "weekday", true},
	}
	for i, sp := range specs {
		if *sp.dst, err = cronField(f[i], sp.lo, sp.hi); err != nil {
			return nil, fmt.Errorf("%s field: %w", sp.name, err)
		}
		if sp.wrapSeven && *sp.dst&(1<<7) != 0 {
			*sp.dst |= 1 // 7 is also Sunday
		}
	}
	return c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		step := 1
		if base, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q", st)
			}
			step, part = n, base
		}
		from, to := lo, hi
		if part != "*" {
			a, b, isRange := strings.Cut(part, "-")
			var err error
			if from, err = cronNum(a); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = cronNum(b); err != nil {
					return 0, err
				}
			} else if step > 1 {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func cronNum(s string) (int, error) {
	if n, ok := cronNames[s]; ok {
		s = n
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

// Next returns the first activation strictly after t (UTC, minute resolution).
func (c *Cron) Next(t time.Time) time.Time {
	t = t.UTC()
	if c.every > 0 {
		return t.Truncate(c.every).Add(c.every)
	}
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(5 * 366 * 24 * time.Hour)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = t.Truncate(time.Hour).Add(time.Hour)
			continue
		}
		if c.min&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches applies the classic rule: when both day fields are restricted, either may match.
func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}
