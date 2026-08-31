// ANSI SGR → HTML spans. Only colour/weight codes: tmux capture-pane already
// flattened cursor movement. Classes a-fg-N / a-bg-N / a-b / a-d / a-u are in app.css.
const esc = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

export function ansiToHtml(input: string): string {
  // strip OSC sequences (ESC ] … BEL | ESC \)
  let s = input.replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "");
  let out = "";
  let fg = "";
  let bg = "";
  let bold = false;
  let dim = false;
  let ul = false;
  let open = false;
  const flush = () => {
    if (open) out += "</span>";
    open = false;
  };
  const start = () => {
    const cls = [fg, bg, bold ? "a-b" : "", dim ? "a-d" : "", ul ? "a-u" : ""].filter(Boolean).join(" ");
    if (cls) {
      out += `<span class="${cls}">`;
      open = true;
    }
  };
  const re = /\x1b\[([0-9;]*)m/g;
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s))) {
    out += esc(s.slice(last, m.index));
    last = m.index + m[0].length;
    flush();
    const codes = (m[1] || "0").split(";").map((x) => parseInt(x || "0", 10));
    for (let i = 0; i < codes.length; i++) {
      const c = codes[i];
      if (c === 0) {
        fg = bg = "";
        bold = dim = ul = false;
      } else if (c === 1) bold = true;
      else if (c === 2) dim = true;
      else if (c === 4) ul = true;
      else if (c === 22) bold = dim = false;
      else if (c === 24) ul = false;
      else if (c === 39) fg = "";
      else if (c === 49) bg = "";
      else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) fg = `a-fg-${c}`;
      else if ((c >= 40 && c <= 47) || (c >= 100 && c <= 107)) bg = `a-bg-${c}`;
      else if (c === 38 || c === 48) {
        const mode = codes[i + 1];
        if (mode === 5) {
          const n = codes[i + 2];
          if (c === 38) fg = `a-fg-x${n}`;
          else bg = `a-bg-x${n}`;
          i += 2;
        } else if (mode === 2) i += 4; // truecolor: skipped
      }
    }
    start();
  }
  out += esc(s.slice(last));
  flush();
  return out;
}
