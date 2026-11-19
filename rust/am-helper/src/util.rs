// Ports of the small Go helpers the three modes share (env, paths, quoting, URL escaping, time).

use std::env;
use std::os::unix::fs::MetadataExt;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

pub fn home_dir() -> String {
    env::var("HOME").unwrap_or_default()
}

pub fn state_dir() -> String {
    match env::var("AGENT_MONITOR_HOME") {
        Ok(v) if !v.is_empty() => v,
        _ => home_dir() + "/.agent-monitor",
    }
}

pub fn getenv(k: &str) -> String {
    env::var(k).unwrap_or_default()
}

/// ensureToolEnv: a UTF-8 locale for tmux, and Homebrew dirs on PATH.
pub fn ensure_tool_env() {
    if getenv("LC_ALL").is_empty() && getenv("LC_CTYPE").is_empty() && getenv("LANG").is_empty() {
        env::set_var("LC_CTYPE", "UTF-8");
    }
    let mut path = getenv("PATH");
    let have: Vec<String> = path.split(':').map(|s| s.to_string()).collect();
    for d in ["/opt/homebrew/bin", "/usr/local/bin"] {
        if !have.iter().any(|h| h == d) && Path::new(d).exists() {
            path.push(':');
            path.push_str(d);
        }
    }
    env::set_var("PATH", path);
}

/// os.Getwd: $PWD when it names the current directory, else getcwd.
pub fn go_getwd() -> String {
    let pwd = getenv("PWD");
    if pwd.starts_with('/') {
        if let (Ok(a), Ok(b)) = (std::fs::metadata(&pwd), std::fs::metadata(".")) {
            if a.dev() == b.dev() && a.ino() == b.ino() {
                return pwd;
            }
        }
    }
    env::current_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default()
}

pub fn base_name(p: &str) -> String {
    let t = p.trim_end_matches('/');
    if t.is_empty() {
        return if p.is_empty() { ".".into() } else { "/".into() };
    }
    t.rsplit('/').next().unwrap_or(t).to_string()
}

fn is_exec_file(p: &str) -> bool {
    match std::fs::metadata(p) {
        Ok(m) => !m.is_dir() && m.permissions().mode() & 0o111 != 0,
        Err(_) => false,
    }
}

/// exec.LookPath, with Go's error text.
pub fn look_path(name: &str) -> Result<String, String> {
    if name.contains('/') {
        if is_exec_file(name) {
            return Ok(name.to_string());
        }
        let why = match std::fs::metadata(name) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => format!("stat {}: no such file or directory", name),
            Err(_) => format!("stat {}: permission denied", name),
            Ok(m) if m.is_dir() => "is a directory".to_string(),
            Ok(_) => "permission denied".to_string(),
        };
        return Err(format!("exec: {}: {}", go_quote(name), why));
    }
    for dir in getenv("PATH").split(':') {
        let d = if dir.is_empty() { "." } else { dir };
        let p = format!("{}/{}", d, name);
        if is_exec_file(&p) {
            if !d.starts_with('/') {
                return Err(format!("exec: {}: cannot run executable found relative to current directory", go_quote(name)));
            }
            return Ok(p);
        }
    }
    Err(format!("exec: {}: executable file not found in $PATH", go_quote(name)))
}

/// clipString: at most n bytes, never splitting a UTF-8 sequence.
pub fn clip_string(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_string();
    }
    let mut n = n;
    while n > 0 && !s.is_char_boundary(n) {
        n -= 1;
    }
    s[..n].to_string()
}

