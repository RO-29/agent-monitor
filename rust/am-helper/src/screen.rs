// Port of host_screen.go: renders raw terminal output to plain text, the same subset of VT handling.

const MAX_LINES: usize = 5000;

#[derive(Clone)]
struct VScreen {
    lines: Vec<Vec<char>>,
    row: i64,
    col: i64,
    cols: i64,
    rows: i64,
    s_row: i64,
    s_col: i64,
    alt: Option<Box<VScreen>>,
}

/// screenText: the last n lines (all when n <= 0).
pub fn screen_text(b: &[u8], cols: i64, rows: i64, n: i64) -> String {
    let mut lines = render(b, cols, rows);
    if n > 0 && lines.len() as i64 > n {
        lines = lines.split_off(lines.len() - n as usize);
    }
    lines.join("\n")
}

fn render(b: &[u8], cols: i64, rows: i64) -> Vec<String> {
    let cols = if cols <= 0 { 80 } else { cols };
    let rows = if rows <= 0 { 24 } else { rows };
    let mut s = VScreen { lines: Vec::new(), row: 0, col: 0, cols, rows, s_row: 0, s_col: 0, alt: None };
    s.feed(b);
    let mut out: Vec<String> = s.lines.iter().map(|l| l.iter().collect::<String>().trim_end_matches(' ').to_string()).collect();
    while out.last().map_or(false, |l| l.is_empty()) {
        out.pop();
    }
    out
}

/// utf8.DecodeRune: (rune, size), size 1 with None on an invalid or cut sequence.
fn decode_rune(b: &[u8]) -> (Option<char>, usize) {
    let end = b.len().min(4);
    match std::str::from_utf8(&b[..end]) {
        Ok(s) => {
            let c = s.chars().next().unwrap();
            (Some(c), c.len_utf8())
        }
        Err(e) if e.valid_up_to() > 0 => {
            let s = unsafe { std::str::from_utf8_unchecked(&b[..e.valid_up_to()]) };
            let c = s.chars().next().unwrap();
            (Some(c), c.len_utf8())
        }
        Err(_) => (None, 1),
    }
}

impl VScreen {
    fn ensure(&mut self) {
        if self.row < 0 {
            self.row = 0;
        }
        while (self.lines.len() as i64) <= self.row {
            self.lines.push(Vec::new());
        }
        if self.lines.len() > MAX_LINES {
            let drop = self.lines.len() - MAX_LINES + 1000;
            self.lines.drain(..drop);
            self.row = self.row.wrapping_sub(drop as i64);
            self.s_row = self.s_row.wrapping_sub(drop as i64);
            if self.row < 0 {
                self.row = 0;
            }
        }
    }

    /// Keeps cursor moves on the visible screen, so a huge count cannot grow the buffer.
    fn clamp_row(&mut self) {
        let m = self.top() + self.rows - 1;
        if self.row > m {
            self.row = m;
        }
    }

    fn top(&self) -> i64 {
        let t = self.lines.len() as i64 - self.rows;
        if t > 0 {
            t
        } else {
            0
        }
    }

    fn put(&mut self, r: char) {
        if self.col >= self.cols {
            self.col = 0;
            self.row = self.row.wrapping_add(1);
        }
        self.ensure();
        let col = self.col as usize;
        let l = &mut self.lines[self.row as usize];
        while l.len() < col {
            l.push(' ');
        }
        if col < l.len() {
            l[col] = r;
        } else {
            l.push(r);
        }
        self.col += 1;
    }

    fn erase_line(&mut self, mode: i64) {
        self.ensure();
        let col = self.col;
        let l = &mut self.lines[self.row as usize];
        match mode {
            0 => {
                if col >= 0 && (col as usize) < l.len() {
                    l.truncate(col as usize);
                }
            }
            1 => {
                let mut i = 0usize;
                while (i as i64) <= col && i < l.len() {
                    l[i] = ' ';
                    i += 1;
                }
            }
            _ => l.clear(),
        }
    }

