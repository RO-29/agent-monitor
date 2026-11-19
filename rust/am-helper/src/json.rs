// Minimal JSON value with Go encoding/json compatible output (key order as built, HTML-safe escapes).

#[derive(Clone, Debug, PartialEq)]
pub enum Json {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Json>),
    Obj(Vec<(String, Json)>),
}

impl Json {
    pub fn str(s: impl Into<String>) -> Json {
        Json::Str(s.into())
    }

    pub fn int(n: i64) -> Json {
        Json::Num(n as f64)
    }

    /// An object with Go map semantics: keys sorted, the last duplicate wins; values keep their own order.
    pub fn map(pairs: Vec<(&str, Json)>) -> Json {
        let mut out: Vec<(String, Json)> = Vec::with_capacity(pairs.len());
        for (k, v) in pairs {
            match out.iter_mut().find(|(ek, _)| ek == k) {
                Some(e) => e.1 = v,
                None => out.push((k.to_string(), v)),
            }
        }
        out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
        Json::Obj(out)
    }

    /// An object in struct field order.
    pub fn obj(pairs: Vec<(&str, Json)>) -> Json {
        Json::Obj(pairs.into_iter().map(|(k, v)| (k.to_string(), v)).collect())
    }

    /// Converts every object to Go map order, as decoding into map[string]any and encoding again does.
    pub fn gomap(self) -> Json {
        match self {
            Json::Obj(pairs) => {
                let mut out: Vec<(String, Json)> = Vec::with_capacity(pairs.len());
                for (k, v) in pairs {
                    let v = v.gomap();
                    if let Some(e) = out.iter_mut().find(|(ek, _)| *ek == k) {
                        e.1 = v;
                    } else {
                        out.push((k, v));
                    }
                }
                out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
                Json::Obj(out)
            }
            Json::Arr(a) => Json::Arr(a.into_iter().map(|v| v.gomap()).collect()),
            v => v,
        }
    }

    /// Exact key lookup, last duplicate wins (map[string]any decoding).
    pub fn get(&self, key: &str) -> Option<&Json> {
        match self {
            Json::Obj(p) => p.iter().rev().find(|(k, _)| k == key).map(|(_, v)| v),
            _ => None,
        }
    }

    /// Struct field lookup: the last key equal to name, ignoring case, wins (Go struct decoding).
    pub fn field(&self, name: &str) -> Option<&Json> {
        match self {
            Json::Obj(p) => p
                .iter()
                .rev()
                .find(|(k, _)| k == name || fold_eq(k, name))
                .map(|(_, v)| v),
            _ => None,
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Json::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Json::Num(n) => Some(*n),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Json::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_arr(&self) -> Option<&Vec<Json>> {
        match self {
            Json::Arr(a) => Some(a),
            _ => None,
        }
    }

    pub fn is_obj(&self) -> bool {
        matches!(self, Json::Obj(_))
    }

    /// String struct field, "" when absent or not a string.
    pub fn fstr(&self, name: &str) -> String {
        self.field(name).and_then(|v| v.as_str()).unwrap_or("").to_string()
    }

    /// Integer struct field, 0 when absent or not a number.
    pub fn fint(&self, name: &str) -> i64 {
        self.field(name).and_then(|v| v.as_f64()).map(|f| f as i64).unwrap_or(0)
    }

    pub fn fbool(&self, name: &str) -> bool {
        self.field(name).and_then(|v| v.as_bool()).unwrap_or(false)
    }

    /// Array struct field; null or absent gives an empty list.
    pub fn farr(&self, name: &str) -> Vec<Json> {
        self.field(name).and_then(|v| v.as_arr()).cloned().unwrap_or_default()
    }

    pub fn encode(&self) -> String {
        let mut s = String::new();
        self.write(&mut s);
        s
    }

    fn write(&self, out: &mut String) {
        match self {
            Json::Null => out.push_str("null"),
            Json::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Json::Num(n) => out.push_str(&go_float(*n)),
            Json::Str(s) => write_str(out, s),
            Json::Arr(a) => {
                out.push('[');
                for (i, v) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    v.write(out);
                }
                out.push(']');
            }
            Json::Obj(p) => {
                out.push('{');
                for (i, (k, v)) in p.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write_str(out, k);
                    out.push(':');
                    v.write(out);
                }
                out.push('}');
            }
        }
    }
}

