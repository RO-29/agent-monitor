// Ports of bin/claude-hook.sh and bin/codex-hook.sh: same requests, same stdout, always exit 0.

use crate::pyjson::{self, Py};
use std::ffi::OsStr;
use std::io::{Read, Write};
use std::os::unix::ffi::{OsStrExt, OsStringExt};
use std::process::{Command, Stdio};
use std::time::Duration;

use std::path::{Path, PathBuf};

// codex-hook.sh looks in $HOME/bin first, then beside this helper's bin dir.
fn pick_pane_script(home: Option<&Path>, exe_dir: Option<&Path>, exists: impl Fn(&Path) -> bool) -> PathBuf {
    let home_copy = home.map(|h| h.join("bin").join("tmux-pane-id.sh"));
    if let Some(p) = &home_copy {
        if exists(p) {
            return p.clone();
        }
    }
    if let Some(d) = exe_dir {
        let p = d.join("bin").join("tmux-pane-id.sh");
        if exists(&p) {
            return p;
        }
    }
    home_copy.unwrap_or_else(|| PathBuf::from("tmux-pane-id.sh"))
}

fn pane_script() -> PathBuf {
    let home = std::env::var_os("HOME").map(PathBuf::from);
    let exe_dir = std::env::current_exe().ok().and_then(|e| e.parent().map(|p| p.to_path_buf()));
    pick_pane_script(home.as_deref(), exe_dir.as_deref(), |p| p.is_file())
}

pub fn run(args: &[String]) -> i32 {
    match args.first().map(|s| s.as_str()) {
        Some("claude") => claude(),
        Some("codex") => codex(),
        _ => {
            eprintln!("usage: am-helper hook claude|codex");
            2
        }
    }
}

fn env_bytes(k: &str) -> Option<Vec<u8>> {
    std::env::var_os(k).map(|v| v.into_vec())
}

/// ${K:-} as bytes.
fn env_or_empty(k: &str) -> Vec<u8> {
    env_bytes(k).unwrap_or_default()
}

/// PAYLOAD="$(cat)": NUL bytes dropped, trailing newlines removed.
fn read_payload() -> Vec<u8> {
    let mut b = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut b);
    shell_capture(b)
}

fn shell_capture(mut b: Vec<u8>) -> Vec<u8> {
    b.retain(|&c| c != 0);
    while b.last() == Some(&b'\n') {
        b.pop();
    }
    b
}

fn monitor_url() -> Vec<u8> {
    match env_bytes("AGENT_MONITOR_URL") {
        Some(u) if !u.is_empty() => u,
        _ => {
            let port = env_or_empty("AGENT_MONITOR_PORT");
            let mut u = b"http://127.0.0.1:".to_vec();
            u.extend_from_slice(if port.is_empty() { b"7777" } else { &port });
            u
        }
    }
}

/// The pid that ran the hook: bash's $PPID survives the shim's exec.
fn ppid() -> i32 {
    unsafe { libc::getppid() }
}

/// ?host=<AM_HOST_ID>&ppid=<ppid> when AM_HOST_ID is non-empty and only [A-Za-z0-9_-].
fn host_query() -> Vec<u8> {
    let id = env_or_empty("AM_HOST_ID");
    if id.is_empty() || !id.iter().all(|c| c.is_ascii_alphanumeric() || *c == b'_' || *c == b'-') {
        return Vec::new();
    }
    let mut q = b"?host=".to_vec();
    q.extend_from_slice(&id);
    q.extend_from_slice(format!("&ppid={}", ppid()).as_bytes());
    q
}

fn cat(parts: &[&[u8]]) -> Vec<u8> {
    parts.concat()
}

