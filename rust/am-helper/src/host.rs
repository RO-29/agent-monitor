// Port of host.go and runAgentWrapper: the session host (pty, control socket, registration, job control).

use crate::json::Json;
use crate::util::{self, getenv};
use std::io::{Read, Write};
use std::os::fd::{FromRawFd, OwnedFd};
use std::os::unix::net::{UnixListener, UnixStream};
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::{mpsc, Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

const RING_SIZE: usize = 256 << 10;
const SCREEN_LINES: i64 = 50;
const MAX_LINE: usize = 1 << 20;
const KEY_GAP: Duration = Duration::from_millis(25);
const REGISTER_EVERY: Duration = Duration::from_secs(10);

/// Threads here only relay bytes, so a small stack keeps RSS low.
pub fn spawn_small<F: FnOnce() + Send + 'static>(f: F) {
    let _ = std::thread::Builder::new().stack_size(128 << 10).spawn(f);
}

fn daemon_url() -> String {
    let p = getenv("AGENT_MONITOR_PORT");
    format!("http://127.0.0.1:{}", if p.is_empty() { "7777".to_string() } else { p })
}

fn is_terminal(fd: i32) -> bool {
    let mut t: libc::termios = unsafe { std::mem::zeroed() };
    unsafe { libc::tcgetattr(fd, &mut t) == 0 }
}

fn valid_host_id(id: &str) -> bool {
    (4..=64).contains(&id.len()) && id.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}

fn new_host_id() -> String {
    let mut b = [0u8; 8];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut b);
    }
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

/// hostChildEnv: environment without host markers inherited from an outer host.
/// The environment without the host markers; bytes pass unchanged, as Go's os.Environ does.
fn child_env() -> Vec<(std::ffi::OsString, std::ffi::OsString)> {
    std::env::vars_os().filter(|(k, _)| k != "AM_HOST_ID" && k != "AM_HOST_ID_ASSIGN").collect()
}

fn tool_from_agent(name: &str) -> String {
    match name.to_lowercase().as_str() {
        "claude" | "claude-code" => "claude".into(),
        "codex" => "codex".into(),
        "cursor-agent" => "cursor-agent".into(),
        "opencode" => "opencode".into(),
        _ => name.to_string(),
    }
}

fn announce_pane(tool: &str, pane: &str) {
    let body = Json::map(vec![
        ("tool", Json::str(tool)),
        ("paneId", Json::str(pane)),
        ("tmuxSocket", Json::str(getenv("TMUX"))),
        ("cwd", Json::str(util::go_getwd())),
        ("pid", Json::int(std::process::id() as i64)),
    ]);
    let _ = crate::http::request("POST", &(daemon_url() + "/api/pane/announce"), Some(body.encode().as_bytes()), Some(Duration::from_secs(1)));
}

/// `run <agent> [args]`: host on a pty when stdin and stdout are terminals, else exec the agent.
pub fn run_main(args: &[String]) -> i32 {
    if args.is_empty() {
        eprintln!("usage: agent-monitor run <agent> [args...]");
        eprintln!("  agents: claude | codex | cursor-agent | opencode | <any executable>");
        return 2;
    }
    let agent = &args[0];
    let pane = getenv("TMUX_PANE");
    if !pane.is_empty() {
        announce_pane(&tool_from_agent(agent), &pane);
    }
    if is_terminal(0) && is_terminal(1) {
        return run_host(args, &pane);
    }
    let bin = match util::look_path(agent) {
        Ok(b) => b,
        Err(e) => {
            eprintln!("agent-monitor run: {} not found in PATH ({})", agent, e);
            return 127;
        }
    };
    let e = Command::new(&bin).arg0(agent).args(&args[1..]).env_clear().envs(child_env()).exec();
    eprintln!("agent-monitor run: exec failed: {}", util::io_err_text(&e));
    1
}

struct Ring {
    buf: Vec<u8>,
    total: i64,
    last: Option<Instant>,
}

struct Event {
    m: Mutex<bool>,
    c: Condvar,
}