fn fold_eq(a: &str, b: &str) -> bool {
    a.len() == b.len() && a.chars().zip(b.chars()).all(|(x, y)| x == y || x.to_lowercase().eq(y.to_lowercase()))
}

/// Go's float64 encoding: shortest digits, exponent form below 1e-6 and from 1e21.
pub fn go_float(f: f64) -> String {
    let a = f.abs();
    if a != 0.0 && (a < 1e-6 || a >= 1e21) {
        let s = format!("{:e}", f);
        match s.find('e') {
            Some(i) if !s[i + 1..].starts_with('-') => format!("{}e+{}", &s[..i], &s[i + 1..]),
            _ => s,
        }
    } else {
        format!("{}", f)
    }
}

fn write_str(out: &mut String, s: &str) {
    const HEX: &[u8] = b"0123456789abcdef";
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' | '>' | '&' | '\u{0}'..='\u{1f}' => {
                let b = c as u32 as u8;
                out.push_str("\\u00");
                out.push(HEX[(b >> 4) as usize] as char);
                out.push(HEX[(b & 15) as usize] as char);
            }
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c => out.push(c),
        }
    }
    out.push('"');
}

pub struct ParseError(pub String);

/// Parses one JSON value. With strict, trailing non-space fails (json.Unmarshal); without, it is ignored (Decoder.Decode).
pub fn parse(b: &[u8], strict: bool) -> Result<Json, ParseError> {
    parse_depth(b, strict, 10000)
}

/// parse with a nesting limit, for callers on small thread stacks.
pub fn parse_depth(b: &[u8], strict: bool, max_depth: usize) -> Result<Json, ParseError> {
    let mut p = Parser { b, i: 0, max_depth };
    p.ws();
    if p.i >= b.len() {
        return Err(ParseError("unexpected end of JSON input".into()));
    }
    let v = p.value(0)?;
    if strict {
        p.ws();
        if p.i < b.len() {
            return Err(p.err_char("after top-level value"));
        }
    }
    Ok(v)
}

struct Parser<'a> {
    b: &'a [u8],
    i: usize,
    max_depth: usize,
}

impl<'a> Parser<'a> {
    fn ws(&mut self) {
        while self.i < self.b.len() && matches!(self.b[self.i], b' ' | b'\t' | b'\n' | b'\r') {
            self.i += 1;
        }
    }

    fn err_char(&self, ctx: &str) -> ParseError {
        if self.i >= self.b.len() {
            return ParseError("unexpected end of JSON input".into());
        }
        let c = self.b[self.i];
        let q = if c == b'\'' { "'\\''".to_string() } else if c == b'"' { "'\"'".to_string() } else { format!("'{}'", c as char) };
        ParseError(format!("invalid character {} {}", q, ctx))
    }