/// The request line curl sends for a URL argument; None where curl refuses it or would glob it.
pub fn curl_url(u: &[u8]) -> Option<String> {
    if u.iter().any(|&c| c <= 0x20 || c == 0x7f || matches!(c, b'{' | b'}' | b'[' | b']')) {
        return None;
    }
    let u = match u.iter().position(|&c| c == b'#') {
        Some(i) => &u[..i],
        None => u,
    };
    let rest = match u.windows(3).position(|w| w == b"://") {
        Some(i) if u[..i].eq_ignore_ascii_case(b"http") => &u[i + 3..],
        Some(_) => return None,
        None => u,
    };
    let a = rest.iter().position(|&c| c == b'/' || c == b'?').unwrap_or(rest.len());
    let (auth, tail) = rest.split_at(a);
    if auth.is_empty() || !auth.is_ascii() || auth.contains(&b'@') {
        return None;
    }
    let q = tail.iter().position(|&c| c == b'?').unwrap_or(tail.len());
    let (path, query) = tail.split_at(q);
    let path = if path.is_empty() { b"/".to_vec() } else { remove_dots(path) };
    let mut out = format!("http://{}", String::from_utf8_lossy(auth));
    for &c in path.iter().chain(query.iter()) {
        if c >= 0x80 {
            out.push_str(&format!("%{:02x}", c));
        } else {
            out.push(c as char);
        }
    }
    Some(out)
}

/// RFC 3986 remove_dot_segments.
fn remove_dots(p: &[u8]) -> Vec<u8> {
    let mut input = p.to_vec();
    let mut out: Vec<u8> = Vec::new();
    while !input.is_empty() {
        if input.starts_with(b"../") {
            input.drain(..3);
        } else if input.starts_with(b"./") {
            input.drain(..2);
        } else if input.starts_with(b"/./") {
            input.drain(..2);
        } else if input == b"/." {
            input = b"/".to_vec();
        } else if input.starts_with(b"/../") || input == b"/.." {
            if input == b"/.." {
                input = b"/".to_vec();
            } else {
                input.drain(..3);
            }
            let cut = out.iter().rposition(|&c| c == b'/').unwrap_or(0);
            out.truncate(cut);
        } else if input == b"." || input == b".." {
            input.clear();
        } else {
            let start = if input[0] == b'/' { 1 } else { 0 };
            let end = input[start..].iter().position(|&c| c == b'/').map_or(input.len(), |i| i + start);
            out.extend(input.drain(..end));
        }
    }
    out
}

fn send(method: &str, url: &[u8], body: Option<&[u8]>, connect: Duration, total: Duration) -> Option<crate::http::Response> {
    let u = curl_url(url)?;
    crate::http::request_ct(method, &u, body, Some(connect), Some(total)).ok()
}

/// grep -o '"<name>"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//'.
pub fn grep_field(payload: &[u8], name: &str) -> Option<Vec<u8>> {
    let key = format!("\"{}\"", name);
    let key = key.as_bytes();
    let sp = |c: u8| matches!(c, b' ' | b'\t' | b'\n' | 0x0b | 0x0c | b'\r');
    for line in payload.split(|&c| c == b'\n') {
        for p in 0..line.len() {
            if !line[p..].starts_with(key) {
                continue;
            }
            let mut i = p + key.len();
            while i < line.len() && sp(line[i]) {
                i += 1;
            }
            if line.get(i) != Some(&b':') {
                continue;
            }
            i += 1;
            while i < line.len() && sp(line[i]) {
                i += 1;
            }
            if line.get(i) != Some(&b'"') {
                continue;
            }
            let close = match line[i + 1..].iter().position(|&c| c == b'"') {
                Some(j) => i + 1 + j,
                None => continue,
            };
            return Some(sed_value(&line[p..=close]));
        }
    }
    None
}

fn sed_value(m: &[u8]) -> Vec<u8> {
    let sp = |c: u8| matches!(c, b' ' | b'\t' | 0x0b | 0x0c | b'\r');
    let mut cut = None;
    for j in (0..m.len()).rev() {
        if m[j] != b'"' {
            continue;
        }
        let mut k = j;
        while k > 0 && sp(m[k - 1]) {
            k -= 1;
        }
        if k > 0 && m[k - 1] == b':' {
            cut = Some(j + 1);
            break;
        }
    }
    let mut v = match cut {
        Some(c) => m[c..].to_vec(),
        None => m.to_vec(),
    };
    if v.last() == Some(&b'"') {
        v.pop();
    }
    v
}

