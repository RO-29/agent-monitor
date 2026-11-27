// Tiny blocking HTTP/1.1 client for the loopback daemon; errors read like Go's *url.Error.

use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::time::{Duration, Instant};

pub struct Response {
    pub status: u16,
    pub status_line: String,
    pub body: Vec<u8>,
    /// False when the connection closed before the declared body length arrived.
    pub complete: bool,
}

fn go_method(m: &str) -> String {
    let l = m.to_lowercase();
    let mut c = l.chars();
    match c.next() {
        Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
        None => String::new(),
    }
}

fn io_text(e: &std::io::Error) -> String {
    match e.raw_os_error() {
        Some(libc::ECONNREFUSED) => "connect: connection refused".into(),
        Some(libc::ECONNRESET) => "read: connection reset by peer".into(),
        Some(libc::EHOSTUNREACH) => "connect: no route to host".into(),
        Some(libc::ENETUNREACH) => "connect: network is unreachable".into(),
        _ => e.to_string(),
    }
}

/// Sends one request with Connection: close. timeout None waits forever, as http.DefaultClient does.
pub fn request(method: &str, url: &str, body: Option<&[u8]>, timeout: Option<Duration>) -> Result<Response, String> {
    request_ct(method, url, body, timeout, timeout)
}

/// request with a separate connect limit, as curl --connect-timeout with --max-time.
pub fn request_ct(method: &str, url: &str, body: Option<&[u8]>, connect: Option<Duration>, timeout: Option<Duration>) -> Result<Response, String> {
    let wrap = |msg: String| format!("{} {}: {}", go_method(method), crate::util::go_quote(url), msg);
    let rest = match url.strip_prefix("http://") {
        Some(r) => r,
        None => return Err(wrap(format!("unsupported protocol scheme {}", crate::util::go_quote(url.split(':').next().unwrap_or(""))))),
    };
    let (hostport, path) = match rest.find('/') {
        Some(i) => (&rest[..i], &rest[i..]),
        None => match rest.find('?') {
            Some(i) => (&rest[..i], &rest[i..]),
            None => (rest, "/"),
        },
    };
    let path = if path.starts_with('?') { format!("/{}", path) } else { path.to_string() };
    let addr_str = if hostport.rsplit(':').next().map_or(true, |p| p.is_empty() || !p.chars().all(|c| c.is_ascii_digit()) || hostport.ends_with(']')) {
        format!("{}:80", hostport)
    } else {
        hostport.to_string()
    };
    let deadline = timeout.map(|t| Instant::now() + t);
    let addrs: Vec<_> = match addr_str.to_socket_addrs() {
        Ok(a) => a.collect(),
        Err(e) => return Err(wrap(format!("dial tcp: lookup {}: {}", hostport, e))),
    };
    let mut last_err = String::from("no addresses");
    let mut stream = None;
    for a in &addrs {
        let lim = match (connect, deadline) {
            (Some(c), Some(d)) => Some(c.min(d.saturating_duration_since(Instant::now()))),
            (c, None) => c,
            (None, Some(d)) => Some(d.saturating_duration_since(Instant::now())),
        };
        let r = match lim {
            Some(t) if t.is_zero() => Err(std::io::Error::from(std::io::ErrorKind::TimedOut)),
            Some(t) => TcpStream::connect_timeout(a, t),
            None => TcpStream::connect(a),
        };
        match r {
            Ok(s) => {
                stream = Some(s);
                break;
            }
            Err(e) => {
                last_err = if e.kind() == std::io::ErrorKind::TimedOut {
                    "context deadline exceeded (Client.Timeout exceeded while awaiting headers)".into()
                } else {
                    format!("dial tcp {}: {}", a, io_text(&e))
                }
            }
        }
    }
    let mut s = match stream {
        Some(s) => s,
        None => return Err(wrap(last_err)),
    };
    let timeout_err = || wrap("context deadline exceeded (Client.Timeout exceeded while awaiting headers)".into());
    let set_left = |s: &TcpStream| -> bool {
        if let Some(d) = deadline {
            let now = Instant::now();
            if now >= d {
                return false;
            }
            let _ = s.set_read_timeout(Some(d - now));
            let _ = s.set_write_timeout(Some(d - now));
        }
        true
    };
    let body = body.unwrap_or(b"");
    let mut head = format!("{} {} HTTP/1.1\r\nHost: {}\r\nUser-Agent: Go-http-client/1.1\r\n", method, path, hostport);
    if method == "POST" || method == "PUT" || !body.is_empty() {
        head.push_str(&format!("Content-Length: {}\r\n", body.len()));
    }
    if method == "POST" {
        head.push_str("Content-Type: application/json\r\n");
    }
    head.push_str("Connection: close\r\n\r\n");
    let mut msg = head.into_bytes();
    msg.extend_from_slice(body);
    if !set_left(&s) {
        return Err(timeout_err());
    }
    if let Err(e) = s.write_all(&msg) {
        return Err(if e.kind() == std::io::ErrorKind::WouldBlock || e.kind() == std::io::ErrorKind::TimedOut { timeout_err() } else { wrap(io_text(&e)) });
    }
    let mut raw = Vec::new();
    let mut buf = [0u8; 16 << 10];
    let mut header_end = None;
    loop {
        if !set_left(&s) {
            return Err(timeout_err());
        }
        match s.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                raw.extend_from_slice(&buf[..n]);
                if header_end.is_none() {
                    header_end = find(&raw, b"\r\n\r\n");
                }
                if let Some(h) = header_end {
                    if let Some(total) = complete_len(&raw[..h], &raw[h + 4..]) {
                        raw.truncate(h + 4 + total);
                        break;
                    }
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock || e.kind() == std::io::ErrorKind::TimedOut => return Err(timeout_err()),
            Err(e) => return Err(wrap(io_text(&e))),
        }
    }
    let h = match header_end {
        Some(h) => h,
        None => return Err(wrap("EOF".into())),
    };
    let head = String::from_utf8_lossy(&raw[..h]).into_owned();
    let mut lines = head.split("\r\n");
    let status_line = lines.next().unwrap_or("");
    let mut parts = status_line.splitn(2, ' ');
    let _proto = parts.next();
    let st = parts.next().unwrap_or("").to_string();
    let status: u16 = st.split(' ').next().unwrap_or("0").parse().unwrap_or(0);
    let mut chunked = false;
    for l in lines {
        if let Some((k, v)) = l.split_once(':') {
            if k.trim().eq_ignore_ascii_case("transfer-encoding") && v.trim().eq_ignore_ascii_case("chunked") {
                chunked = true;
            }
        }
    }
    let rest = &raw[h + 4..];
    let complete = complete_len(&raw[..h], rest).is_some() || !has_length(&raw[..h]);
    let body = if chunked { dechunk(rest) } else { rest.to_vec() };
    Ok(Response { status, status_line: st, body, complete })
}

