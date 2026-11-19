package main

import (
	"strings"
	"testing"
	"time"
)

func bitsOf(b uint64, lo, hi int) []int {
	var out []int
	for v := lo; v <= hi; v++ {
		if has(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCronParseFields(t *testing.T) {
	c, err := parseCron("0,15-17,*/20 9-17/4 1,15 jan-mar,DEC mon-fri")
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  []int
		want []int
	}{
		{"minute", bitsOf(c.min, 0, 59), []int{0, 15, 16, 17, 20, 40}},
		{"hour", bitsOf(c.hour, 0, 23), []int{9, 13, 17}},
		{"dom", bitsOf(c.dom, 1, 31), []int{1, 15}},
		{"month", bitsOf(c.month, 1, 12), []int{1, 2, 3, 12}},
		{"dow", bitsOf(c.dow, 0, 6), []int{1, 2, 3, 4, 5}},
	}
	for _, ch := range checks {
		if !eqInts(ch.got, ch.want) {
			t.Errorf("%s: %v want %v", ch.name, ch.got, ch.want)
		}
	}
	if c.domStar || c.dowStar {
		t.Error("restricted day fields marked star")
	}
	for expr, want := range map[string][]int{"* * * * 7": {0}, "* * * * 5-7": {0, 5, 6}, "* * * * */2": {0, 2, 4, 6},
		"* * * * sun,SAT": {0, 6}, "* * * * 1/2": {0, 1, 3, 5}, "* * * * *": {0, 1, 2, 3, 4, 5, 6}} {
		c, err := parseCron(expr)
		if err != nil || !eqInts(bitsOf(c.dow, 0, 6), want) {
			t.Errorf("%s: %v %v want %v", expr, err, bitsOf(c.dow, 0, 6), want)
		}
	}
	if c, _ := parseCron("10/25 * * * *"); !eqInts(bitsOf(c.min, 0, 59), []int{10, 35}) {
		t.Errorf("a/n minutes %v", bitsOf(c.min, 0, 59))
	}
	if c, _ := parseCron("@daily"); !eqInts(bitsOf(c.min, 0, 59), []int{0}) || !eqInts(bitsOf(c.hour, 0, 23), []int{0}) {
		t.Error("@daily")
	}
	for _, bad := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"5-1 * * * *", "*/0 * * * *", "* * * foo *", "* * * * funday", "1,,2 * * * *", "a * * * *", "-1 * * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := parseCron("61 * * * *"); err == nil || !strings.Contains(err.Error(), "minute") {
		t.Errorf("error names the field: %v", err)
	}
}

func TestCronNext(t *testing.T) {
	loc := time.FixedZone("T", 3*3600)
	at := func(s string) time.Time {
		tt, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return tt
	}
	cases := []struct{ expr, from, want string }{
		{"0 9 * * mon-fri", "2026-11-13 09:00", "2026-11-16 09:00"}, // Friday 09:00 exactly -> Monday
		{"0 9 * * mon-fri", "2026-11-16 08:59", "2026-11-16 09:00"},
		{"*/15 * * * *", "2026-11-16 10:07", "2026-11-16 10:15"},
		{"30 23 31 12 *", "2026-12-31 23:30", "2027-12-31 23:30"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 1 * sun", "2026-11-16 00:00", "2026-11-22 12:00"}, // dom OR dow: next Sunday before Dec 1
		{"0 12 1 * sun", "2026-11-29 12:00", "2026-12-01 12:00"},
		{"0 12 */10 * *", "2026-11-16 00:00", "2026-11-21 12:00"},
		{"5 4 * jun *", "2026-11-16 00:00", "2027-06-01 04:05"},
	}
	for _, c := range cases {
		spec, err := parseCron(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s from %s: %s want %s", c.expr, c.from, got.Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
	if _, err := parseCron("0 0 30 2 *"); err == nil {
		t.Error("Feb 30 accepted")
	}
	spec, _ := parseCron("0 9 * * 1")
	if n := spec.nextN(at("2026-11-16 10:00"), 3); len(n) != 3 || !n[2].Equal(at("2026-12-07 09:00")) {
		t.Errorf("nextN %v", n)
	}
}

func TestCronNextAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz data")
	}
	spec, _ := parseCron("30 * * * *")
	// 2026-11-01 01:30 happens twice; the second 01:30 must not loop.
	t0 := time.Date(2026, 11, 1, 1, 30, 0, 0, ny).Add(time.Hour)
	got := spec.next(t0)
	if !got.After(t0) || got.Sub(t0) != time.Hour {
		t.Fatalf("after second 01:30: %v (%v later)", got, got.Sub(t0))
	}
	spec, _ = parseCron("15 2 * * *")
	got = spec.next(time.Date(2026, 3, 8, 0, 0, 0, 0, ny))
	if got.Day() != 8 || got.Hour() != 3 || got.Minute() != 0 {
		t.Fatalf("skipped 02:15 on spring-forward day must run at 03:00: %v", got)
	}
}
