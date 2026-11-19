// Python json.loads / json.dumps(separators=(",", ":")) and str() semantics, for the codex hook port.

#[derive(Clone, Debug, PartialEq)]
pub enum Py {
    None,
    Bool(bool),
    Int(String),
    Float(f64),
    Str(Vec<u32>),
    List(Vec<Py>),
    Dict(Vec<(Vec<u32>, Py)>),
}

pub fn s(t: &str) -> Vec<u32> {
    t.chars().map(|c| c as u32).collect()
}

/// Bytes as Python decodes argv and environ: UTF-8 with surrogateescape.
pub fn from_os_bytes(b: &[u8]) -> Vec<u32> {
    let mut out = Vec::new();
    for chunk in b.utf8_chunks() {
        out.extend(chunk.valid().chars().map(|c| c as u32));
        out.extend(chunk.invalid().iter().map(|&x| 0xDC00 + x as u32));
    }
    out
}

/// UTF-8 bytes of a Python str as print() writes them; None when it holds a lone surrogate.
pub fn to_utf8(v: &[u32]) -> Option<String> {
    v.iter().map(|&c| char::from_u32(c)).collect()
}

impl Py {
    pub fn get(&self, key: &str) -> Option<&Py> {
        let k = s(key);
        match self {
            Py::Dict(p) => p.iter().find(|(pk, _)| *pk == k).map(|(_, v)| v),
            _ => None,
        }
    }

    pub fn truthy(&self) -> bool {
        match self {
            Py::None => false,
            Py::Bool(b) => *b,
            Py::Int(d) => d != "0",
            Py::Float(f) => *f != 0.0,
            Py::Str(v) => !v.is_empty(),
            Py::List(v) => !v.is_empty(),
            Py::Dict(v) => !v.is_empty(),
        }
    }

    /// str(value).
    pub fn to_str(&self) -> Vec<u32> {
        match self {
            Py::Str(v) => v.clone(),
            _ => s(&self.repr()),
        }
    }

    /// repr(value); non-ASCII printability is approximated.
    pub fn repr(&self) -> String {
        match self {
            Py::None => "None".into(),
            Py::Bool(true) => "True".into(),
            Py::Bool(false) => "False".into(),
            Py::Int(d) => d.clone(),
            Py::Float(f) => float_repr(*f),
            Py::Str(v) => str_repr(v),
            Py::List(v) => format!("[{}]", v.iter().map(|x| x.repr()).collect::<Vec<_>>().join(", ")),
            Py::Dict(p) => format!("{{{}}}", p.iter().map(|(k, x)| format!("{}: {}", str_repr(k), x.repr())).collect::<Vec<_>>().join(", ")),
        }
    }

    /// json.dumps(value, separators=(",", ":")) with ensure_ascii.
    pub fn dumps(&self) -> String {
        let mut o = String::new();
        self.write(&mut o);
        o
    }

    fn write(&self, o: &mut String) {
        match self {
            Py::None => o.push_str("null"),
            Py::Bool(b) => o.push_str(if *b { "true" } else { "false" }),
            Py::Int(d) => o.push_str(d),
            Py::Float(f) => o.push_str(&if f.is_nan() {
                "NaN".to_string()
            } else if f.is_infinite() {
                if *f > 0.0 { "Infinity".into() } else { "-Infinity".into() }
            } else {
                float_repr(*f)
            }),
            Py::Str(v) => write_str(v, o),
            Py::List(v) => {
                o.push('[');
                for (i, x) in v.iter().enumerate() {
                    if i > 0 {
                        o.push(',');
                    }
                    x.write(o);
                }
                o.push(']');
            }
            Py::Dict(p) => {
                o.push('{');
                for (i, (k, x)) in p.iter().enumerate() {
                    if i > 0 {
                        o.push(',');
                    }
                    write_str(k, o);
                    o.push(':');
                    x.write(o);
                }
                o.push('}');
            }
        }
    }
}

pub fn write_str(v: &[u32], o: &mut String) {
    o.push('"');
    for &c in v {
        match c {
            0x22 => o.push_str("\\\""),
            0x5c => o.push_str("\\\\"),
            0x0a => o.push_str("\\n"),
            0x0d => o.push_str("\\r"),
            0x09 => o.push_str("\\t"),
            0x08 => o.push_str("\\b"),
            0x0c => o.push_str("\\f"),
            0x20..=0x7e => o.push(c as u8 as char),
            0x10000.. => {
                let n = c - 0x10000;
                o.push_str(&format!("\\u{:04x}\\u{:04x}", 0xD800 + (n >> 10), 0xDC00 + (n & 0x3ff)));
            }
            _ => o.push_str(&format!("\\u{:04x}", c)),
        }
    }
    o.push('"');
}