    fn feed(&mut self, b: &[u8]) {
        let mut i = 0;
        while i < b.len() {
            let c = b[i];
            match c {
                0x1b => {
                    i = self.escape(b, i);
                    continue;
                }
                b'\r' => self.col = 0,
                b'\n' | 0x0b | 0x0c => {
                    self.row = self.row.wrapping_add(1);
                    self.ensure();
                }
                0x08 => {
                    if self.col > 0 {
                        self.col -= 1;
                    }
                }
                b'\t' => {
                    self.col = (self.col / 8 + 1) * 8;
                    if self.col >= self.cols {
                        self.col = self.cols - 1;
                    }
                }
                c if c < 0x20 || c == 0x7f => {}
                _ => {
                    let (r, size) = decode_rune(&b[i..]);
                    match r {
                        None => {
                            i += 1;
                            continue;
                        }
                        Some(r) => {
                            self.put(r);
                            i += size;
                            continue;
                        }
                    }
                }
            }
            i += 1;
        }
    }

    fn escape(&mut self, b: &[u8], i: usize) -> usize {
        if i + 1 >= b.len() {
            return b.len();
        }
        match b[i + 1] {
            b'[' => {
                let mut j = i + 2;
                while j < b.len() && (0x20..=0x3f).contains(&b[j]) {
                    j += 1;
                }
                if j >= b.len() {
                    return b.len();
                }
                let params = String::from_utf8_lossy(&b[i + 2..j]).into_owned();
                self.csi(&params, b[j]);
                return j + 1;
            }
            b']' | b'P' | b'_' | b'^' | b'X' => {
                let mut j = i + 2;
                while j < b.len() {
                    if b[j] == 0x07 {
                        return j + 1;
                    }
                    if b[j] == 0x1b && j + 1 < b.len() && b[j + 1] == b'\\' {
                        return j + 2;
                    }
                    j += 1;
                }
                return b.len();
            }
            b'(' | b')' | b'*' | b'+' | b'#' | b'%' => return i + 3,
            b'7' => {
                self.s_row = self.row;
                self.s_col = self.col;
            }
            b'8' => {
                self.row = self.s_row;
                self.col = self.s_col;
                self.ensure();
            }
            b'M' => {
                if self.row > 0 {
                    self.row -= 1;
                }
            }
            b'D' => {
                self.row = self.row.wrapping_add(1);
                self.ensure();
            }
            b'E' => {
                self.row = self.row.wrapping_add(1);
                self.col = 0;
                self.ensure();
            }
            b'c' => {
                self.lines.clear();
                self.row = 0;
                self.col = 0;
            }
            _ => {}
        }
        i + 2
    }

    fn csi(&mut self, params: &str, fin: u8) {
        let private = params.starts_with('?');
        let params = params.trim_start_matches(['?', '<', '=', '>']);
        let nums: Vec<i64> = params.split(';').map(|p| crate::util::atoi_lenient(go_trim_space(p))).collect();
        let arg = |k: usize, def: i64| -> i64 {
            if k < nums.len() && nums[k] > 0 {
                nums[k]
            } else {
                def
            }
        };
        let mode = nums.first().copied().unwrap_or(0);
        match fin {
            b'A' => {
                self.row = self.row.wrapping_sub(arg(0, 1));
                if self.row < 0 {
                    self.row = 0;
                }
            }
            b'B' | b'e' => {
                self.row = self.row.wrapping_add(arg(0, 1).min(self.rows));
                self.clamp_row();
                self.ensure();
            }
            b'C' | b'a' => {
                self.col = self.col.wrapping_add(arg(0, 1).min(self.cols));
                if self.col >= self.cols {
                    self.col = self.cols - 1;
                }
            }
            b'D' => {
                self.col = self.col.wrapping_sub(arg(0, 1));
                if self.col < 0 {
                    self.col = 0;
                }
            }
            b'E' => {
                self.row = self.row.wrapping_add(arg(0, 1).min(self.rows));
                self.col = 0;
                self.clamp_row();
                self.ensure();
            }
            b'F' => {
                self.row = self.row.wrapping_sub(arg(0, 1));
                self.col = 0;
                if self.row < 0 {
                    self.row = 0;
                }
            }
            b'G' | b'`' => self.col = arg(0, 1).min(self.cols).wrapping_sub(1),
            b'd' => {
                self.row = self.top().wrapping_add(arg(0, 1).min(self.rows)).wrapping_sub(1);
                self.ensure();
            }
            b'H' | b'f' => {
                self.row = self.top().wrapping_add(arg(0, 1).min(self.rows)).wrapping_sub(1);
                self.col = arg(1, 1).min(self.cols).wrapping_sub(1);
                self.ensure();
            }
            b'K' => self.erase_line(mode),
            b'J' => match mode {
                0 => {
                    self.erase_line(0);
                    if (self.row + 1) < self.lines.len() as i64 {
                        self.lines.truncate((self.row + 1) as usize);
                    }
                }
                2 | 3 => {
                    let t = self.top();
                    self.lines.truncate(t as usize);
                    if mode == 3 {
                        self.lines.clear();
                        self.row -= t;
                    }
                    self.ensure();
                }
                _ => {}
            },
            b'h' | b'l' => {
                if !private {
                    return;
                }
                for &n in &nums {
                    if n != 1049 && n != 1047 && n != 47 {
                        continue;
                    }
                    if fin == b'h' && self.alt.is_none() {
                        let mut saved = self.clone();
                        saved.alt = None;
                        self.alt = Some(Box::new(saved));
                        self.lines.clear();
                        self.row = 0;
                        self.col = 0;
                    } else if fin == b'l' && self.alt.is_some() {
                        let saved = self.alt.take().unwrap();
                        *self = *saved;
                        self.alt = None;
                    }
                }
            }
            _ => {}
        }
    }
}