fn claude() -> i32 {
    let payload = read_payload();
    let base = monitor_url();
    let (ct, tt) = (Duration::from_millis(500), Duration::from_secs(5));
    let events = cat(&[&base, b"/events/claude", &host_query()]);
    let _ = send("POST", &events, Some(&payload), ct, tt);
    // The shell exits here (set -e with pipefail) when a field is missing.
    let Some(event) = grep_field(&payload, "hook_event_name") else { return 0 };
    let Some(sid) = grep_field(&payload, "session_id") else { return 0 };
    match event.as_slice() {
        b"SessionStart" => {
            let pane = env_or_empty("TMUX_PANE");
            if !pane.is_empty() && !sid.is_empty() {
                let Some(cwd) = grep_field(&payload, "cwd") else { return 0 };
                let body = cat(&[
                    b"{\"tool\":\"claude\",\"sessionId\":\"", &sid, b"\",\"paneId\":\"", &pane, b"\",\"tmuxSocket\":\"",
                    &env_or_empty("TMUX"), b"\",\"cwd\":\"", &cwd, b"\",\"source\":\"hook\"}",
                ]);
                let body = shell_capture(body);
                let _ = send("POST", &cat(&[&base, b"/api/pane/register"]), Some(&body), ct, tt);
            }
        }
        b"SessionEnd" => {
            if !sid.is_empty() {
                let _ = send("DELETE", &cat(&[&base, b"/api/pane/registration/claude:", &sid]), None, ct, tt);
            }
        }
        _ => {}
    }
    0
}

/// python3 -c 'print(json.load(sys.stdin).get(key, ""))' captured by $(...); "" on any error.
fn py_field(p: &Option<Py>, key: &str) -> Vec<u8> {
    let Some(d @ Py::Dict(_)) = p else { return Vec::new() };
    let v = d.get(key).cloned().unwrap_or(Py::Str(Vec::new()));
    match pyjson::to_utf8(&v.to_str()) {
        Some(s) => shell_capture(s.into_bytes()),
        None => Vec::new(),
    }
}

fn permission_notification(p: &Py) -> String {
    let empty = || Py::Str(Vec::new());
    let tool_input = p.get("tool_input").cloned().unwrap_or(Py::None);
    let mut detail = Py::Str(Vec::new());
    if let Py::Dict(_) = tool_input {
        for k in ["command", "description"] {
            if let Some(v) = tool_input.get(k) {
                if v.truthy() {
                    detail = v.clone();
                    break;
                }
            }
        }
    }
    let trivial = matches!(&tool_input, Py::None) || tool_input == Py::Dict(vec![]) || tool_input == Py::List(vec![]);
    if !detail.truthy() && !trivial {
        detail = Py::Str(pyjson::s(&tool_input.dumps()));
    }
    let mut d = pyjson::squash(&detail.to_str());
    if d.len() > 200 {
        d.truncate(197);
        d.extend(pyjson::s("..."));
    }
    let tool_name = match p.get("tool_name") {
        Some(v) if v.truthy() => v.to_str(),
        _ => pyjson::s("tool"),
    };
    let mut message = pyjson::s("Permission required");
    if !d.is_empty() {
        message.extend(pyjson::s(": "));
        message.extend(d);
    } else {
        message.extend(pyjson::s(" for "));
        message.extend(tool_name.iter());
    }
    Py::Dict(vec![
        (pyjson::s("session_id"), p.get("session_id").cloned().unwrap_or_else(empty)),
        (pyjson::s("transcript_path"), p.get("transcript_path").cloned().unwrap_or(Py::None)),
        (pyjson::s("cwd"), p.get("cwd").cloned().unwrap_or_else(empty)),
        (pyjson::s("hook_event_name"), Py::Str(pyjson::s("Notification"))),
        (pyjson::s("notification_type"), Py::Str(pyjson::s("permission_prompt"))),
        (pyjson::s("message"), Py::Str(message)),
        (pyjson::s("tool_name"), Py::Str(tool_name)),
    ])
    .dumps()
}