fn printable(c: u32) -> bool {
    match c {
        0..=0x1f | 0x7f..=0xa0 | 0xad => false,
        0x1680 | 0x2000..=0x200f | 0x2028..=0x202f | 0x205f..=0x2064 | 0x2066..=0x206f | 0x3000 => false,
        0xd800..=0xdfff | 0xe000..=0xf8ff | 0xfeff | 0xfff9..=0xfffb | 0xf0000.. => false,
        _ => true,
    }
}

fn str_repr(v: &[u32]) -> String {
    let q = if v.contains(&0x27) && !v.contains(&0x22) { '"' } else { '\'' };
    let mut o = String::new();
    o.push(q);
    for &c in v {
        match c {
            0x5c => o.push_str("\\\\"),
            0x09 => o.push_str("\\t"),
            0x0a => o.push_str("\\n"),
            0x0d => o.push_str("\\r"),
            _ if c == q as u32 => {
                o.push('\\');
                o.push(q);
            }
            _ if printable(c) => o.push(char::from_u32(c).unwrap_or('?')),
            0..=0xff => o.push_str(&format!("\\x{:02x}", c)),
            0x100..=0xffff => o.push_str(&format!("\\u{:04x}", c)),
            _ => o.push_str(&format!("\\U{:08x}", c)),
        }
    }
    o.push(q);
    o
}