    fn value(&mut self, depth: usize) -> Result<Json, ParseError> {
        if depth > self.max_depth {
            return Err(ParseError("exceeded max depth".into()));
        }
        self.ws();
        if self.i >= self.b.len() {
            return Err(ParseError("unexpected end of JSON input".into()));
        }
        match self.b[self.i] {
            b'{' => {
                self.i += 1;
                let mut pairs = Vec::new();
                self.ws();
                if self.peek() == Some(b'}') {
                    self.i += 1;
                    return Ok(Json::Obj(pairs));
                }
                loop {
                    self.ws();
                    if self.peek() != Some(b'"') {
                        return Err(self.err_char("looking for beginning of object key string"));
                    }
                    let k = self.string()?;
                    self.ws();
                    if self.peek() != Some(b':') {
                        return Err(self.err_char("after object key"));
                    }
                    self.i += 1;
                    let v = self.value(depth + 1)?;
                    pairs.push((k, v));
                    self.ws();
                    match self.peek() {
                        Some(b',') => self.i += 1,
                        Some(b'}') => {
                            self.i += 1;
                            return Ok(Json::Obj(pairs));
                        }
                        _ => return Err(self.err_char("after object key:value pair")),
                    }
                }
            }
            b'[' => {
                self.i += 1;
                let mut arr = Vec::new();
                self.ws();
                if self.peek() == Some(b']') {
                    self.i += 1;
                    return Ok(Json::Arr(arr));
                }
                loop {
                    arr.push(self.value(depth + 1)?);
                    self.ws();
                    match self.peek() {
                        Some(b',') => self.i += 1,
                        Some(b']') => {
                            self.i += 1;
                            return Ok(Json::Arr(arr));
                        }
                        _ => return Err(self.err_char("after array element")),
                    }
                }
            }
            b'"' => Ok(Json::Str(self.string()?)),
            b't' => self.lit("true", Json::Bool(true)),
            b'f' => self.lit("false", Json::Bool(false)),
            b'n' => self.lit("null", Json::Null),
            b'-' | b'0'..=b'9' => self.number(),
            _ => Err(self.err_char("looking for beginning of value")),
        }
    }

    fn peek(&self) -> Option<u8> {
        self.b.get(self.i).copied()
    }

    fn lit(&mut self, w: &str, v: Json) -> Result<Json, ParseError> {
        let wb = w.as_bytes();
        for (k, &c) in wb.iter().enumerate() {
            match self.b.get(self.i + k) {
                Some(&x) if x == c => {}
                Some(_) => {
                    self.i += k;
                    return Err(self.err_char(&format!("in literal {} (expecting '{}')", w, c as char)));
                }
                None => return Err(ParseError("unexpected end of JSON input".into())),
            }
        }
        self.i += wb.len();
        Ok(v)
    }

    fn number(&mut self) -> Result<Json, ParseError> {
        let st = self.i;
        let b = self.b;
        let mut i = self.i;
        if b.get(i) == Some(&b'-') {
            i += 1;
        }
        match b.get(i) {
            Some(b'0') => i += 1,
            Some(b'1'..=b'9') => {
                while matches!(b.get(i), Some(b'0'..=b'9')) {
                    i += 1;
                }
            }
            _ => {
                self.i = i;
                return Err(self.err_char("in numeric literal"));
            }
        }
        if b.get(i) == Some(&b'.') {
            i += 1;
            if !matches!(b.get(i), Some(b'0'..=b'9')) {
                self.i = i;
                return Err(self.err_char("after decimal point in numeric literal"));
            }
            while matches!(b.get(i), Some(b'0'..=b'9')) {
                i += 1;
            }
        }
        if matches!(b.get(i), Some(b'e') | Some(b'E')) {
            i += 1;
            if matches!(b.get(i), Some(b'+') | Some(b'-')) {
                i += 1;
            }
            if !matches!(b.get(i), Some(b'0'..=b'9')) {
                self.i = i;
                return Err(self.err_char("in exponent of numeric literal"));
            }
            while matches!(b.get(i), Some(b'0'..=b'9')) {
                i += 1;
            }
        }
        self.i = i;
        let s = std::str::from_utf8(&b[st..i]).unwrap_or("0");
        Ok(Json::Num(s.parse::<f64>().unwrap_or(0.0)))
    }

    fn hex4(&self, at: usize) -> Option<u32> {
        let s = self.b.get(at..at + 4)?;
        let mut v = 0u32;
        for &c in s {
            v = v * 16 + (c as char).to_digit(16)?;
        }
        Some(v)
    }