/// Lines that `while IFS= read -r` sees: newline-terminated ones only, NULs dropped.
fn read_lines(b: &[u8]) -> Vec<Vec<u8>> {
    let mut v: Vec<Vec<u8>> = b.split(|&c| c == b'\n').map(|l| l.iter().copied().filter(|&c| c != 0).collect()).collect();
    v.pop();
    v
}

/// grep -Fq -- pat: a newline in pat separates patterns; an empty pattern matches any line.
fn grep_fq(input: &[u8], pat: &[u8]) -> bool {
    if input.is_empty() {
        return false;
    }
    let mut lines: Vec<&[u8]> = input.split(|&c| c == b'\n').collect();
    if input.ends_with(b"\n") {
        lines.pop();
    }
    let pats: Vec<&[u8]> = pat.split(|&c| c == b'\n').collect();
    lines.iter().any(|l| pats.iter().any(|p| p.is_empty() || l.windows(p.len()).any(|w| w == *p)))
}

fn tmux_out(args: &[&[u8]]) -> Option<Vec<u8>> {
    let mut c = Command::new("tmux");
    for a in args {
        c.arg(OsStr::from_bytes(a));
    }
    c.stdin(Stdio::null()).stderr(Stdio::null()).output().ok().map(|o| o.stdout)
}

fn resolve_pane(sid: &[u8]) -> Option<Vec<u8>> {
    let out = Command::new("/bin/sh")
        .arg(pane_script())
        .arg(ppid().to_string())
        .env("CODEX_THREAD_ID", OsStr::from_bytes(sid))
        .stdin(Stdio::null())
        .output();
    if let Ok(o) = out {
        let pane = shell_capture(o.stdout);
        if o.status.success() && !pane.is_empty() {
            return Some(pane);
        }
    }
    let list = tmux_out(&[b"list-panes", b"-a", b"-F", b"#{pane_id}"]).unwrap_or_default();
    let mut found: Option<Vec<u8>> = None;
    for pane in read_lines(&list) {
        if pane.is_empty() {
            continue;
        }
        let cap = tmux_out(&[b"capture-pane", b"-p", b"-S", b"-100", b"-t", &pane]).unwrap_or_default();
        if grep_fq(&cap, sid) {
            if found.is_some() {
                return None;
            }
            found = Some(pane);
        }
    }
    found
}

/// int(settings["holdSeconds"] or 120), then the shell's digits check; None where curl rejects --max-time.
fn hold_max_time() -> Option<u64> {
    let dir = match env_bytes("AGENT_MONITOR_HOME") {
        Some(h) if !h.is_empty() => h,
        _ => cat(&[&env_bytes("HOME")?, b"/.agent-monitor"]),
    };
    let path = cat(&[&dir, b"/approval-settings.json"]);
    let hold = std::fs::read(OsStr::from_bytes(&path))
        .ok()
        .and_then(|b| pyjson::loads(&b).ok())
        .and_then(|v| match v {
            Py::Dict(_) => Some(v.get("holdSeconds").cloned().unwrap_or(Py::None)),
            _ => None,
        })
        .and_then(|v| if v.truthy() { pyjson::py_int(&v) } else { Some("120".into()) })
        .filter(|h| !h.is_empty() && h.bytes().all(|c| c.is_ascii_digit()))
        .unwrap_or_else(|| "120".into());
    let mut n: i64 = 0;
    for c in hold.bytes() {
        n = n.wrapping_mul(10).wrapping_add((c - b'0') as i64);
    }
    let t = n.wrapping_add(5);
    if t < 0 || t > i64::MAX / 1000 {
        return None;
    }
    Some(t as u64)
}