/// strings.TrimSpace for the ASCII-only CSI parameter bytes.
fn go_trim_space(s: &str) -> &str {
    s.trim()
}

#[cfg(test)]
mod tests {
    use super::screen_text;

    fn st(s: &str, cols: i64) -> String {
        screen_text(s.as_bytes(), cols, 24, 0)
    }

    #[test]
    fn strips_escapes() {
        let cases: &[(&str, &str, i64)] = &[
            ("\x1b]0;title\x07\x1b[1;31mred\x1b[0m text\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\r\n", "red textlink", 80),
            ("progress 10%\rprogress 99%\r\n", "progress 99%", 80),
            ("abcdef\rXY\r\n", "XYcdef", 80),
            ("old line\r\x1b[2Knew\r\n", "new", 80),
            ("ab\x08c\r\n", "ac", 80),
            ("ab\ncd", "ab\n  cd", 80),
            ("\x1b(B\x1b[?25l\x1b[?2004hok\x1b[?25h", "ok", 80),
            ("\x1bPq#0;2;0;0;0\x1b\\after", "after", 80),
            ("abcdefghij", "abcde\nfghij", 5),
            ("done\x1b[3", "done", 80),
        ];
        for (inp, want, cols) in cases {
            assert_eq!(&st(inp, *cols), want, "input {:?}", inp);
        }
    }

    #[test]
    fn huge_cursor_moves_stay_on_screen() {
        for seq in ["\x1b[9999999999B", "\x1b[9999999999d", "\x1b[9999999999;9999999999H", "\x1b[9999999999E", "\x1b[99999999999999999999B"] {
            assert_eq!(screen_text(format!("a{}b", seq).as_bytes(), 10, 4, 0).lines().count(), 4, "{:?}", seq);
        }
        assert_eq!(screen_text(b"a\x1b[9999999999999999999Cb", 10, 4, 0), "a        b");
        assert_eq!(screen_text(b"x\x1b[9999999999Gy", 10, 4, 0), "x        y");
    }

    #[test]
    fn ink_redraw() {
        let frame1 = "Do you want to proceed?\r\n❯ 1. Yes\r\n  2. No\r\n";
        let redraw = "\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[G✓ Done\r\n";
        assert_eq!(st(&format!("$ start\r\n{}{}", frame1, redraw), 80), "$ start\n✓ Done");
    }

    #[test]
    fn clear_and_alt_screen() {
        assert_eq!(st("old\r\nstuff\r\n\x1b[2J\x1b[3J\x1b[Hfresh", 80), "fresh");
        assert_eq!(st("shell\r\n\x1b[?1049hfull screen app\x1b[?1049lback", 80), "shell\nback");
        assert_eq!(screen_text(b"a\r\nb\r\nc\r\nd\r\n", 80, 24, 2), "c\nd");
        assert_eq!(st("row1\r\nrow2\r\n\x1b[1;1HXX", 80), "XXw1\nrow2");
    }
}
