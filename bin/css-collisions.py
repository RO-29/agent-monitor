#!/usr/bin/env python3
"""Fails when a stylesheet change adds a CSS class collision that is not in web/css-baseline.txt.

dup:  one selector defined twice at the top level (the .wkids and .wseg breakages).
leak: a class with a global `.v3 .x` rule that is also styled inside a context (the .navi breakage).
Usage: bin/css-collisions.py [styles-dir] [--update]
"""
import collections, glob, os, re, sys

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
args = [a for a in sys.argv[1:] if not a.startswith("--")]
styles = args[0] if args else os.path.join(root, "web/src/v3/styles")
base_path = os.path.join(root, "web/css-baseline.txt")


def top_rules(text):
    out, stack, buf, line, start = [], [], "", 1, 1
    for ch in re.sub(r"/\*.*?\*/", lambda m: "\n" * m.group(0).count("\n"), text, flags=re.S):
        if ch == "\n":
            line += 1
        if ch == "{":
            sel = buf.strip()
            if not sel.startswith("@") and not any(s.startswith("@") for s in stack):
                out.append((sel, start))
            stack.append(sel)
            buf = ""
        elif ch == "}":
            if stack:
                stack.pop()
            buf = ""
        elif ch == ";" and not stack:
            buf = ""
        else:
            if not buf.strip():
                start = line
            buf += ch
    return out


where = collections.defaultdict(list)
for f in sorted(glob.glob(os.path.join(styles, "**/*.css"), recursive=True)):
    for sel, ln in top_rules(open(f).read()):
        for s in sel.split(","):
            where[" ".join(s.split())].append(f"{os.path.basename(f)}:{ln}")

found = {f"dup: {s}": locs for s, locs in where.items() if len(locs) > 1}
glob_cls = {m.group(1): s for s in where if (m := re.fullmatch(r"\.v3 \.([a-zA-Z][\w-]*)", s))}
for s, locs in where.items():
    m = re.search(r"\s\.([a-zA-Z][\w-]*)$", s)
    if m and len(s.split()) > 2 and m.group(1) in glob_cls:
        found.setdefault(f"leak: .{m.group(1)}", []).extend(where[glob_cls[m.group(1)]] + locs)

# A count per entry: a third rule for an already doubled selector is a new collision too.
count = {k: len(set(v)) for k, v in found.items()}
if "--update" in sys.argv:
    with open(base_path, "w") as fh:
        fh.write("# Known CSS collisions with their rule count; bin/css-collisions.py fails when one is new or grows.\n")
        fh.write("".join(f"{k} x{count[k]}\n" for k in sorted(found)))
    print(f"wrote {len(found)} entries to {os.path.relpath(base_path, root)}")
    sys.exit(0)

known = {}
if os.path.exists(base_path):
    for l in open(base_path):
        if l.strip() and not l.startswith("#"):
            k, _, n = l.strip().rpartition(" x")
            known[k] = int(n)
new = sorted(k for k in found if count[k] > known.get(k, 1 if k in known else 0))
for k in new:
    was = f", was {known[k]}" if k in known else ""
    print(f"css collision: {k}  ({count[k]} rules{was}: {', '.join(sorted(set(found[k])))})")
if new:
    print(f"{len(new)} new CSS collision(s). Rename the class, or run bin/css-collisions.py --update if the override is meant.")
    sys.exit(1)
print(f"css collisions: ok ({len(found)} known)")