/// Prints the daemon's answer only when it is a well-formed Codex PermissionRequest decision.
fn valid_decision(body: &[u8]) -> Option<String> {
    let d = pyjson::loads(body).ok()?;
    let h = d.get("hookSpecificOutput")?;
    if !matches!(h, Py::Dict(_)) || h.get("hookEventName")? != &Py::Str(pyjson::s("PermissionRequest")) {
        return None;
    }
    let b = h.get("decision")?;
    if !matches!(b, Py::Dict(_)) {
        return None;
    }
    match b.get("behavior")? {
        Py::Str(v) if *v == pyjson::s("allow") || *v == pyjson::s("deny") => Some(d.dumps()),
        _ => None,
    }
}

fn codex() -> i32 {
    let payload = read_payload();
    let base = monitor_url();
    let parsed = pyjson::loads(&payload).ok();
    let event = py_field(&parsed, "hook_event_name");
    let sid = py_field(&parsed, "session_id");
    let (ct, tt) = (Duration::from_millis(200), Duration::from_millis(500));

    let forward = match (&parsed, event.as_slice()) {
        (Some(p), b"PermissionRequest") => permission_notification(p).into_bytes(),
        _ => payload.clone(),
    };
    let events = cat(&[&base, b"/events/codex", &host_query()]);
    let _ = send("POST", &events, Some(&forward), ct, tt);

    match event.as_slice() {
        b"SessionStart" | b"UserPromptSubmit" | b"Stop" | b"PermissionRequest" => {
            if !sid.is_empty() {
                if let (Some(pane), Some(p)) = (resolve_pane(&sid), &parsed) {
                    let empty = || Py::Str(Vec::new());
                    let body = Py::Dict(vec![
                        (pyjson::s("tool"), Py::Str(pyjson::s("codex"))),
                        (pyjson::s("sessionId"), p.get("session_id").cloned().unwrap_or_else(empty)),
                        (pyjson::s("paneId"), Py::Str(pyjson::from_os_bytes(&pane))),
                        (pyjson::s("tmuxSocket"), Py::Str(pyjson::from_os_bytes(&env_or_empty("TMUX")))),
                        (pyjson::s("cwd"), p.get("cwd").cloned().unwrap_or_else(empty)),
                        (pyjson::s("source"), Py::Str(pyjson::s("hook"))),
                    ])
                    .dumps();
                    let _ = send("POST", &cat(&[&base, b"/api/pane/register"]), Some(body.as_bytes()), ct, tt);
                }
            }
        }
        b"SessionEnd" => {
            if !sid.is_empty() {
                let _ = send("DELETE", &cat(&[&base, b"/api/pane/registration/codex:", &sid]), None, ct, tt);
            }
        }
        _ => {}
    }

    if event == b"PermissionRequest" {
        let Some(max) = hold_max_time() else { return 0 };
        let url = cat(&[&base, b"/api/approvals/hook/codex"]);
        if let Some(r) = send("POST", &url, Some(&payload), Duration::from_millis(500), Duration::from_secs(max)) {
            let body = shell_capture_body(r.body);
            if r.complete && r.status == 200 && !body.is_empty() {
                if let Some(out) = valid_decision(&body) {
                    let mut so = std::io::stdout().lock();
                    let _ = so.write_all(out.as_bytes());
                    let _ = so.write_all(b"\n");
                    let _ = so.flush();
                }
            }
        }
    }
    0
}