impl Event {
    fn set(&self) {
        *self.m.lock().unwrap() = true;
        self.c.notify_all();
    }
    fn is_set(&self) -> bool {
        *self.m.lock().unwrap()
    }
    /// Waits up to d; true when the event is set.
    fn wait(&self, d: Duration) -> bool {
        let g = self.m.lock().unwrap();
        let (g, _) = self.c.wait_timeout_while(g, d, |s| !*s).unwrap();
        *g
    }
}

struct Host {
    id: String,
    agent: String,
    cwd: String,
    tty_name: String,
    tmux_pane: String,
    master: i32,
    wmu: Mutex<()>,
    ring: Mutex<Ring>,
    agent_pid: AtomicI32,
    leader_pid: AtomicI32,
    has_leader: bool,
    exited: Event,
    sock: Mutex<String>,
    saved: Mutex<Option<libc::termios>>,
    register: bool,
}

fn get_winsize(fd: i32) -> Option<libc::winsize> {
    let mut ws: libc::winsize = unsafe { std::mem::zeroed() };
    if unsafe { libc::ioctl(fd, libc::TIOCGWINSZ, &mut ws) } == 0 {
        Some(ws)
    } else {
        None
    }
}

fn set_winsize(fd: i32, cols: u16, rows: u16) {
    let ws = libc::winsize { ws_row: rows, ws_col: cols, ws_xpixel: 0, ws_ypixel: 0 };
    unsafe { libc::ioctl(fd, libc::TIOCSWINSZ, &ws) };
}

fn write_all(fd: i32, mut b: &[u8]) -> Result<(), i32> {
    while !b.is_empty() {
        let n = unsafe { libc::write(fd, b.as_ptr() as *const libc::c_void, b.len()) };
        if n < 0 {
            let e = util::last_errno();
            if e == libc::EINTR || e == libc::EAGAIN {
                continue;
            }
            return Err(e);
        }
        b = &b[n as usize..];
    }
    Ok(())
}

// The cooked terminal state, for the panic hook; panic = "abort" skips every destructor.
static PANIC_TERMIOS: Mutex<Option<libc::termios>> = Mutex::new(None);

/// Restores the terminal before a panic aborts the host, then runs the default report.
fn install_panic_restore() {
    let prev = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        if let Ok(g) = PANIC_TERMIOS.try_lock() {
            if let Some(t) = *g {
                unsafe { libc::tcsetattr(0, libc::TCSANOW, &t) };
            }
        }
        let _ = std::io::stderr().write_all(b"\r\n");
        prev(info);
    }));
}

/// Test hook: put the terminal in raw mode as the host does, then panic.
pub fn panic_selftest() -> i32 {
    install_panic_restore();
    let h = Host {
        id: String::new(),
        agent: String::new(),
        cwd: String::new(),
        tty_name: String::new(),
        tmux_pane: String::new(),
        master: -1,
        wmu: Mutex::new(()),
        ring: Mutex::new(Ring { buf: Vec::new(), total: 0, last: None }),
        agent_pid: AtomicI32::new(0),
        leader_pid: AtomicI32::new(0),
        has_leader: false,
        exited: Event { m: Mutex::new(false), c: Condvar::new() },
        sock: Mutex::new(String::new()),
        saved: Mutex::new(None),
        register: false,
    };
    h.raw();
    panic!("am-helper panic self-test");
}

impl Host {
    fn raw(&self) {
        let mut saved = self.saved.lock().unwrap();
        let mut old: libc::termios = unsafe { std::mem::zeroed() };
        if unsafe { libc::tcgetattr(0, &mut old) } != 0 {
            return;
        }
        let mut raw = old;
        raw.c_iflag &= !(libc::IGNBRK | libc::BRKINT | libc::PARMRK | libc::ISTRIP | libc::INLCR | libc::IGNCR | libc::ICRNL | libc::IXON);
        raw.c_oflag &= !libc::OPOST;
        raw.c_lflag &= !(libc::ECHO | libc::ECHONL | libc::ICANON | libc::ISIG | libc::IEXTEN);
        raw.c_cflag &= !(libc::CSIZE | libc::PARENB);
        raw.c_cflag |= libc::CS8;
        raw.c_cc[libc::VMIN] = 1;
        raw.c_cc[libc::VTIME] = 0;
        if unsafe { libc::tcsetattr(0, libc::TCSANOW, &raw) } == 0 {
            *saved = Some(old);
            if let Ok(mut g) = PANIC_TERMIOS.lock() {
                *g = Some(old);
            }
        }
    }

