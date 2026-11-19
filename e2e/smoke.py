#!/usr/bin/env python3
# Smoke test of the running daemon, safe on the live install: routes, three tiny uploads, one scratch haiku session.
# Usage: python3 e2e/smoke.py [--no-agent]   (--no-agent uses no tokens); exit code 0 when every check passes.
import json, os, subprocess, sys, tempfile, time, urllib.error, urllib.request, uuid

D = os.environ.get("AM_URL", "http://127.0.0.1:7777")
CWD = os.environ.get("AM_SMOKE_CWD", os.path.expanduser("~/agent-monitor-v3"))  # a folder Claude already trusts
fails = []


def call(method, path, body=None, raw=None, ctype="application/json"):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(D + path, method=method, data=data, headers={"Content-Type": ctype})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def js(b):
    try:
        return json.loads(b or b"{}")
    except ValueError:
        return {}


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""))
    if not ok:
        fails.append(name)


def upload(name, data, ctype):
    b = uuid.uuid4().hex
    body = (f"--{b}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\nContent-Type: {ctype}\r\n\r\n").encode() + data + f"\r\n--{b}--\r\n".encode()
    return call("POST", "/api/orch/uploads", raw=body, ctype=f"multipart/form-data; boundary={b}")


def routes():
    c, h, _ = call("GET", "/api/no-such-route")
    check("unknown /api path is a JSON 404", c == 404 and "json" in h.get("Content-Type", ""))
    c, h, _ = call("GET", "/session/abc")
    check("old deep link serves the app", c == 200 and "html" in h.get("Content-Type", ""))
    c, _, b = call("GET", "/api/needs-you")
    check("needs-you answers", c == 200 and "items" in js(b))


def uploads():
    c, _, b = upload("page.html", b"<!doctype html><script>alert(1)</script>", "text/html")
    up = js(b).get("upload") or js(b)
    check("html upload kept as text", c == 201 and up.get("mime") == "text/plain" and up.get("path", "").endswith(".txt"), b[:120])
    if c == 201:
        c2, h, _ = call("GET", up["url"])
        check("upload served inert", c2 == 200 and h.get("Content-Type", "").startswith("text/plain") and h.get("X-Content-Type-Options") == "nosniff" and "sandbox" in h.get("Content-Security-Policy", ""))
    c, _, _ = upload("main.go", b"package main\n", "text/x-go")
    check("source file accepted", c == 201)
    c, _, _ = upload("bin.go", b"a\x00b", "text/x-go")
    check("binary refused", c == 415)


def tmux_has(s):
    return subprocess.run(["tmux", "has-session", "-t", s], capture_output=True).returncode == 0


def screen(s):
    return subprocess.run(["tmux", "capture-pane", "-p", "-t", s], capture_output=True, text=True).stdout


def wait_for(s, word, secs=90):
    for _ in range(secs // 2):
        if f"⏺ {word}" in screen(s).lower():
            return True
        time.sleep(2)
    return False


def session_state(sid):
    _, _, b = call("GET", "/api/sessions?slim=1")
    d = js(b)
    for s in d if isinstance(d, list) else d.get("sessions", []):
        if s.get("id") == sid:
            return s.get("state")
    return None


def agent():
    c, _, b = call("POST", "/api/orch/terminal", {"agent": "claude", "cwd": CWD, "model": "haiku", "prompt": "Reply with the word alpha only."})
    t = js(b)
    check("terminal start", c == 200 and "session" in t, b[:160])
    if c != 200:
        return
    tmux = t["session"]
    try:
        check("first prompt reaches Claude", wait_for(tmux, "alpha"), screen(tmux)[-300:])
        _, _, hb = call("GET", "/api/hosts")
        sid = next((h["sessionId"] for h in js(hb).get("hosts", []) if h["hostId"] == t["hostId"]), "")
        check("host linked to its session", bool(sid))
        if not sid:
            return
        aid = "claude:" + sid
        long = "\n".join(["LONG-START count the file lines below and reply with the number only.", ""] +
                          [f"- /tmp/smoke/file-{i:02d}.png (image/png, {300 + i} KB)" for i in range(12)] + ["LONG-END"])
        c, _, _ = call("POST", f"/api/session/{aid}/cli-queue/send", {"text": long})
        check("long multi-line message sent", c == 200)
        check("long message arrives whole", wait_for(tmux, "12", 60), screen(tmux)[-300:])
        time.sleep(2)
        c, _, b = call("POST", f"/api/session/end/{aid}")
        check("end session", c == 200 and js(b).get("ended") is True, b[:160])
        time.sleep(3)
        check("ended session stays completed", session_state(aid) == "completed", session_state(aid))
        c, _, b = call("POST", f"/api/session/resume/{aid}", {"prompt": "Reply with the word beta only."})
        r = js(b).get("terminal", {})
        check("resume in terminal", c == 201 and r.get("session"), b[:160])
        if c == 201:
            tmux = r["session"]
            check("resumed session answers", wait_for(tmux, "beta"), screen(tmux)[-300:])
            c, _, _ = call("POST", f"/api/session/resume/{aid}")
            check("resume refused while live", c == 409)
            c, _, b = call("POST", f"/api/session/end/{aid}")
            check("end resumed session", c == 200 and js(b).get("ended") is True, b[:160])
    finally:
        if tmux_has(tmux):
            subprocess.run(["tmux", "kill-session", "-t", tmux])


if __name__ == "__main__":
    routes()
    uploads()
    if "--no-agent" not in sys.argv:
        agent()
    print(f"\n{len(fails)} failed" if fails else "\nall passed")
    sys.exit(1 if fails else 0)