fn has_length(head: &[u8]) -> bool {
    String::from_utf8_lossy(head).split("\r\n").skip(1).any(|l| {
        l.split_once(':').map_or(false, |(k, v)| {
            let k = k.trim();
            k.eq_ignore_ascii_case("content-length") || (k.eq_ignore_ascii_case("transfer-encoding") && v.trim().eq_ignore_ascii_case("chunked"))
        })
    })
}

fn find(h: &[u8], n: &[u8]) -> Option<usize> {
    h.windows(n.len()).position(|w| w == n)
}

/// Returns the body length once the response is complete: Content-Length, or a chunked terminator.
fn complete_len(head: &[u8], body: &[u8]) -> Option<usize> {
    let head = String::from_utf8_lossy(head);
    for l in head.split("\r\n").skip(1) {
        if let Some((k, v)) = l.split_once(':') {
            if k.trim().eq_ignore_ascii_case("content-length") {
                let n: usize = v.trim().parse().ok()?;
                return if body.len() >= n { Some(n) } else { None };
            }
            if k.trim().eq_ignore_ascii_case("transfer-encoding") && v.trim().eq_ignore_ascii_case("chunked") {
                return chunked_end(body);
            }
        }
    }
    if head.starts_with("HTTP/1.1 204") || head.starts_with("HTTP/1.1 304") {
        return Some(0);
    }
    None
}

fn chunked_end(b: &[u8]) -> Option<usize> {
    let mut i = 0;
    loop {
        let e = find(&b[i..], b"\r\n")? + i;
        let size_s = String::from_utf8_lossy(&b[i..e]);
        let size = usize::from_str_radix(size_s.split(';').next().unwrap_or("").trim(), 16).ok()?;
        i = e + 2;
        if size == 0 {
            let t = find(&b[i..], b"\r\n")? + i;
            if t == i {
                return Some(i + 2);
            }
            i = t + 2;
            loop {
                let t = find(&b[i..], b"\r\n")? + i;
                if t == i {
                    return Some(i + 2);
                }
                i = t + 2;
            }
        }
        if b.len() < i + size + 2 {
            return None;
        }
        i += size + 2;
    }
}

fn dechunk(b: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    let mut i = 0;
    while let Some(e) = find(&b[i..], b"\r\n").map(|p| p + i) {
        let size_s = String::from_utf8_lossy(&b[i..e]);
        let size = match usize::from_str_radix(size_s.split(';').next().unwrap_or("").trim(), 16) {
            Ok(n) => n,
            Err(_) => break,
        };
        i = e + 2;
        if size == 0 || i + size > b.len() {
            break;
        }
        out.extend_from_slice(&b[i..i + size]);
        i += size + 2;
        if i > b.len() {
            break;
        }
    }
    out
}

/// A hung daemon must not freeze an MCP tool forever; the long waits pass their own limit to request.
const JSON_TIMEOUT: Duration = Duration::from_secs(60);

/// httpJSON: GET and decode; status >= 400 becomes "HTTP <code>: <body>".
pub fn get_json(url: &str) -> Result<crate::json::Json, String> {
    let r = request("GET", url, None, Some(JSON_TIMEOUT))?;
    decode(r)
}

/// httpPostJSON.
pub fn post_json(url: &str, body: &[u8]) -> Result<crate::json::Json, String> {
    let r = request("POST", url, Some(body), Some(JSON_TIMEOUT))?;
    decode(r)
}

pub fn decode(r: Response) -> Result<crate::json::Json, String> {
    if r.status >= 400 {
        return Err(format!("HTTP {}: {}", r.status, String::from_utf8_lossy(&r.body).trim()));
    }
    if r.body.iter().all(|c| c.is_ascii_whitespace()) {
        return Err("EOF".into());
    }
    crate::json::parse(&r.body, false).map_err(|e| if e.0 == "unexpected end of JSON input" { "unexpected EOF".into() } else { e.0 })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chunked_bodies() {
        let b = b"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n";
        assert_eq!(chunked_end(b), Some(b.len()));
        assert_eq!(dechunk(b), b"hello world");
        assert_eq!(chunked_end(b"5\r\nhel"), None);
    }

    #[test]
    fn refused_reads_like_go() {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        drop(l);
        let url = format!("http://127.0.0.1:{}/api/panes", port);
        let e = request("GET", &url, None, None).err().unwrap();
        assert_eq!(e, format!("Get \"{}\": dial tcp 127.0.0.1:{}: connect: connection refused", url, port));
    }
}