    fn cooked(&self) {
        if let Some(old) = self.saved.lock().unwrap().take() {
            unsafe { libc::tcsetattr(0, libc::TCSANOW, &old) };
        }
    }

    fn write(&self, p: &[u8]) -> Result<(), String> {
        let _g = self.wmu.lock().unwrap();
        write_all(self.master, p).map_err(|e| format!("write /dev/ptmx: {}", util::errno_text(e)))
    }

    fn size(&self) -> (i64, i64) {
        match get_winsize(self.master) {
            Some(ws) if ws.ws_col > 0 => (ws.ws_col as i64, ws.ws_row as i64),
            _ => (80, 24),
        }
    }

    fn resize_from_tty(&self) {
        if let Some(ws) = get_winsize(0) {
            set_winsize(self.master, ws.ws_col, ws.ws_row);
        }
    }

    fn signal_agent(&self, sig: i32) {
        let pid = self.agent_pid.load(Ordering::SeqCst);
        if pid <= 0 {
            return;
        }
        unsafe {
            if libc::kill(-pid, sig) != 0 {
                libc::kill(pid, sig);
            }
        }
        let lp = self.leader_pid.load(Ordering::SeqCst);
        if lp != pid && sig == libc::SIGKILL {
            unsafe { libc::kill(lp, sig) };
        }
    }

    fn registration(&self) -> Json {
        let mut p = vec![
            ("hostId", Json::str(&self.id)),
            ("pid", Json::int(std::process::id() as i64)),
            ("agentPid", Json::int(self.agent_pid.load(Ordering::SeqCst) as i64)),
            ("agent", Json::str(&self.agent)),
            ("cwd", Json::str(&self.cwd)),
        ];
        if !self.tty_name.is_empty() {
            p.push(("tty", Json::str(&self.tty_name)));
        }
        if !self.tmux_pane.is_empty() {
            p.push(("tmuxPane", Json::str(&self.tmux_pane)));
        }
        p.push(("socket", Json::str(self.sock.lock().unwrap().clone())));
        Json::obj(p)
    }

    fn post_registration(&self) -> Result<(), String> {
        let body = self.registration().encode();
        let r = crate::http::request("POST", &(daemon_url() + "/api/hosts"), Some(body.as_bytes()), Some(Duration::from_secs(1)))?;
        if r.status != 200 {
            return Err(format!("register: {}", r.status_line));
        }
        Ok(())
    }

    fn suspend(&self) {
        if shell_job_control() {
            self.cooked();
            unsafe { libc::kill(libc::getpid(), libc::SIGTSTP) };
            self.raw();
            self.resize_from_tty();
        }
        // The leader continues the agent's process group itself.
        if self.has_leader {
            unsafe { libc::kill(self.leader_pid.load(Ordering::SeqCst), libc::SIGCONT) };
        } else {
            self.signal_agent(libc::SIGCONT);
        }
    }