/// ${resp%$'\n'*} of curl's body plus "\n<code>": the body with NULs dropped.
fn shell_capture_body(mut b: Vec<u8>) -> Vec<u8> {
    b.retain(|&c| c != 0);
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pane_script_prefers_home_then_install_dir() {
        let home = Path::new("/h");
        let exe = Path::new("/i");
        let has = |want: &'static [&'static str]| move |p: &Path| want.iter().any(|w| Path::new(w) == p);
        assert_eq!(pick_pane_script(Some(home), Some(exe), has(&["/h/bin/tmux-pane-id.sh", "/i/bin/tmux-pane-id.sh"])), PathBuf::from("/h/bin/tmux-pane-id.sh"));
        assert_eq!(pick_pane_script(Some(home), Some(exe), has(&["/i/bin/tmux-pane-id.sh"])), PathBuf::from("/i/bin/tmux-pane-id.sh"));
        assert_eq!(pick_pane_script(Some(home), Some(exe), has(&[])), PathBuf::from("/h/bin/tmux-pane-id.sh"));
        assert_eq!(pick_pane_script(None, None, has(&[])), PathBuf::from("tmux-pane-id.sh"));
    }

    #[test]
    fn grep_and_sed_like_shell() {
        let p = br#"{"session_id":"abc","cwd" : "/x/y","hook_event_name":"SessionStart"}"#;
        assert_eq!(grep_field(p, "session_id").unwrap(), b"abc");
        assert_eq!(grep_field(p, "cwd").unwrap(), b"/x/y");
        assert_eq!(grep_field(p, "hook_event_name").unwrap(), b"SessionStart");
        assert_eq!(grep_field(br#"{"cwd":"a: "}"#, "cwd").unwrap(), b"");
        assert_eq!(grep_field(br#"{"cwd":"a\"b"}"#, "cwd").unwrap(), b"a\\");
        assert_eq!(grep_field(br#"{"x":"cwd","cwd":"q"}"#, "cwd").unwrap(), b"q");
        assert!(grep_field(br#"{"cwd":null}"#, "cwd").is_none());
    }

    #[test]
    fn urls_like_curl() {
        assert_eq!(curl_url(b"http://h:1/a/claude:x/../b").unwrap(), "http://h:1/a/b");
        assert_eq!(curl_url(b"http://h:1/a#frag").unwrap(), "http://h:1/a");
        assert_eq!(curl_url("http://h:1/\u{e9}?q=\u{e9}".as_bytes()).unwrap(), "http://h:1/%c3%a9?q=%c3%a9");
        assert_eq!(curl_url(b"h:1//x").unwrap(), "http://h:1//x");
        assert!(curl_url(b"http://h:1/a b").is_none());
        assert!(curl_url(b"https://h/").is_none());
        assert!(curl_url(b"http://h/{a}").is_none());
    }

    #[test]
    fn notification_message() {
        let p = pyjson::loads(br#"{"session_id":"s","cwd":"/w","tool_name":"Bash","tool_input":{"command":"ls   -la\n x"}}"#).unwrap();
        assert_eq!(
            permission_notification(&p),
            r#"{"session_id":"s","transcript_path":null,"cwd":"/w","hook_event_name":"Notification","notification_type":"permission_prompt","message":"Permission required: ls -la x","tool_name":"Bash"}"#
        );
        let p = pyjson::loads(br#"{"tool_input":{}}"#).unwrap();
        assert!(permission_notification(&p).contains(r#""message":"Permission required for tool""#));
        let p = pyjson::loads(br#"{"tool_input":{"command":["bash","-lc","ls"]}}"#).unwrap();
        assert!(permission_notification(&p).contains(r#"Permission required: ['bash', '-lc', 'ls']"#));
    }

    #[test]
    fn decisions() {
        assert_eq!(
            valid_decision(br#"{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"no"}}}"#).unwrap(),
            r#"{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"no"}}}"#
        );
        assert!(valid_decision(br#"{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"ask"}}}"#).is_none());
        assert!(valid_decision(b"<html>").is_none());
    }

    #[test]
    fn grep_fixed() {
        assert!(grep_fq(b"a\nxx-sid-yy\n", b"sid"));
        assert!(!grep_fq(b"", b""));
        assert!(grep_fq(b"\n", b""));
        assert!(!grep_fq(b"s\nid\n", b"sid"));
    }
}