/// float.__repr__: shortest round-trip digits, exponent form when decpt <= -4 or > 16.
pub fn float_repr(f: f64) -> String {
    if f.is_nan() {
        return "nan".into();
    }
    if f.is_infinite() {
        return if f > 0.0 { "inf".into() } else { "-inf".into() };
    }
    let e = format!("{:e}", f.abs());
    let (mant, exp) = e.split_once('e').unwrap_or((&e, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let digits: String = mant.chars().filter(|c| *c != '.').collect();
    let sign = if f.is_sign_negative() { "-" } else { "" };
    if f == 0.0 {
        return format!("{}0.0", sign);
    }
    let decpt = exp + 1;
    let n = digits.len() as i32;
    let body = if decpt <= -4 || decpt > 16 {
        let m = if n == 1 { digits.clone() } else { format!("{}.{}", &digits[..1], &digits[1..]) };
        let ex = decpt - 1;
        format!("{}e{}{:02}", m, if ex < 0 { '-' } else { '+' }, ex.abs())
    } else if decpt <= 0 {
        format!("0.{}{}", "0".repeat((-decpt) as usize), digits)
    } else if decpt >= n {
        format!("{}{}.0", digits, "0".repeat((decpt - n) as usize))
    } else {
        format!("{}.{}", &digits[..decpt as usize], &digits[decpt as usize..])
    };
    format!("{}{}", sign, body)
}

/// Python str.split() whitespace.
pub fn is_space(c: u32) -> bool {
    matches!(c, 0x09..=0x0d | 0x1c..=0x20 | 0x85 | 0xa0 | 0x1680 | 0x2000..=0x200a | 0x2028 | 0x2029 | 0x202f | 0x205f | 0x3000)
}

/// " ".join(v.split()).
pub fn squash(v: &[u32]) -> Vec<u32> {
    let mut out = Vec::new();
    for w in v.split(|c| is_space(*c)).filter(|w| !w.is_empty()) {
        if !out.is_empty() {
            out.push(0x20);
        }
        out.extend_from_slice(w);
    }
    out
}

/// int(value) for a JSON-decoded value; None where Python raises.
pub fn py_int(v: &Py) -> Option<String> {
    match v {
        Py::Bool(b) => Some(if *b { "1" } else { "0" }.into()),
        Py::Int(d) => Some(d.clone()),
        Py::Float(f) if f.is_finite() => {
            let t = format!("{:.0}", f.trunc());
            Some(if t == "-0" { "0".into() } else { t })
        }
        Py::Str(t) => {
            let mut t: &[u32] = t;
            while let Some((&c, r)) = t.split_first() {
                if !is_space(c) { break; }
                t = r;
            }
            while let Some((&c, r)) = t.split_last() {
                if !is_space(c) { break; }
                t = r;
            }
            let mut neg = false;
            if let Some((&c, r)) = t.split_first() {
                if c == '-' as u32 || c == '+' as u32 {
                    neg = c == '-' as u32;
                    t = r;
                }
            }
            if t.is_empty() || t[0] == '_' as u32 || *t.last().unwrap() == '_' as u32 {
                return None;
            }
            let mut d = String::new();
            let mut prev_us = false;
            for &c in t {
                if c == '_' as u32 {
                    if prev_us { return None; }
                    prev_us = true;
                    continue;
                }
                prev_us = false;
                if !(0x30..=0x39).contains(&c) { return None; }
                d.push(c as u8 as char);
            }
            let d = d.trim_start_matches('0');
            Some(if d.is_empty() { "0".into() } else if neg { format!("-{}", d) } else { d.into() })
        }
        _ => None,
    }
}

/// json.loads; Err when Python raises (bad UTF-8, BOM, control characters, trailing data).
pub fn loads(b: &[u8]) -> Result<Py, ()> {
    let t = std::str::from_utf8(b).map_err(|_| ())?;
    if t.starts_with('\u{feff}') {
        return Err(());
    }
    let cs: Vec<char> = t.chars().collect();
    let mut p = P { c: &cs, i: 0 };
    p.ws();
    let v = p.value(0)?;
    p.ws();
    if p.i != cs.len() {
        return Err(());
    }
    Ok(v)
}

struct P<'a> {
    c: &'a [char],
    i: usize,
}

impl<'a> P<'a> {
    fn ws(&mut self) {
        while self.i < self.c.len() && matches!(self.c[self.i], ' ' | '\t' | '\n' | '\r') {
            self.i += 1;
        }
    }

    fn at(&self, w: &str) -> bool {
        let n = w.chars().count();
        self.i + n <= self.c.len() && self.c[self.i..self.i + n].iter().copied().eq(w.chars())
    }

    fn value(&mut self, depth: usize) -> Result<Py, ()> {
        if depth > 900 {
            return Err(());
        }
        let c = *self.c.get(self.i).ok_or(())?;
        match c {
            '{' => {
                self.i += 1;
                let mut d: Vec<(Vec<u32>, Py)> = Vec::new();
                self.ws();
                if self.c.get(self.i) == Some(&'}') {
                    self.i += 1;
                    return Ok(Py::Dict(d));
                }
                loop {
                    self.ws();
                    if self.c.get(self.i) != Some(&'"') {
                        return Err(());
                    }
                    self.i += 1;
                    let k = self.string()?;
                    self.ws();
                    if self.c.get(self.i) != Some(&':') {
                        return Err(());
                    }
                    self.i += 1;
                    self.ws();
                    let v = self.value(depth + 1)?;
                    match d.iter_mut().find(|(ek, _)| *ek == k) {
                        Some(e) => e.1 = v,
                        None => d.push((k, v)),
                    }
                    self.ws();
                    match self.c.get(self.i) {
                        Some(',') => self.i += 1,
                        Some('}') => {
                            self.i += 1;
                            return Ok(Py::Dict(d));
                        }
                        _ => return Err(()),
                    }
                }
            }
            '[' => {
                self.i += 1;
                let mut a = Vec::new();
                self.ws();
                if self.c.get(self.i) == Some(&']') {
                    self.i += 1;
                    return Ok(Py::List(a));
                }
                loop {
                    self.ws();
                    a.push(self.value(depth + 1)?);
                    self.ws();
                    match self.c.get(self.i) {
                        Some(',') => self.i += 1,
                        Some(']') => {
                            self.i += 1;
                            return Ok(Py::List(a));
                        }
                        _ => return Err(()),
                    }
                }
            }
            '"' => {
                self.i += 1;
                Ok(Py::Str(self.string()?))
            }
            _ if self.at("null") => {
                self.i += 4;
                Ok(Py::None)
            }
            _ if self.at("true") => {
                self.i += 4;
                Ok(Py::Bool(true))
            }
            _ if self.at("false") => {
                self.i += 5;
                Ok(Py::Bool(false))
            }
            _ if self.at("NaN") => {
                self.i += 3;
                Ok(Py::Float(f64::NAN))
            }
            _ if self.at("Infinity") => {
                self.i += 8;
                Ok(Py::Float(f64::INFINITY))
            }
            _ if self.at("-Infinity") => {
                self.i += 9;
                Ok(Py::Float(f64::NEG_INFINITY))
            }
            '-' | '0'..='9' => self.number(),
            _ => Err(()),
        }
    }

    fn digits(&mut self) -> usize {
        let s = self.i;
        while self.i < self.c.len() && self.c[self.i].is_ascii_digit() {
            self.i += 1;
        }
        self.i - s
    }

    fn number(&mut self) -> Result<Py, ()> {
        let start = self.i;
        if self.c[self.i] == '-' {
            self.i += 1;
        }
        match self.c.get(self.i) {
            Some('0') => self.i += 1,
            Some('1'..='9') => {
                self.digits();
            }
            _ => return Err(()),
        }
        let int_end = self.i;
        let mut is_float = false;
        if self.c.get(self.i) == Some(&'.') && self.c.get(self.i + 1).map_or(false, |c| c.is_ascii_digit()) {
            self.i += 1;
            self.digits();
            is_float = true;
        }
        if matches!(self.c.get(self.i), Some('e') | Some('E')) {
            let save = self.i;
            self.i += 1;
            if matches!(self.c.get(self.i), Some('+') | Some('-')) {
                self.i += 1;
            }
            if self.digits() == 0 {
                self.i = save;
            } else {
                is_float = true;
            }
        }
        let text: String = self.c[start..self.i].iter().collect();
        if is_float {
            return Ok(Py::Float(text.parse::<f64>().map_err(|_| ())?));
        }
        let t: String = self.c[start..int_end].iter().collect();
        Ok(Py::Int(if t == "-0" { "0".into() } else { t }))
    }

    fn hex4(&mut self) -> Result<u32, ()> {
        if self.i + 4 > self.c.len() {
            return Err(());
        }
        let h: String = self.c[self.i..self.i + 4].iter().collect();
        if !h.chars().all(|c| c.is_ascii_hexdigit()) {
            return Err(());
        }
        self.i += 4;
        u32::from_str_radix(&h, 16).map_err(|_| ())
    }

    fn string(&mut self) -> Result<Vec<u32>, ()> {
        let mut out = Vec::new();
        loop {
            let c = *self.c.get(self.i).ok_or(())?;
            self.i += 1;
            match c {
                '"' => return Ok(out),
                '\\' => {
                    let e = *self.c.get(self.i).ok_or(())?;
                    self.i += 1;
                    match e {
                        '"' => out.push(0x22),
                        '\\' => out.push(0x5c),
                        '/' => out.push(0x2f),
                        'b' => out.push(0x08),
                        'f' => out.push(0x0c),
                        'n' => out.push(0x0a),
                        'r' => out.push(0x0d),
                        't' => out.push(0x09),
                        'u' => {
                            let u = self.hex4()?;
                            if (0xd800..=0xdbff).contains(&u) && self.at("\\u") {
                                let save = self.i;
                                self.i += 2;
                                let l = self.hex4()?;
                                if (0xdc00..=0xdfff).contains(&l) {
                                    out.push(0x10000 + ((u - 0xd800) << 10) + (l - 0xdc00));
                                    continue;
                                }
                                self.i = save;
                            }
                            out.push(u);
                        }
                        _ => return Err(()),
                    }
                }
                c if (c as u32) < 0x20 => return Err(()),
                c => out.push(c as u32),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn floats_like_python() {
        for (f, want) in [(1.0, "1.0"), (1e16, "1e+16"), (1e15, "1000000000000000.0"), (0.0001, "0.0001"), (0.00001, "1e-05"), (-1.5e-7, "-1.5e-07"), (123.456, "123.456"), (-0.0, "-0.0"), (1.5e300, "1.5e+300")] {
            assert_eq!(float_repr(f), want, "{}", f);
        }
    }

    #[test]
    fn dumps_and_repr() {
        let v = loads(b"{\"a\":[1,\"x'y\",true,null,1.50,{\"k\":\"\\u00e9\\ud83d\\ude00\"}],\"a\":2,\"b\":\"\\ud800\"}").unwrap();
        assert_eq!(v.dumps(), r#"{"a":2,"b":"\ud800"}"#);
        let v = loads(br#"["bash","-lc","ls 'x'",1.5,false,null,{"k":"v"}]"#).unwrap();
        assert_eq!(v.repr(), r#"['bash', '-lc', "ls 'x'", 1.5, False, None, {'k': 'v'}]"#);
        let v = loads("\"\u{e9}\u{1F600}\\n\"".as_bytes()).unwrap();
        assert_eq!(v.dumps(), r#""\u00e9\ud83d\ude00\n""#);
        assert!(loads(b"{\"a\":\"x\ty\"}").is_err());
        assert!(loads(b"01").is_err());
        assert!(loads(b"{} x").is_err());
        assert_eq!(loads(b"-0").unwrap(), Py::Int("0".into()));
        assert_eq!(loads(b" [NaN,-Infinity] ").unwrap().dumps(), "[NaN,-Infinity]");
    }

    #[test]
    fn int_and_split() {
        assert_eq!(py_int(&Py::Str(s(" 1_0 "))), Some("10".into()));
        assert_eq!(py_int(&Py::Str(s("1__0"))), None);
        assert_eq!(py_int(&Py::Str(s("-007"))), Some("-7".into()));
        assert_eq!(py_int(&Py::Float(-2.9)), Some("-2".into()));
        assert_eq!(py_int(&Py::List(vec![])), None);
        assert_eq!(squash(&s("  a \u{a0}b\n\x1c c ")), s("a b c"));
    }
}