    fn handle(&self, req: &Json) -> Json {
        if self.exited.is_set() {
            return resp_err("agent exited");
        }
        let op = req.fstr("op");
        match op.as_str() {
            "ping" => {
                let (total, last) = {
                    let r = self.ring.lock().unwrap();
                    (r.total, r.last)
                };
                let idle = last.map(|l| l.elapsed().as_millis() as i64).unwrap_or(0);
                resp(vec![
                    ("hostId", Json::str(&self.id)),
                    ("agent", Json::str(&self.agent)),
                    ("agentPid", Json::int(self.agent_pid.load(Ordering::SeqCst) as i64)),
                    ("outBytes", Json::int(total)),
                    ("idleMs", Json::int(idle)),
                ])
            }
            "input" => {
                let data = req.fstr("data");
                if data.is_empty() {
                    return resp(vec![]);
                }
                match self.write(data.as_bytes()) {
                    Ok(()) => resp(vec![]),
                    Err(e) => resp_err(&e),
                }
            }
            "keys" => {
                let keys: Vec<String> = req.farr("keys").iter().map(|k| k.as_str().unwrap_or("").to_string()).collect();
                let seqs = match key_bytes(&keys) {
                    Ok(s) => s,
                    Err(e) => return resp_err(&e),
                };
                for (i, s) in seqs.iter().enumerate() {
                    if i > 0 {
                        std::thread::sleep(KEY_GAP);
                    }
                    if let Err(e) = self.write(s) {
                        return resp_err(&e);
                    }
                }
                resp(vec![])
            }
            "screen" => {
                let (cols, rows) = self.size();
                let mut n = req.fint("lines");
                if n <= 0 {
                    n = SCREEN_LINES;
                }
                if n > 2000 {
                    n = 2000;
                }
                let snap = self.ring.lock().unwrap().buf.clone();
                let text = crate::screen::screen_text(&snap, cols, rows, n);
                resp(vec![("text", Json::Str(text)), ("cols", Json::int(cols)), ("rows", Json::int(rows))])
            }
            "size" => {
                let (cols, rows) = self.size();
                resp(vec![("cols", Json::int(cols)), ("rows", Json::int(rows))])
            }
            _ => resp_err(&format!("unknown op {}", op)),
        }
    }
}

/// hostResponse encoding: "ok" always, the rest omitempty, in struct order.
fn resp(fields: Vec<(&str, Json)>) -> Json {
    resp_full(true, "", fields)
}

fn resp_err(e: &str) -> Json {
    resp_full(false, e, vec![])
}

fn resp_full(ok: bool, err: &str, fields: Vec<(&str, Json)>) -> Json {
    let order = ["error", "text", "cols", "rows", "hostId", "agent", "agentPid", "outBytes", "idleMs"];
    let mut p = vec![("ok", Json::Bool(ok))];
    let mut all = fields;
    all.push(("error", Json::str(err)));
    for k in order {
        if let Some((_, v)) = all.iter().find(|(n, _)| *n == k) {
            let empty = matches!(v, Json::Str(s) if s.is_empty()) || matches!(v, Json::Num(n) if *n == 0.0);
            if !empty {
                p.push((k, v.clone()));
            }
        }
    }
    Json::obj(p)
}

fn shell_job_control() -> bool {
    let sid = unsafe { libc::getsid(0) };
    sid != -1 && sid != unsafe { libc::getpgrp() }
}

/// hostKeyBytes: tmux-style key names to terminal bytes; any unknown name fails the list.
pub fn key_bytes(keys: &[String]) -> Result<Vec<Vec<u8>>, String> {
    if keys.is_empty() {
        return Err("keys required".into());
    }
    let named = |k: &str| -> Option<&'static str> {
        Some(match k {
            "enter" | "cr" => "\r",
            "escape" | "esc" => "\x1b",
            "tab" => "\t",
            "btab" => "\x1b[Z",
            "up" => "\x1b[A",
            "down" => "\x1b[B",
            "right" => "\x1b[C",
            "left" => "\x1b[D",
            "bspace" | "backspace" => "\x7f",
            "space" => " ",
            "home" => "\x1b[H",
            "end" => "\x1b[F",
            "pageup" | "ppage" => "\x1b[5~",
            "pagedown" | "npage" => "\x1b[6~",
            "dc" | "delete" => "\x1b[3~",
            _ => return None,
        })
    };
    let mut out = Vec::with_capacity(keys.len());
    for k in keys {
        if let Some(s) = named(&k.to_lowercase()) {
            out.push(s.as_bytes().to_vec());
            continue;
        }
        let b = k.as_bytes();
        if b.len() == 3 && (b[0] == b'C' || b[0] == b'c') && b[1] == b'-' {
            let c = b[2] | 0x20;
            if c.is_ascii_lowercase() {
                out.push(vec![c - b'a' + 1]);
                continue;
            }
        }
        if k.chars().count() == 1 {
            out.push(b.to_vec());
            continue;
        }
        return Err(format!("unknown key {}", util::go_quote(k)));
    }
    Ok(out)
}