fn is_print(c: char) -> bool {
    let u = c as u32;
    if u < 0x20 || (0x7f..0xa0).contains(&u) {
        return false;
    }
    if u == 0x20 {
        return true;
    }
    !matches!(u,
        0xa0 | 0xad | 0x600..=0x605 | 0x61c | 0x6dd | 0x70f | 0x890..=0x891 | 0x8e2 | 0x1680 | 0x180e
        | 0x2000..=0x200f | 0x2028..=0x202f | 0x205f..=0x2064 | 0x2066..=0x206f | 0x3000
        | 0xd800..=0xf8ff | 0xfeff | 0xfff9..=0xfffb | 0xfffe..=0xffff | 0x110bd | 0x110cd
        | 0x13430..=0x1343f | 0x1bca0..=0x1bca3 | 0x1d173..=0x1d17a | 0xe0001 | 0xe0020..=0xe007f
        | 0xf0000..=0x10ffff)
}

/// strconv.Quote (Go's %q for strings).
pub fn go_quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            c if is_print(c) => out.push(c),
            c if (c as u32) < 0x80 => out.push_str(&format!("\\x{:02x}", c as u32)),
            c if (c as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out.push('"');
    out
}

fn unreserved(c: u8) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, b'-' | b'_' | b'.' | b'~')
}

/// url.QueryEscape.
pub fn query_escape(s: &str) -> String {
    let mut out = String::new();
    for &c in s.as_bytes() {
        if unreserved(c) {
            out.push(c as char);
        } else if c == b' ' {
            out.push('+');
        } else {
            out.push_str(&format!("%{:02X}", c));
        }
    }
    out
}

/// url.PathEscape.
pub fn path_escape(s: &str) -> String {
    let mut out = String::new();
    for &c in s.as_bytes() {
        if unreserved(c) || matches!(c, b'$' | b'&' | b'+' | b':' | b'=' | b'@') {
            out.push(c as char);
        } else {
            out.push_str(&format!("%{:02X}", c));
        }
    }
    out
}

/// url.Values.Encode: keys sorted.
pub fn encode_query(pairs: &[(&str, String)]) -> String {
    let mut v: Vec<&(&str, String)> = pairs.iter().collect();
    v.sort_by(|a, b| a.0.cmp(b.0));
    v.iter().map(|(k, val)| format!("{}={}", query_escape(k), query_escape(val))).collect::<Vec<_>>().join("&")
}

extern "C" {
    fn tzset();
}

/// time.UnixMilli(ms).Format("2006-01-02 15:04") in local time; "" for ms <= 0.
pub fn fmt_ms(ms: i64) -> String {
    if ms <= 0 {
        return String::new();
    }
    let secs = ms.div_euclid(1000) as libc::time_t;
    let mut tm: libc::tm = unsafe { std::mem::zeroed() };
    unsafe {
        tzset();
        libc::localtime_r(&secs, &mut tm);
    }
    format!("{:04}-{:02}-{:02} {:02}:{:02}", tm.tm_year + 1900, tm.tm_mon + 1, tm.tm_mday, tm.tm_hour, tm.tm_min)
}

/// Go's strconv.Atoi as used with the error ignored: 0 on syntax errors, clamped on overflow.
pub fn atoi_lenient(s: &str) -> i64 {
    let b = s.as_bytes();
    if b.is_empty() {
        return 0;
    }
    let (neg, digits) = match b[0] {
        b'-' => (true, &b[1..]),
        b'+' => (false, &b[1..]),
        _ => (false, b),
    };
    if digits.is_empty() || !digits.iter().all(|c| c.is_ascii_digit()) {
        return 0;
    }
    let mut v: i128 = 0;
    for &c in digits {
        v = v * 10 + (c - b'0') as i128;
        if v > i64::MAX as i128 + 1 {
            break;
        }
    }
    let v = if neg { -v } else { v };
    v.clamp(i64::MIN as i128, i64::MAX as i128) as i64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quote_like_go() {
        assert_eq!(go_quote("a\"b\n\u{1}é\u{a0}"), "\"a\\\"b\\n\\x01é\\u00a0\"");
    }

    #[test]
    fn escapes_like_go() {
        assert_eq!(query_escape("a b/c,d"), "a+b%2Fc%2Cd");
        assert_eq!(path_escape("claude:x/y z"), "claude:x%2Fy%20z");
        assert_eq!(encode_query(&[("q", "x y".into()), ("limit", "200".into())]), "limit=200&q=x+y");
    }

    #[test]
    fn atoi_matches_go() {
        assert_eq!(atoi_lenient("12"), 12);
        assert_eq!(atoi_lenient("+3"), 3);
        assert_eq!(atoi_lenient("x"), 0);
        assert_eq!(atoi_lenient("99999999999999999999"), i64::MAX);
        assert_eq!(atoi_lenient("-99999999999999999999"), i64::MIN);
    }

    #[test]
    fn clip_keeps_utf8() {
        assert_eq!(clip_string("aé", 2), "a");
        assert_eq!(base_name("/usr/bin/claude"), "claude");
    }
}

