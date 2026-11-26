package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSpec is a parsed 5-field cron expression (minute hour day-of-month month day-of-week), local time.
type cronSpec struct {
	min, hour, dom, month, dow uint64
	domStar, dowStar           bool
}

type cronField struct {
	name    string
	lo, hi  int
	names   map[string]int
	sunday7 bool
}

var (
	cronMonths = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	cronDays   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	cronFields = []cronField{
		{name: "minute", lo: 0, hi: 59},
		{name: "hour", lo: 0, hi: 23},
		{name: "day of month", lo: 1, hi: 31},
		{name: "month", lo: 1, hi: 12, names: cronMonths},
		{name: "day of week", lo: 0, hi: 7, names: cronDays, sunday7: true},
	}
	cronMacros = map[string]string{"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
		"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *"}
)

func (f cronField) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < f.lo || n > f.hi {
		return 0, fmt.Errorf("%s: %q is not in %d-%d", f.name, s, f.lo, f.hi)
	}
	return n, nil
}

// parse reads one field: lists of *, n, a-b, with an optional /step.
func (f cronField) parse(expr string) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(expr, ",") {
		if part == "" {
			return 0, fmt.Errorf("%s: empty list item", f.name)
		}
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 || n > f.hi {
				return 0, fmt.Errorf("%s: bad step %q", f.name, stepS)
			}
			step = n
		}
		lo, hi := f.lo, f.hi
		switch {
		case rng == "*":
			if f.sunday7 {
				hi = 6
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%s: range %s runs backwards", f.name, rng)
			}
		default:
			v, err := f.value(rng)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
			if hasStep {
				hi = f.hi
			}
		}
		for v := lo; v <= hi; v += step {
			if f.sunday7 && v == 7 {
				v = 0
				bits |= 1
				break
			}
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

// parseCron accepts "m h dom mon dow" or one of the @macros.
func parseCron(expr string) (*cronSpec, error) {
	expr = strings.TrimSpace(expr)
	if m, ok := cronMacros[strings.ToLower(expr)]; ok {
		expr = m
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron needs 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var vals [5]uint64
	for i, f := range cronFields {
		b, err := f.parse(parts[i])
		if err != nil {
			return nil, err
		}
		vals[i] = b
	}
	c := &cronSpec{min: vals[0], hour: vals[1], dom: vals[2], month: vals[3], dow: vals[4],
		domStar: strings.HasPrefix(parts[2], "*"), dowStar: strings.HasPrefix(parts[4], "*")}
	if c.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return nil, fmt.Errorf("cron %q never runs: no such date", expr)
	}
	return c, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

// dayMatch follows Vixie cron: when either day field starts with "*" both must match, else either one may.
func (c *cronSpec) dayMatch(t time.Time) bool {
	d, w := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	if c.domStar || c.dowStar {
		return d && w
	}
	return d || w
}

// wallMatch reports whether a wall-clock minute matches; w carries the wall time in UTC.
func (c *cronSpec) wallMatch(w time.Time) bool {
	return has(c.month, int(w.Month())) && c.dayMatch(w) && has(c.hour, w.Hour()) && has(c.min, w.Minute())
}

func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// gapMatch reports whether a wall-clock minute skipped by a forward clock change just before t matches.
func (c *cronSpec) gapMatch(t time.Time) bool {
	prev, cur := wall(t.Add(-time.Minute)), wall(t)
	for w := prev.Add(time.Minute); w.Before(cur) && cur.Sub(w) <= 3*time.Hour; w = w.Add(time.Minute) {
		if c.wallMatch(w) {
			return true
		}
	}
	return false
}

// repeatedWall reports whether t's wall-clock minute already occurred earlier (the second pass of a fall-back hour).
func repeatedWall(t time.Time) bool {
	w := wall(t)
	for _, d := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour} {
		if wall(t.Add(-d)).Equal(w) {
			return true
		}
	}
	return false
}

// next returns the first match strictly after t, or zero within 5 years.
// A repeated DST minute runs once; a skipped one runs at the first minute after the gap.
func (c *cronSpec) next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.gapMatch(t) {
			return t
		}
		if !has(c.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.dayMatch(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !has(c.hour, t.Hour()) {
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
			continue
		}
		if !has(c.min, t.Minute()) || repeatedWall(t) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// nextN lists the next n run times after t.
func (c *cronSpec) nextN(t time.Time, n int) []time.Time {
	out := []time.Time{}
	for len(out) < n {
		t = c.next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}
