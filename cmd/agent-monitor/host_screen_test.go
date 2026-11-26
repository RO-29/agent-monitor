package main

import (
	"strings"
	"testing"
)

func TestScreenTextStripsEscapes(t *testing.T) {
	cases := []struct {
		name, in, want string
		cols           int
	}{
		{"sgr and osc", "\x1b]0;title\x07\x1b[1;31mred\x1b[0m text\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\r\n", "red textlink", 0},
		{"carriage return overwrites", "progress 10%\rprogress 99%\r\n", "progress 99%", 0},
		{"short overwrite keeps tail", "abcdef\rXY\r\n", "XYcdef", 0},
		{"erase line", "old line\r\x1b[2Knew\r\n", "new", 0},
		{"backspace", "ab\bc\r\n", "ac", 0},
		{"bare lf keeps column", "ab\ncd", "ab\n  cd", 0},
		{"charset and private modes", "\x1b(B\x1b[?25l\x1b[?2004hok\x1b[?25h", "ok", 0},
		{"dcs dropped", "\x1bPq#0;2;0;0;0\x1b\\after", "after", 0},
		{"wrap at cols", "abcdefghij", "abcde\nfghij", 5},
		{"cut sequence at end", "done\x1b[3", "done", 0},
	}
	for _, c := range cases {
		if c.cols == 0 {
			c.cols = 80
		}
		if got := screenText([]byte(c.in), c.cols, 24, 0); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Ink redraws a frame by moving up and erasing each line; the old frame must not survive.
func TestScreenTextInkRedraw(t *testing.T) {
	frame1 := "Do you want to proceed?\r\n❯ 1. Yes\r\n  2. No\r\n"
	redraw := "\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[G" + "✓ Done\r\n"
	got := screenText([]byte("$ start\r\n"+frame1+redraw), 80, 24, 0)
	if got != "$ start\n✓ Done" {
		t.Fatalf("got %q", got)
	}
}

func TestScreenTextClearAndAltScreen(t *testing.T) {
	got := screenText([]byte("old\r\nstuff\r\n\x1b[2J\x1b[3J\x1b[Hfresh"), 80, 24, 0)
	if got != "fresh" {
		t.Fatalf("clear: got %q", got)
	}
	got = screenText([]byte("shell\r\n\x1b[?1049hfull screen app\x1b[?1049lback"), 80, 24, 0)
	if got != "shell\nback" {
		t.Fatalf("alt screen: got %q", got)
	}
	got = screenText([]byte("a\r\nb\r\nc\r\nd\r\n"), 80, 24, 2)
	if got != "c\nd" {
		t.Fatalf("last lines: got %q", got)
	}
	got = screenText([]byte("row1\r\nrow2\r\n\x1b[1;1HXX"), 80, 24, 0)
	if got != "XXw1\nrow2" {
		t.Fatalf("cup: got %q", got)
	}
}

func TestScreenTextHugeCursorMovesStayOnScreen(t *testing.T) {
	for _, seq := range []string{"\x1b[9999999999B", "\x1b[9999999999d", "\x1b[9999999999;9999999999H", "\x1b[9999999999E", "\x1b[99999999999999999999B"} {
		if n := len(strings.Split(screenText([]byte("a"+seq+"b"), 10, 4, 0), "\n")); n != 4 {
			t.Errorf("%q: %d lines, want 4", seq, n)
		}
	}
	if got := screenText([]byte("a\x1b[9999999999999999999Cb"), 10, 4, 0); got != "a        b" {
		t.Errorf("huge CUF: %q", got)
	}
	if got := screenText([]byte("x\x1b[9999999999Gy"), 10, 4, 0); got != "x        y" {
		t.Errorf("huge CHA: %q", got)
	}
}