/// Go's syscall.Errno text: strerror with a lowercase first letter.
pub fn errno_text(code: i32) -> String {
    let s = unsafe { std::ffi::CStr::from_ptr(libc::strerror(code)) }.to_string_lossy().into_owned();
    let mut c = s.chars();
    match c.next() {
        Some(f) => f.to_lowercase().collect::<String>() + c.as_str(),
        None => s,
    }
}

pub fn io_err_text(e: &std::io::Error) -> String {
    match e.raw_os_error() {
        Some(c) => errno_text(c),
        None if e.kind() == std::io::ErrorKind::InvalidInput => "invalid argument".into(),
        None => e.to_string(),
    }
}

pub fn last_errno() -> i32 {
    std::io::Error::last_os_error().raw_os_error().unwrap_or(0)
}

/// strconv.Atoi with its error checked: None on syntax or range errors.
pub fn atoi_strict(s: &str) -> Option<i64> {
    let b = s.as_bytes();
    let digits = match b.first() {
        Some(b'-') | Some(b'+') => &b[1..],
        _ => b,
    };
    if digits.is_empty() || !digits.iter().all(|c| c.is_ascii_digit()) {
        return None;
    }
    s.parse::<i64>().ok()
}

/// strings.EqualFold, close enough for aliases.
pub fn eq_fold(a: &str, b: &str) -> bool {
    a == b || a.to_lowercase() == b.to_lowercase()
}

pub fn first_of<'a>(vals: &[&'a str]) -> &'a str {
    vals.iter().find(|v| !v.is_empty()).copied().unwrap_or("")
}

/// html.UnescapeString for what html.EscapeString produces, plus numeric and common named entities.
pub fn html_unescape(s: &str) -> String {
    if !s.contains('&') {
        return s.to_string();
    }
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find('&') {
        out.push_str(&rest[..i]);
        rest = &rest[i..];
        let end = rest[1..].find(|c: char| !(c.is_ascii_alphanumeric() || c == '#')).map(|e| e + 1).unwrap_or(rest.len());
        let name = &rest[1..end];
        let semi = rest[end..].starts_with(';');
        let decoded = if let Some(num) = name.strip_prefix('#') {
            let v = if let Some(h) = num.strip_prefix('x').or_else(|| num.strip_prefix('X')) { u32::from_str_radix(h, 16).ok() } else { num.parse::<u32>().ok() };
            v.map(|v| char::from_u32(v).filter(|_| v != 0).unwrap_or('\u{fffd}'))
        } else {
            match name {
                "amp" => Some('&'),
                "lt" => Some('<'),
                "gt" => Some('>'),
                "quot" => Some('"'),
                "apos" => Some('\''),
                "nbsp" => Some('\u{a0}'),
                _ => None,
            }
        };
        match decoded {
            Some(c) if !name.is_empty() => {
                out.push(c);
                rest = &rest[end + if semi { 1 } else { 0 }..];
            }
            _ => {
                out.push('&');
                rest = &rest[1..];
            }
        }
    }
    out.push_str(rest);
    out
}
