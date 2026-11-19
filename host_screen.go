package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// screenText renders raw terminal output to plain text and returns its last n lines.
// It follows CR, LF, BS, tabs, cursor moves, line and screen erases and the alternate screen on a
// growing line buffer. It is not a full VT emulator: no scroll regions, no insert/delete, wide runes count one column.
func screenText(b []byte, cols, rows, n int) string {
	lines := renderScreen(b, cols, rows)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type vscreen struct {
	lines      [][]rune
	row, col   int
	cols, rows int
	sRow, sCol int
	alt        *vscreen
}

const vscreenMaxLines = 5000

func renderScreen(b []byte, cols, rows int) []string {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	s := &vscreen{cols: cols, rows: rows}
	s.feed(b)
	out := make([]string, 0, len(s.lines))
	for _, l := range s.lines {
		out = append(out, strings.TrimRight(string(l), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (s *vscreen) ensure() {
	if s.row < 0 {
		s.row = 0
	}
	for len(s.lines) <= s.row {
		s.lines = append(s.lines, nil)
	}
	if len(s.lines) > vscreenMaxLines {
		drop := len(s.lines) - vscreenMaxLines + 1000
		s.lines = append([][]rune(nil), s.lines[drop:]...)
		s.row -= drop
		s.sRow -= drop
		if s.row < 0 {
			s.row = 0
		}
	}
}

// clampRow keeps cursor moves on the visible screen, so a huge count cannot grow the buffer.
func (s *vscreen) clampRow() {
	if m := s.top() + s.rows - 1; s.row > m {
		s.row = m
	}
}

func (s *vscreen) top() int {
	if t := len(s.lines) - s.rows; t > 0 {
		return t
	}
	return 0
}

func (s *vscreen) put(r rune) {
	if s.col >= s.cols {
		s.col = 0
		s.row++
	}
	s.ensure()
	l := s.lines[s.row]
	for len(l) < s.col {
		l = append(l, ' ')
	}
	if s.col < len(l) {
		l[s.col] = r
	} else {
		l = append(l, r)
	}
	s.lines[s.row] = l
	s.col++
}

func (s *vscreen) eraseLine(mode int) {
	s.ensure()
	l := s.lines[s.row]
	switch mode {
	case 0:
		if s.col < len(l) {
			s.lines[s.row] = l[:s.col]
		}
	case 1:
		for i := 0; i <= s.col && i < len(l); i++ {
			l[i] = ' '
		}
	default:
		s.lines[s.row] = nil
	}
}

func (s *vscreen) feed(b []byte) {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			i = s.escape(b, i)
			continue
		case c == '\r':
			s.col = 0
		case c == '\n', c == 0x0b, c == 0x0c:
			s.row++
			s.ensure()
		case c == '\b':
			if s.col > 0 {
				s.col--
			}
		case c == '\t':
			s.col = (s.col/8 + 1) * 8
			if s.col >= s.cols {
				s.col = s.cols - 1
			}
		case c < 0x20 || c == 0x7f:
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			s.put(r)
			i += size
			continue
		}
		i++
	}
}

// escape handles the sequence at b[i] (ESC) and returns the index after it.
func (s *vscreen) escape(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	switch b[i+1] {
	case '[':
		j := i + 2
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
			j++
		}
		if j >= len(b) {
			return len(b)
		}
		s.csi(string(b[i+2:j]), b[j])
		return j + 1
	case ']', 'P', '_', '^', 'X':
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j + 1
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
		return len(b)
	case '(', ')', '*', '+', '#', '%':
		return i + 3
	case '7':
		s.sRow, s.sCol = s.row, s.col
	case '8':
		s.row, s.col = s.sRow, s.sCol
		s.ensure()
	case 'M':
		if s.row > 0 {
			s.row--
		}
	case 'D':
		s.row++
		s.ensure()
	case 'E':
		s.row++
		s.col = 0
		s.ensure()
	case 'c':
		s.lines, s.row, s.col = nil, 0, 0
	}
	return i + 2
}

func (s *vscreen) csi(params string, final byte) {
	private := strings.HasPrefix(params, "?")
	params = strings.TrimLeft(params, "?<=>")
	var nums []int
	for _, p := range strings.Split(params, ";") {
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		nums = append(nums, n)
	}
	arg := func(k, def int) int {
		if k < len(nums) && nums[k] > 0 {
			return nums[k]
		}
		return def
	}
	mode := 0
	if len(nums) > 0 {
		mode = nums[0]
	}
	switch final {
	case 'A':
		s.row -= arg(0, 1)
		if s.row < 0 {
			s.row = 0
		}
	case 'B', 'e':
		s.row += min(arg(0, 1), s.rows)
		s.clampRow()
		s.ensure()
	case 'C', 'a':
		s.col += min(arg(0, 1), s.cols)
		if s.col >= s.cols {
			s.col = s.cols - 1
		}
	case 'D':
		s.col -= arg(0, 1)
		if s.col < 0 {
			s.col = 0
		}
	case 'E':
		s.row += min(arg(0, 1), s.rows)
		s.col = 0
		s.clampRow()
		s.ensure()
	case 'F':
		s.row -= arg(0, 1)
		s.col = 0
		if s.row < 0 {
			s.row = 0
		}
	case 'G', '`':
		s.col = min(arg(0, 1), s.cols) - 1
	case 'd':
		s.row = s.top() + min(arg(0, 1), s.rows) - 1
		s.ensure()
	case 'H', 'f':
		s.row = s.top() + min(arg(0, 1), s.rows) - 1
		s.col = min(arg(1, 1), s.cols) - 1
		s.ensure()
	case 'K':
		s.eraseLine(mode)
	case 'J':
		switch mode {
		case 0:
			s.eraseLine(0)
			if s.row+1 < len(s.lines) {
				s.lines = s.lines[:s.row+1]
			}
		case 2, 3:
			t := s.top()
			s.lines = s.lines[:t]
			if mode == 3 {
				s.lines = nil
				s.row -= t
			}
			s.ensure()
		}
	case 'h', 'l':
		if !private {
			return
		}
		for _, n := range nums {
			if n != 1049 && n != 1047 && n != 47 {
				continue
			}
			if final == 'h' && s.alt == nil {
				saved := *s
				s.alt = &saved
				s.lines, s.row, s.col = nil, 0, 0
			} else if final == 'l' && s.alt != nil {
				saved := s.alt
				*s = *saved
				s.alt = nil
			}
		}
	}
}