    fn string(&mut self) -> Result<String, ParseError> {
        self.i += 1;
        let mut out: Vec<u8> = Vec::new();
        loop {
            let c = match self.peek() {
                Some(c) => c,
                None => return Err(ParseError("unexpected end of JSON input".into())),
            };
            match c {
                b'"' => {
                    self.i += 1;
                    return Ok(String::from_utf8_lossy(&out).into_owned());
                }
                b'\\' => {
                    let e = match self.b.get(self.i + 1) {
                        Some(&e) => e,
                        None => return Err(ParseError("unexpected end of JSON input".into())),
                    };
                    self.i += 2;
                    let ch = match e {
                        b'"' => '"',
                        b'\\' => '\\',
                        b'/' => '/',
                        b'b' => '\u{8}',
                        b'f' => '\u{c}',
                        b'n' => '\n',
                        b'r' => '\r',
                        b't' => '\t',
                        b'u' => {
                            let u = match self.hex4(self.i) {
                                Some(u) => u,
                                None => return Err(self.err_char("in \\u hexadecimal character escape")),
                            };
                            self.i += 4;
                            if (0xD800..0xDC00).contains(&u) {
                                if self.b.get(self.i) == Some(&b'\\') && self.b.get(self.i + 1) == Some(&b'u') {
                                    if let Some(l) = self.hex4(self.i + 2) {
                                        if (0xDC00..0xE000).contains(&l) {
                                            self.i += 6;
                                            let cp = 0x10000 + ((u - 0xD800) << 10) + (l - 0xDC00);
                                            char::from_u32(cp).unwrap_or('\u{fffd}')
                                        } else {
                                            '\u{fffd}'
                                        }
                                    } else {
                                        '\u{fffd}'
                                    }
                                } else {
                                    '\u{fffd}'
                                }
                            } else {
                                char::from_u32(u).unwrap_or('\u{fffd}')
                            }
                        }
                        _ => {
                            self.i -= 1;
                            return Err(self.err_char("in string escape code"));
                        }
                    };
                    let mut buf = [0u8; 4];
                    out.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                }
                0..=0x1f => return Err(self.err_char("in string literal")),
                _ => {
                    out.push(c);
                    self.i += 1;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn go_escapes() {
        let v = Json::str("<a&b>\u{2028}\n\u{1}\u{8}\u{7f}é");
        assert_eq!(v.encode(), "\"\\u003ca\\u0026b\\u003e\\u2028\\n\\u0001\\b\u{7f}é\"");
    }

    #[test]
    fn go_floats() {
        assert_eq!(go_float(1.0), "1");
        assert_eq!(go_float(1.5), "1.5");
        assert_eq!(go_float(1e21), "1e+21");
        assert_eq!(go_float(1.5e-7), "1.5e-7");
        assert_eq!(go_float(1e20), "100000000000000000000");
        assert_eq!(go_float(-0.000001), "-0.000001");
    }

    #[test]
    fn map_order_and_roundtrip() {
        let v = parse(br#"{"b":1,"a":{"z":[1,2,{"y":null,"x":true}],"c":"s"},"b":2}"#, true).ok().unwrap();
        assert_eq!(v.gomap().encode(), r#"{"a":{"c":"s","z":[1,2,{"x":true,"y":null}]},"b":2}"#);
        assert!(parse(b"{} x", true).is_err());
        assert!(parse(b"{} x", false).is_ok());
        let s = parse(b"\"\\ud83d\\ude00\\u00e9\"", true).ok().unwrap();
        assert_eq!(s, Json::str("\u{1f600}\u{e9}"));
    }

    #[test]
    fn field_folds_case() {
        let v = parse(br#"{"Status":"x","reason":"r"}"#, true).ok().unwrap();
        assert_eq!(v.fstr("status"), "x");
        assert_eq!(v.fstr("Reason"), "r");
        assert!(v.get("status").is_none());
    }
}
