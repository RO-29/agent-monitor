#!/usr/bin/env python3
"""Fails when a stylesheet change adds a CSS class collision that is not in web/css-baseline.txt.

dup:  one selector (in the same @media context) defined one more time (the .wkids and .wseg breakages).
leak: a class with a global `.x` or `.v3 .x` rule that is also styled under other ancestors (the .navi breakage).
Usage: bin/css-collisions.py [styles-dir] [--update]
"""
import collections, glob, os, re, sys

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
args = [a for a in sys.argv[1:] if not a.startswith("--")]
styles = args[0] if args else os.path.join(root, "web/src")
base_path = os.path.join(root, "web/css-baseline.txt")


def strip_noise(text):
    text = re.sub(r"/\*.*?\*/", lambda m: "\n" * m.group(0).count("\n"), text, flags=re.S)
    return re.sub(r'"(?:[^"\\\n]|\\.)*"|\'(?:[^\'\\\n]|\\.)*\'', '""', text)


def rules(text):
    """Yields (at-rule context, selector, line) for every style rule, nested ones included."""
    out, stack, buf, line, start = [], [], "", 1, 1
    for ch in strip_noise(text):
        if ch == "\n":
            line += 1
        if ch == "{":
            sel = " ".join(buf.split())
            if not sel.startswith("@"):
                ctx = " ".join(x for x in stack if x.startswith("@"))
                parent = next((x for x in reversed(stack) if not x.startswith("@")), "")
                if parent and "&" in sel:
                    sel = sel.replace("&", parent)
                elif parent:
                    sel = parent + " " + sel
                out.append((ctx, sel, start))
            stack.append(sel)
            buf = ""
        elif ch == "}":
            if stack:
                stack.pop()
            buf = ""
        elif ch == ";":
            buf = ""
        else:
            if not buf.strip():
                start = line
            buf += ch
    return out


def split_top(sel):
    parts, depth, cur = [], 0, ""
    for ch in sel:
        depth += ch in "([" and 1 or (ch in ")]" and -1 or 0)
        if ch == "," and depth == 0:
            parts.append(cur)
            cur = ""
        else:
            cur += ch
    return [" ".join(p.split()) for p in parts + [cur] if p.strip()]


where = collections.defaultdict(list)
for f in sorted(glob.glob(os.path.join(styles, "**/*.css"), recursive=True)):
    for ctx, sel, ln in rules(open(f).read()):
        for one in split_top(sel):
            where[(ctx, one)].append(f"{os.path.relpath(f, styles)}:{ln}")

found = {}
for (ctx, sel), locs in where.items():
    if len(locs) > 1:
        found[f"dup: {ctx + ' | ' if ctx else ''}{sel}"] = locs

# A global class rule: `.x` or `.v3 .x` alone. A context rule styles that class under other ancestors.
glob_cls = collections.defaultdict(list)
for (ctx, sel), locs in where.items():
    if m := re.fullmatch(r"(?:\.v3 )?\.([a-zA-Z][\w-]*)", sel):
        glob_cls[m.group(1)] += locs
for (ctx, sel), locs in where.items():
    tokens = [t for t in re.split(r"\s*[\s>+~]\s*", sel) if t]
    if len(tokens) < 2:
        continue
    ancestors = [t for t in tokens[:-1] if t != ".v3"]
    if not ancestors:
        continue
    for c in set(re.findall(r"\.([a-zA-Z][\w-]*)", tokens[-1])):
        if c in glob_cls:
            found.setdefault(f"leak: .{c}", list(glob_cls[c])).extend(locs)

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
fell = sorted(k for k, n in known.items() if count.get(k, 0) < n)
if fell and not new:
    print(f"note: {len(fell)} baseline entries fell; run bin/css-collisions.py --update so a later regression fails")
if new:
    print(f"{len(new)} new CSS collision(s). Rename the class, or run bin/css-collisions.py --update if the override is meant.")
    sys.exit(1)
print(f"css collisions: ok ({len(found)} known)")