/// Validates a hostRequest the way json.Unmarshal into the Go struct would.
fn parse_request(line: &[u8]) -> Option<Json> {
    // Connection threads run on 128 KiB stacks; requests are flat objects.
    let v = crate::json::parse_depth(line, true, 64).ok()?;
    match &v {
        Json::Null => return Some(Json::Obj(vec![])),
        Json::Obj(_) => {}
        _ => return None,
    }
    let ok_str = |f: Option<&Json>| matches!(f, None | Some(Json::Null) | Some(Json::Str(_)));
    if !ok_str(v.field("op")) || !ok_str(v.field("data")) {
        return None;
    }
    match v.field("keys") {
        None | Some(Json::Null) => {}
        Some(Json::Arr(a)) if a.iter().all(|k| matches!(k, Json::Str(_) | Json::Null)) => {}
        _ => return None,
    }
    match v.field("lines") {
        None | Some(Json::Null) => {}
        Some(Json::Num(n)) if n.fract() == 0.0 && n.abs() < 9.2e18 => {}
        _ => return None,
    }
    Some(v)
}

fn serve_conn(h: Arc<Host>, mut c: UnixStream) {
    let mut buf: Vec<u8> = Vec::new();
    let mut chunk = [0u8; 16 << 10];
    let mut eof = false;
    loop {
        let _ = c.set_read_timeout(Some(Duration::from_secs(30)));
        let _ = c.set_write_timeout(Some(Duration::from_secs(30)));
        let line = loop {
            if let Some(i) = buf.iter().position(|&b| b == b'\n') {
                let mut l: Vec<u8> = buf.drain(..=i).collect();
                l.pop();
                break l;
            }
            if eof {
                if buf.is_empty() {
                    return;
                }
                break std::mem::take(&mut buf);
            }
            if buf.len() >= MAX_LINE {
                return;
            }
            match c.read(&mut chunk) {
                Ok(0) => eof = true,
                Ok(n) => buf.extend_from_slice(&chunk[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                Err(_) => return,
            }
        };
        let line = if line.last() == Some(&b'\r') { &line[..line.len() - 1] } else { &line[..] };
        let r = match parse_request(line) {
            Some(req) => h.handle(&req),
            None => resp_err("bad json"),
        };
        let mut out = r.encode().into_bytes();
        out.push(b'\n');
        if c.write_all(&out).is_err() {
            return;
        }
    }
}

fn listen(h: &Arc<Host>, dir: &str) -> Result<UnixListener, String> {
    if let Err(e) = std::fs::DirBuilder::new().recursive(true).mode(0o700).create(dir) {
        return Err(format!("mkdir {}: {}", dir, util::io_err_text(&e)));
    }
    let _ = std::fs::set_permissions(dir, std::os::unix::fs::PermissionsExt::from_mode(0o700));
    let sock = format!("{}/{}.sock", dir, h.id);
    let _ = std::fs::remove_file(&sock);
    let ln = UnixListener::bind(&sock).map_err(|e| format!("listen unix {}: bind: {}", sock, util::io_err_text(&e)))?;
    let _ = std::fs::set_permissions(&sock, std::os::unix::fs::PermissionsExt::from_mode(0o600));
    *h.sock.lock().unwrap() = sock;
    Ok(ln)
}

use std::os::unix::fs::DirBuilderExt;

/// runHostMain: host argv on the current terminal and return the agent's exit code.
fn run_host(argv: &[String], tmux_pane: &str) -> i32 {
    install_panic_restore();
    let mut id = getenv("AM_HOST_ID_ASSIGN");
    if !valid_host_id(&id) {
        id = new_host_id();
    }
    let leader = std::env::current_exe().ok().map(|p| p.to_string_lossy().into_owned());
    let tty_name = unsafe {
        let p = libc::ttyname(0);
        if p.is_null() {
            String::new()
        } else {
            std::ffi::CStr::from_ptr(p).to_string_lossy().into_owned()
        }
    };
    let bin = match util::look_path(&argv[0]) {
        Ok(b) => b,
        Err(e) => {
            eprintln!("agent-monitor run: {} not found in PATH ({})", argv[0], e);
            return 127;
        }
    };
    let cwd = util::go_getwd();

    let mut ws = libc::winsize { ws_row: 24, ws_col: 80, ws_xpixel: 0, ws_ypixel: 0 };
    if let Some(t) = get_winsize(0) {
        if t.ws_col > 0 && t.ws_row > 0 {
            ws = t;
        }
    }
    let (mut master, mut slave) = (-1i32, -1i32);
    if unsafe { libc::openpty(&mut master, &mut slave, std::ptr::null_mut(), std::ptr::null_mut(), &mut ws) } != 0 {
        eprintln!("agent-monitor run: {}", util::errno_text(util::last_errno()));
        return 126;
    }
    unsafe {
        libc::fcntl(master, libc::F_SETFD, libc::FD_CLOEXEC);
        libc::fcntl(slave, libc::F_SETFD, libc::FD_CLOEXEC);
    }
    let slave = unsafe { OwnedFd::from_raw_fd(slave) };

    let mut env = child_env();
    env.push(("AM_HOST_ID".into(), id.clone().into()));
    let mut cmd;
    let mut pid_pipe: Option<(i32, i32)> = None;
    if let Some(l) = &leader {
        cmd = Command::new(l);
        cmd.arg("__pty-leader").arg("--").arg(&bin).args(&argv[1..]);
        let mut fds = [0i32; 2];
        if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
            eprintln!("agent-monitor run: {}", util::errno_text(util::last_errno()));
            return 126;
        }
        unsafe {
            libc::fcntl(fds[0], libc::F_SETFD, libc::FD_CLOEXEC);
            libc::fcntl(fds[1], libc::F_SETFD, libc::FD_CLOEXEC);
        }
        pid_pipe = Some((fds[0], fds[1]));
    } else {
        cmd = Command::new(&bin);
        cmd.args(&argv[1..]);
    }
    cmd.env_clear().envs(env);
    if !cwd.is_empty() {
        cmd.current_dir(&cwd);
    }
    let dup = |f: &OwnedFd| f.try_clone().map(Stdio::from);
    match (dup(&slave), dup(&slave), dup(&slave)) {
        (Ok(a), Ok(b), Ok(c)) => {
            cmd.stdin(a).stdout(b).stderr(c);
        }
        _ => return 126,
    }
    let pid_w = pid_pipe.map(|p| p.1).unwrap_or(-1);
    unsafe {
        cmd.pre_exec(move || {
            if libc::setsid() < 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::ioctl(0, libc::TIOCSCTTY as _, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if pid_w >= 0 {
                if pid_w == 3 {
                    libc::fcntl(3, libc::F_SETFD, 0);
                } else if libc::dup2(pid_w, 3) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
            }
            Ok(())
        });
    }
    let child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("agent-monitor run: fork/exec {}: {}", leader.as_deref().unwrap_or(&bin), util::io_err_text(&e));
            return 126;
        }
    };
    drop(cmd);
    drop(slave);
    let cpid = child.id() as i32;
    let h = Arc::new(Host {
        id: id.clone(),
        agent: util::base_name(&argv[0]),
        cwd,
        tty_name,
        tmux_pane: tmux_pane.to_string(),
        master,
        wmu: Mutex::new(()),
        ring: Mutex::new(Ring { buf: Vec::new(), total: 0, last: None }),
        agent_pid: AtomicI32::new(cpid),
        leader_pid: AtomicI32::new(cpid),
        has_leader: leader.is_some(),
        exited: Event { m: Mutex::new(false), c: Condvar::new() },
        sock: Mutex::new(String::new()),
        saved: Mutex::new(None),
        register: true,
    });
    if let Some((r, w)) = pid_pipe {
        unsafe { libc::close(w) };
        read_agent_pid(&h, r);
    }
    h.raw();
    let ln = match listen(&h, &(util::state_dir() + "/hosts")) {
        Ok(l) => Some(l),
        Err(e) => {
            let _ = write!(std::io::stderr(), "agent-monitor run: control socket: {}\r\n", e);
            None
        }
    };
    if let Some(ln) = &ln {
        if let Ok(ln) = ln.try_clone() {
            let hh = h.clone();
            spawn_small(move || {
                // A transient accept error (EMFILE, ECONNABORTED) backs off; only a dead listener ends the loop.
                let mut delay = Duration::from_millis(5);
                for c in ln.incoming() {
                    match c {
                        Ok(c) => {
                            delay = Duration::from_millis(5);
                            let h2 = hh.clone();
                            spawn_small(move || serve_conn(h2, c));
                        }
                        Err(e) if matches!(e.raw_os_error(), Some(libc::EBADF) | Some(libc::EINVAL) | Some(libc::ENOTSOCK)) => return,
                        Err(_) => {
                            std::thread::sleep(delay);
                            delay = (delay * 2).min(Duration::from_secs(1));
                        }
                    }
                }
            });
        }
    }

    let (out_tx, out_rx) = mpsc::channel::<()>();
    {
        let h = h.clone();
        spawn_small(move || {
            let mut b = vec![0u8; 32 << 10];
            loop {
                let n = unsafe { libc::read(h.master, b.as_mut_ptr() as *mut libc::c_void, b.len()) };
                if n < 0 && util::last_errno() == libc::EINTR {
                    continue;
                }
                if n <= 0 {
                    break;
                }
                let p = &b[..n as usize];
                {
                    let mut r = h.ring.lock().unwrap();
                    r.buf.extend_from_slice(p);
                    if r.buf.len() > RING_SIZE {
                        let cut = r.buf.len() - RING_SIZE;
                        r.buf.drain(..cut);
                    }
                    r.total += p.len() as i64;
                    r.last = Some(Instant::now());
                }
                // A dead terminal must not stall the agent, so write errors are ignored.
                let _ = write_all(1, p);
            }
            let _ = out_tx.send(());
        });
    }
    {
        let h = h.clone();
        spawn_small(move || {
            let mut b = vec![0u8; 32 << 10];
            loop {
                let n = unsafe { libc::read(0, b.as_mut_ptr() as *mut libc::c_void, b.len()) };
                if n < 0 && util::last_errno() == libc::EINTR {
                    continue;
                }
                if n <= 0 || h.write(&b[..n as usize]).is_err() {
                    return;
                }
            }
        });
    }
    let sigs = [libc::SIGWINCH, libc::SIGINT, libc::SIGTERM, libc::SIGHUP];
    let sig_fd = crate::sig::notify(&sigs);
    let stop = Arc::new(AtomicBool::new(false));
    {
        let h = h.clone();
        let stop = stop.clone();
        spawn_small(move || {
            while let Some(s) = crate::sig::next(sig_fd) {
                if stop.load(Ordering::SeqCst) {
                    return;
                }
                if s == libc::SIGWINCH {
                    if !h.exited.is_set() {
                        h.resize_from_tty();
                    }
                    continue;
                }
                h.signal_agent(s);
                if s == libc::SIGHUP {
                    let h = h.clone();
                    spawn_small(move || {
                        if !h.exited.wait(Duration::from_secs(10)) {
                            h.signal_agent(libc::SIGKILL);
                        }
                    });
                }
            }
        });
    }
    if h.register {
        let h = h.clone();
        spawn_small(move || {
            let mut delay = Duration::from_millis(500);
            loop {
                let err = h.post_registration().is_err();
                let mut wait = REGISTER_EVERY;
                if err && delay < REGISTER_EVERY {
                    wait = delay;
                    delay *= 2;
                }
                if h.exited.wait(wait) {
                    return;
                }
            }
        });
    }

    let code = loop {
        let st = match crate::sig::wait_untraced(cpid) {
            Ok(st) => st,
            Err(_) => break 1,
        };
        if crate::sig::stopped(st) {
            h.suspend();
            continue;
        }
        if let Some(c) = crate::sig::code_of(st) {
            break c;
        }
    };
    h.exited.set();
    // Grandchildren can hold the pty open, so the drain wait is bounded.
    if out_rx.recv_timeout(Duration::from_millis(500)).is_ok() {
        unsafe { libc::close(h.master) };
    }
    let _ = crate::http::request("DELETE", &(daemon_url() + "/api/hosts/" + &h.id), None, Some(Duration::from_millis(300)));
    stop.store(true, Ordering::SeqCst);
    crate::sig::reset(&sigs);
    drop(ln);
    let sock = h.sock.lock().unwrap().clone();
    if !sock.is_empty() {
        let _ = std::fs::remove_file(&sock);
    }
    h.cooked();
    code
}

/// readAgentPid: the leader writes the agent's pid; wait at most 2 s.
fn read_agent_pid(h: &Arc<Host>, r: i32) {
    let (tx, rx) = mpsc::channel::<i32>();
    spawn_small(move || {
        let mut f = unsafe { std::fs::File::from_raw_fd(r) };
        let mut s = String::new();
        let _ = f.read_to_string(&mut s);
        let t = s.trim_start();
        let digits: String = t.chars().take_while(|c| c.is_ascii_digit()).collect();
        if let Ok(pid) = digits.parse::<i32>() {
            if pid > 0 {
                let _ = tx.send(pid);
            }
        }
    });
    if let Ok(pid) = rx.recv_timeout(Duration::from_secs(2)) {
        h.agent_pid.store(pid, Ordering::SeqCst);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_names() {
        let k: Vec<String> = ["Enter", "Escape", "C-c", "Up", "3", "BTab"].iter().map(|s| s.to_string()).collect();
        let got = key_bytes(&k).unwrap();
        let want: Vec<&[u8]> = vec![b"\r", b"\x1b", b"\x03", b"\x1b[A", b"3", b"\x1b[Z"];
        for (g, w) in got.iter().zip(want) {
            assert_eq!(g.as_slice(), w);
        }
        assert_eq!(key_bytes(&["Enter".into(), "Bogus".into()]).err().unwrap(), "unknown key \"Bogus\"");
        assert_eq!(key_bytes(&[]).err().unwrap(), "keys required");
    }

    #[test]
    fn response_omits_empty() {
        assert_eq!(resp(vec![]).encode(), r#"{"ok":true}"#);
        assert_eq!(resp_err("bad json").encode(), r#"{"ok":false,"error":"bad json"}"#);
        assert_eq!(resp(vec![("cols", Json::int(80)), ("rows", Json::int(24))]).encode(), r#"{"ok":true,"cols":80,"rows":24}"#);
    }

    #[test]
    fn request_validation() {
        assert!(parse_request(br#"{"op":"ping"}"#).is_some());
        assert!(parse_request(br#"{"op":1}"#).is_none());
        assert!(parse_request(br#"{"op":"keys","keys":[1]}"#).is_none());
        assert!(parse_request(br#"{"op":"screen","lines":1.5}"#).is_none());
        assert!(parse_request(b"[]").is_none());
        assert!(parse_request(b"null").is_some());
        let deep = format!("{}{}", "[".repeat(70), "]".repeat(70));
        assert!(parse_request(deep.as_bytes()).is_none());
        assert!(parse_request(format!("{}{}", "[".repeat(200000), "]".repeat(200000)).as_bytes()).is_none());
    }

    #[test]
    fn child_env_drops_markers() {
        assert!(valid_host_id("abcd") && !valid_host_id("ab") && !valid_host_id("a/bcd"));
        use std::os::unix::ffi::OsStrExt;
        std::env::set_var("AM_TEST_RAW", std::ffi::OsStr::from_bytes(b"a\xffb"));
        std::env::set_var("AM_HOST_ID_ASSIGN", "x");
        let e = child_env();
        assert!(e.iter().any(|(k, v)| k == "AM_TEST_RAW" && v.as_bytes() == b"a\xffb"));
        assert!(!e.iter().any(|(k, _)| k == "AM_HOST_ID_ASSIGN"));
    }
}
