// Right panel phase 4: Git box (commit, push, PR, open), changes from git, checkpoints with diff and restore.
import { useEffect, useMemo, useRef, useState } from "react";
import { showToast } from "../../../app/toast";
import { fmtAgo } from "../../../lib/format";
import { ApiError } from "../../api/client";
import { gitApi, rememberThread, toMs, type OCheckpoint, type ODetail, type OGit, type OOpenTarget } from "../../api/orch";
import type { V3Thread } from "../../api/types";
import { statusInfo } from "./Diff";
import { FoldBox } from "../../components/fold";
import { p7, type OMergeRefusal, type OMergeStrategy } from "../../api/p7";
import { commitSubject, gitErrorToast, prBody, isLoopback, pushBlocked, refreshGit, runActive, shortSha, useGit, useGitPoll, type DiffNav, type GitEntry } from "./git";

type Form = null | "commit" | "pr" | "pushfirst" | "rmwt" | "merge";

export const OPEN_LABEL: Record<OOpenTarget, string> = { editor: "Editor", terminal: "Terminal", finder: "Folder" };

export async function openIn(id: string, target: OOpenTarget) {
  try {
    await gitApi().open(id, target);
    showToast(`Opened in ${OPEN_LABEL[target].toLowerCase()}`);
  } catch (e) {
    gitErrorToast(`Could not open the ${OPEN_LABEL[target].toLowerCase()}`, e);
  }
}

function OpenMenu({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const local = isLoopback();
  useEffect(() => {
    if (!open) return;
    const off = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", off);
    return () => document.removeEventListener("mousedown", off);
  }, [open]);
  return (
    <div className="wopen" ref={ref}>
      <button className="btn sm" onClick={() => setOpen(!open)} aria-haspopup="menu" aria-expanded={open} disabled={!local} title={local ? "Open the folder on this Mac" : "Open works only on the Mac itself"}>
        Open ▾
      </button>
      {open && (
        <div className="wopen-m" role="menu">
          {(["editor", "terminal", "finder"] as OOpenTarget[]).map((t) => (
            <button
              key={t}
              role="menuitem"
              onClick={() => {
                setOpen(false);
                void openIn(id, t);
              }}
            >
              {OPEN_LABEL[t]}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function aheadText(g: OGit) {
  if (!g.upstream) return "";
  const p = [g.ahead ? `↑${g.ahead}` : "", g.behind ? `↓${g.behind}` : ""].filter(Boolean).join(" ");
  return p || "up to date";
}

/** Only an https: link opens from a daemon answer. */
const httpsUrl = (u?: string) => {
  try {
    return !!u && new URL(u).protocol === "https:";
  } catch {
    return false;
  }
};

export function GitBox({ detail, git }: { detail: ODetail; git: GitEntry }) {
  const t = detail.thread;
  const g = git.data;
  const active = runActive(detail);
  const [form, setForm] = useState<Form>(null);
  const [msg, setMsg] = useState("");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [draft, setDraft] = useState(true);
  const [busy, setBusy] = useState<string>("");
  useGitPoll(t.id, !t.worktree?.removed && git.status !== "missing" && git.status !== "nogit");

  if (t.worktree?.removed)
    return (
      <FoldBox id="git" title="Git">
        <div className="wkv">
          <span className="k">Branch</span>
          <span className="v mono">{t.worktree.branch}</span>
        </div>
        <div className="muted sm">The worktree was removed. The branch stays, so you can check it out again.</div>
      </FoldBox>
    );
  if (git.status === "loading" && !g)
    return (
      <FoldBox id="git" title="Git">
        <div className="muted sm">Reading the repository…</div>
      </FoldBox>
    );
  if (git.status === "missing")
    return (
      <FoldBox id="git" title="Git">
        <div className="muted sm">This daemon has no git routes yet. Update agent-monitor for commit, push and checkpoints.</div>
      </FoldBox>
    );
  if (git.status === "nogit")
    return (
      <FoldBox id="git" title="Git">
        <div className="muted sm">{t.worktree?.removed ? `The worktree was removed. The branch ${t.worktree.branch} stays.` : "Not a git repo."}</div>
      </FoldBox>
    );
  if (!g)
    return (
      <FoldBox id="git" title="Git">
        <div className="werr sm">{git.error || "Could not read the repository."}</div>
        <button className="btn sm" style={{ marginTop: 8 }} onClick={() => refreshGit(t.id)}>
          Try again
        </button>
      </FoldBox>
    );

  const blocked = pushBlocked(g);
  const commitWhy = active ? "A run is active. Commit when it ends." : g.dirty === 0 ? "Nothing to commit." : "";
  const pushWhy = blocked ? `Push is off on ${g.branch}. Work on a branch to push.` : g.upstream && g.ahead === 0 ? "Nothing to push." : "";
  const run = async (name: string, fn: () => Promise<void>) => {
    if (busy) return;
    setBusy(name);
    try {
      await fn();
    } finally {
      setBusy("");
      refreshGit(t.id);
    }
  };
  const openForm = (f: Form) => {
    if (f === form) return setForm(null);
    if (f === "commit") setMsg(commitSubject(detail));
    if (f === "pr") {
      setTitle(t.title);
      setBody(prBody(detail));
      setDraft(true);
    }
    setForm(f);
  };
  const commit = () =>
    run("commit", async () => {
      const m = msg.trim();
      if (!m || active) return;
      try {
        const r = await gitApi().commit(t.id, m);
        showToast("Committed", `${shortSha(r.sha)} ${m.split("\n")[0]}`);
        setForm(null);
      } catch (e) {
        gitErrorToast(e instanceof ApiError && e.status === 422 ? "A commit hook failed" : "Commit failed", e);
      }
    });
  const push = () =>
    run("push", async () => {
      try {
        const r = await gitApi().push(t.id);
        showToast("Pushed", `${g.branch} to origin`, { output: r.output });
        if (form === "pushfirst") setForm("pr");
      } catch (e) {
        gitErrorToast("Push failed", e);
      }
    });
  const pr = () =>
    run("pr", async () => {
      if (!title.trim()) return;
      try {
        const r = await gitApi().pr(t.id, { title: title.trim(), body: body.trim() || undefined, draft });
        showToast(draft ? "Draft pull request created" : "Pull request created", r.url ? `${r.url} (click to open)` : undefined, { onClick: httpsUrl(r.url) ? () => window.open(r.url, "_blank", "noopener") : undefined, ms: 15000 });
        setForm(null);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) setForm("pushfirst");
        else gitErrorToast("Could not create the pull request", e);
      }
    });
  const rmWorktree = (force: boolean) =>
    run("rmwt", async () => {
      try {
        const r = await gitApi().removeWorktree(t.id, force);
        if (r.thread?.id === t.id) rememberThread(r.thread);
        showToast("Worktree removed", "The branch stays.");
        setForm(null);
      } catch (e) {
        if (e instanceof ApiError && e.status === 409 && !force && (e.body as { output?: string } | undefined)?.output) gitErrorToast("The worktree has changes. Commit them first, or remove it with the changes.", e);
        else gitErrorToast("Could not remove the worktree", e);
      }
    });

  const wt = t.worktree || g.worktree;
  return (
    <FoldBox
      id="git"
      title="Git"
      className="wgit"
      summary={`${g.branch || shortSha(g.head)}${g.dirty ? ` · ${g.dirty} uncommitted` : ""}`}
      right={
        git.status === "error" && (
          <span className="r err-t" title={git.error}>
            stale
          </span>
        )
      }
    >
      <div className="wkv">
        <span className="k">Branch</span>
        <span className="v mono" title={g.branch}>
          {g.branch || <span className="muted">detached at {shortSha(g.head)}</span>}
        </span>
      </div>
      <div className="wkv">
        <span className="k">Upstream</span>
        <span className={`v ${g.upstream ? "mono" : ""}`} title={g.upstream}>
          {g.upstream ? (
            <>
              <span className={g.ahead || g.behind ? "need-t" : "muted"}>{aheadText(g)}</span> {g.upstream}
            </>
          ) : (
            <span className="muted">not pushed</span>
          )}
        </span>
      </div>
      <div className="wkv">
        <span className="k">Changes</span>
        <span className="v">{g.dirty ? <span className="need-t">{g.dirty} uncommitted {g.dirty === 1 ? "file" : "files"}</span> : <span className="muted">clean</span>}</span>
      </div>
      <div className="wgit-btns">
        <button className={`btn sm ${form === "commit" ? "on" : ""}`} disabled={!!commitWhy} title={commitWhy || "Commit every change"} onClick={() => openForm("commit")}>
          Commit
        </button>
        <button className="btn sm" disabled={!!pushWhy || !!busy} title={pushWhy || `git push -u origin ${g.branch}`} onClick={push}>
          {busy === "push" ? "Pushing…" : "Push"}
        </button>
        <button className={`btn sm ${form === "pr" || form === "pushfirst" ? "on" : ""}`} disabled={blocked} title={blocked ? `No pull request from ${g.branch}` : "gh pr create"} onClick={() => openForm(g.upstream ? "pr" : "pushfirst")}>
          Create PR
        </button>
        {wt && (
          <button className={`btn sm ${form === "merge" ? "on" : ""}`} disabled={active} title={active ? "A run is active. Merge when it ends." : "Merge this worktree's branch back. Never pushes."} onClick={() => openForm("merge")}>
            Merge
          </button>
        )}
        <OpenMenu id={t.id} />
      </div>
      {(blocked || commitWhy) && !form && <div className="wgit-why">{blocked ? pushWhy : commitWhy}</div>}
      {!isLoopback() && <div className="wgit-why">Open works only in a browser on the Mac itself (127.0.0.1).</div>}

      {form === "commit" && (
        <div className="wgit-form">
          <textarea
            className="inp"
            rows={3}
            autoFocus
            value={msg}
            placeholder="Commit message"
            onChange={(e) => setMsg(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                if (msg.trim() && !busy && !active) void commit();
              } else if (e.key === "Escape") setForm(null);
            }}
          />
          <div className="wgit-note">Adds all {g.dirty} changed files. Your git config and hooks apply.</div>
          <div className="wgit-fa">
            <button className="btn sm ghost" onClick={() => setForm(null)}>
              Cancel
            </button>
            <button className="btn sm pri" disabled={!msg.trim() || !!busy || active} onClick={commit}>
              {busy === "commit" ? "Committing…" : "Commit"}
            </button>
          </div>
        </div>
      )}
      {form === "pushfirst" && (
        <div className="wgit-form">
          <div className="wgit-note">
            <b>Push first.</b> The branch has no upstream yet, so GitHub cannot open a pull request from it.
          </div>
          <div className="wgit-fa">
            <button className="btn sm ghost" onClick={() => setForm(null)}>
              Cancel
            </button>
            <button className="btn sm pri" disabled={!!pushWhy || !!busy} onClick={push}>
              {busy === "push" ? "Pushing…" : "Push"}
            </button>
          </div>
        </div>
      )}
      {form === "pr" && (
        <div className="wgit-form">
          <input className="inp" value={title} autoFocus placeholder="Title" aria-label="Pull request title" onChange={(e) => setTitle(e.target.value)} />
          <textarea className="inp" rows={8} value={body} placeholder="Description (optional)" aria-label="Pull request description" onChange={(e) => setBody(e.target.value)} />
          <label className="wgit-chk">
            <input type="checkbox" checked={draft} onChange={(e) => setDraft(e.target.checked)} /> Draft
          </label>
          <div className="wgit-fa">
            <span className="wgit-note">
              Into {g.defaultBranch || "the default branch"}
            </span>
            <span className="sp" />
            <button className="btn sm ghost" onClick={() => setForm(null)}>
              Cancel
            </button>
            <button className="btn sm pri" disabled={!title.trim() || !!busy} onClick={pr}>
              {busy === "pr" ? "Creating…" : "Create PR"}
            </button>
          </div>
        </div>
      )}
      {form === "merge" && wt && <MergeForm detail={detail} g={g} onClose={() => setForm(null)} />}
      {wt && form !== "rmwt" && (
        <button className="wlink sm" onClick={() => setForm("rmwt")}>
          Remove worktree…
        </button>
      )}
      {form === "rmwt" && (
        <div className="wgit-form">
          <div className="wgit-note">Removes the folder {wt?.path}. The branch stays.</div>
          <div className="wgit-fa">
            <button className="btn sm ghost" onClick={() => setForm(null)}>
              Cancel
            </button>
            {g.dirty > 0 && (
              <button className="btn sm" disabled={!!busy || active} onClick={() => rmWorktree(true)}>
                Remove with changes
              </button>
            )}
            <button className="btn sm pri" disabled={!!busy || active || g.dirty > 0} title={g.dirty > 0 ? "The worktree has uncommitted changes" : ""} onClick={() => rmWorktree(false)}>
              Remove
            </button>
          </div>
        </div>
      )}
    </FoldBox>
  );
}

export function GitChanges({ git, onDiff }: { git: GitEntry; onDiff: (d: DiffNav) => void }) {
  const files = git.data?.files || [];
  const add = files.reduce((n, f) => n + Math.max(0, f.add || 0), 0);
  const del = files.reduce((n, f) => n + Math.max(0, f.del || 0), 0);
  if (!files.length) return null;
  return (
    <FoldBox
      id="changes"
      title="Changes"
      summary={`${files.length} ${files.length === 1 ? "file" : "files"} · +${add} −${del}`}
      right={
        <span className="r mono">
          <span className="add">+{add}</span> <span className="del">−{del}</span>
        </span>
      }
    >
      {files.length === 0 ? (
        <div className="muted sm">No uncommitted changes.</div>
      ) : (
        files.map((f) => {
          const st = statusInfo(f.status);
          return (
            <button key={f.path} className="f fbtn" onClick={() => onDiff({ from: "head", to: "worktree", file: f.path, label: "Uncommitted" })} title={`${st.name}: ${f.path}`}>
              <span className={`dst ${st.cls}`}>{st.l}</span>
              <span className="p">{f.path}</span>
              <span className="n">
                {f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}
              </span>
            </button>
          );
        })
      )}
    </FoldBox>
  );
}

interface Turn {
  runIndex: number;
  prompt: string;
  start?: OCheckpoint;
  end?: OCheckpoint;
  safety: OCheckpoint[];
  at: number;
}

export function Checkpoints({ detail, git, onDiff }: { detail: ODetail; git: GitEntry; onDiff: (d: DiffNav) => void }) {
  const t = detail.thread;
  const active = runActive(detail);
  const [confirm, setConfirm] = useState<string | null>(null);
  const [pick, setPick] = useState<string>("");
  const [busy, setBusy] = useState(false);
  const cps = git.data?.checkpoints || [];
  const turns = useMemo(() => {
    const m = new Map<number, Turn>();
    for (const c of [...cps].sort((a, b) => toMs(a.at) - toMs(b.at))) {
      const tr = m.get(c.runIndex) || { runIndex: c.runIndex, prompt: "", safety: [], at: 0 };
      if (c.kind === "start") tr.start = c;
      else if (c.kind === "end") tr.end = c;
      else tr.safety.push(c);
      tr.prompt = tr.prompt || c.prompt || detail.runs.find((r) => r.index === c.runIndex)?.prompt || "";
      tr.at = Math.max(tr.at, toMs(c.at));
      m.set(c.runIndex, tr);
    }
    return [...m.values()].sort((a, b) => b.runIndex - a.runIndex);
  }, [cps, detail.runs]);
  if ((git.status !== "ok" && !git.data) || !turns.length) return null;

  const restore = async (ref: string) => {
    if (busy) return;
    setBusy(true);
    try {
      const r = await gitApi().restore(t.id, ref);
      showToast("Files restored", r.safetyRef ? `Safety checkpoint: ${r.safetyRef.replace(/^refs\//, "")}` : undefined);
      setConfirm(null);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) showToast("A run is active", "Restore when the run ends.", { kind: "alert" });
      else gitErrorToast("Restore failed", e);
    } finally {
      setBusy(false);
      refreshGit(t.id);
    }
  };
  const firstLine = (s: string) => s.split("\n").map((l) => l.trim()).find(Boolean) || "";

  return (
    <FoldBox id="checkpoints" title="Checkpoints" summary={`${turns.length} ${turns.length === 1 ? "turn" : "turns"}`} right={cps.length > 0 && <span className="r muted sm">{cps.length}</span>}>
      {turns.length === 0 ? (
        <div className="muted sm">No checkpoints yet. One is taken at the start and the end of each turn.</div>
      ) : (
        turns.map((tr) => {
          const last = tr.end || tr.start;
          const prev = turns.find((x) => x.runIndex < tr.runIndex);
          const from = tr.start?.ref || prev?.end?.ref || prev?.start?.ref || "base";
          const to = tr.end?.ref || (detail.runs.find((r) => r.index === tr.runIndex)?.status === "running" ? "worktree" : "");
          const key = `t${tr.runIndex}`;
          const restorable = [tr.end && { ref: tr.end.ref, label: "After this turn" }, tr.start && { ref: tr.start.ref, label: "Before this turn" }, ...tr.safety.map((c) => ({ ref: c.ref, label: `Safety point ${shortSha(c.sha)}` }))].filter(Boolean) as { ref: string; label: string }[];
          return (
            <div key={key} className="wcp">
              <span className="n">T{tr.runIndex}</span>
              <div className="x">
                <span className="trunc" title={tr.prompt}>
                  {firstLine(tr.prompt) || `Turn ${tr.runIndex}`}
                </span>
                <small>
                  {last && (last.add || last.del) ? (
                    <>
                      <span className="add">+{last.add}</span> <span className="del">−{last.del}</span> · {last.files} {last.files === 1 ? "file" : "files"} ·{" "}
                    </>
                  ) : null}
                  {tr.start && tr.end ? "start, end" : tr.end ? "end" : tr.start ? "start" : "safety"}
                  {tr.at ? ` · ${fmtAgo(tr.at)}` : ""}
                </small>
              </div>
              <div className="a">
                {to && (
                  <button className="wlink" onClick={() => onDiff({ from, to, run: tr.runIndex, label: `Turn ${tr.runIndex}` })}>
                    Diff
                  </button>
                )}
                {restorable.length > 0 && (
                  <button
                    className="wlink"
                    disabled={active}
                    title={active ? "Restore is off while a run is active" : "Put the files back to this turn"}
                    onClick={() => {
                      setPick(restorable[0].ref);
                      setConfirm(confirm === key ? null : key);
                    }}
                  >
                    Restore
                  </button>
                )}
              </div>
              {confirm === key && (
                <div className="wcp-c">
                  <div>Files go back to this turn. A safety checkpoint is taken first.</div>
                  {restorable.length > 1 && (
                    <div className="wcp-pick" role="radiogroup" aria-label="Restore point">
                      {restorable.map((r) => (
                        <label key={r.ref}>
                          <input type="radio" name={`rp-${key}`} checked={pick === r.ref} onChange={() => setPick(r.ref)} /> {r.label}
                        </label>
                      ))}
                    </div>
                  )}
                  <div className="wgit-fa">
                    <button className="btn sm ghost" onClick={() => setConfirm(null)}>
                      Cancel
                    </button>
                    <button className="btn sm pri" disabled={busy || active} onClick={() => restore(pick || restorable[0].ref)}>
                      {busy ? "Restoring…" : "Restore files"}
                    </button>
                  </div>
                </div>
              )}
            </div>
          );
        })
      )}
      {active && turns.length > 0 && <div className="wgit-why">Restore is off while a run is active.</div>}
    </FoldBox>
  );
}

/** Watched sessions: GET /api/orch/git?cwd= (read only), plus commits and PRs the thread record saw. */
export function WatchedGitBox({ t, cwd, onDiff }: { t?: V3Thread; cwd: string; onDiff?: (d: DiffNav) => void }) {
  const git = useGit(`cwd:${cwd}`, "w");
  useGitPoll(`cwd:${cwd}`, git.status === "ok" || git.status === "loading");
  const g = git.status === "ok" ? git.data : undefined;
  const shipped = (t?.shipped || []).filter((s) => s.kind === "commit" || s.kind === "push" || s.kind === "pr" || s.kind === "merge");
  if (!g && !t?.ref && !shipped.length) return null;
  const files = g?.files || [];
  return (
    <FoldBox id="git" title="Git" summary={`${g?.branch || t?.ref || ""}${g?.dirty ? ` · ${g.dirty} uncommitted` : ""}`}>
      {(g?.branch || t?.ref) && (
        <div className="wkv">
          <span className="k">Branch</span>
          <span className="v mono" title={g?.branch || t?.ref}>
            {g?.branch || t?.ref}
          </span>
        </div>
      )}
      {g && (
        <>
          <div className="wkv">
            <span className="k">Upstream</span>
            <span className={`v ${g.upstream ? "mono" : ""}`} title={g.upstream}>
              {g.upstream ? (
                <>
                  <span className={g.ahead || g.behind ? "need-t" : "muted"}>{aheadText(g)}</span> {g.upstream}
                </>
              ) : (
                <span className="muted">not pushed</span>
              )}
            </span>
          </div>
          <div className="wkv">
            <span className="k">Changes</span>
            <span className="v">{g.dirty ? <span className="need-t">{g.dirty} uncommitted {g.dirty === 1 ? "file" : "files"}</span> : <span className="muted">clean</span>}</span>
          </div>
          {files.slice(0, 8).map((f) => {
            const st = statusInfo(f.status);
            const inner = (
              <>
                <span className={`dst ${st.cls}`}>{st.l}</span>
                <span className="p">{f.path}</span>
                <span className="n">
                  {f.add > 0 && <span className="add">+{f.add}</span>} {f.del > 0 && <span className="del">−{f.del}</span>}
                </span>
              </>
            );
            return onDiff ? (
              <button key={f.path} className="f fbtn" title={`${st.name}: ${f.path}. Show the diff`} onClick={() => onDiff({ from: "head", to: "worktree", file: f.path, label: "Uncommitted" })}>
                {inner}
              </button>
            ) : (
              <div key={f.path} className="f" title={`${st.name}: ${f.path}`}>
                {inner}
              </div>
            );
          })}
          {files.length > 8 && <div className="muted sm">and {files.length - 8} more</div>}
          {onDiff && g.dirty > 0 && (
            <div className="wgit-a">
              <button className="btn sm" onClick={() => onDiff({ from: "head", to: "worktree", label: "Uncommitted" })}>
                Diff
              </button>
            </div>
          )}
        </>
      )}
      {shipped.slice(-5).reverse().map((s, i) => (
        <div key={i} className="f">
          <span className="dst m">{s.kind === "commit" ? "C" : s.kind === "pr" ? "PR" : s.kind === "merge" ? "MG" : "P"}</span>
          <span className="p" title={s.label}>
            {s.label}
          </span>
          <span className="n muted">{fmtAgo(s.ts)}</span>
        </div>
      ))}
    </FoldBox>
  );
}

function MergeForm({ detail, g, onClose }: { detail: ODetail; g: OGit; onClose: () => void }) {
  const t = detail.thread;
  const [strategy, setStrategy] = useState<OMergeStrategy>("squash");
  const [into, setInto] = useState("");
  const [msg, setMsg] = useState(() => commitSubject(detail) || t.title);
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState<OMergeRefusal | null>(null);
  const active = runActive(detail);
  const target = into.trim() || "the branch it came from";
  const dirty = g.dirty > 0;
  const go = async () => {
    setBusy(true);
    setRefused(null);
    try {
      const r = await p7.merge(t.id, { into: into.trim() || undefined, strategy, message: msg.trim() || undefined });
      showToast(`Merged into ${r.into || into.trim() || "the parent branch"}`, [r.sha && shortSha(r.sha), strategy === "squash" ? "one squashed commit" : "merge commit", "not pushed"].filter(Boolean).join(" · "), { output: r.output });
      onClose();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        const b = (e.body || {}) as Partial<OMergeRefusal>;
        setRefused({ error: b.error || e.message, reason: b.reason, conflicts: b.conflicts || b.files, output: b.output });
      } else gitErrorToast("Merge failed", e);
      setConfirm(false);
    } finally {
      setBusy(false);
      refreshGit(t.id);
    }
  };
  const conflicts = refused?.conflicts || [];
  return (
    <div className="wgit-form wmerge">
      <div className="wmerge-r">
        <label>
          <span>Strategy</span>
          <select className="inp" value={strategy} onChange={(e) => setStrategy(e.target.value as OMergeStrategy)}>
            <option value="squash">Squash into one commit</option>
            <option value="merge">Merge commit</option>
          </select>
        </label>
        <label>
          <span>Into</span>
          <input className="inp mono" value={into} placeholder={t.worktree?.origin ? "the origin checkout's branch" : "the branch it came from"} aria-label="Target branch" onChange={(e) => setInto(e.target.value)} />
        </label>
      </div>
      <textarea className="inp" rows={2} value={msg} placeholder="Commit message" aria-label="Merge commit message" onChange={(e) => setMsg(e.target.value)} />
      {dirty && <div className="wgit-why">The worktree has {g.dirty} uncommitted {g.dirty === 1 ? "file" : "files"}. Commit them first.</div>}
      {refused && (
        <div className="wmerge-x" role="alert">
          <b>{conflicts.length ? `${conflicts.length} ${conflicts.length === 1 ? "file conflicts" : "files conflict"}. Nothing changed.` : refused.reason === "dirty-target" ? "The target checkout has uncommitted changes." : refused.error}</b>
          {conflicts.length > 0 && (
            <ul>
              {conflicts.map((f) => (
                <li key={f} className="mono">
                  {f}
                </li>
              ))}
            </ul>
          )}
          <span className="muted">{conflicts.length ? "Ask the agent to merge the target into its branch and fix them, then merge again." : refused.reason === "dirty-target" ? "Commit or stash them there, then merge again." : ""}</span>
        </div>
      )}
      {confirm ? (
        <div className="wgit-fa">
          <span className="wgit-note">
            {strategy === "squash" ? "Squash" : "Merge"} {g.branch} into {target}? Nothing is pushed.
          </span>
          <span className="sp" />
          <button className="btn sm ghost" onClick={() => setConfirm(false)}>
            Back
          </button>
          <button className="btn sm pri" disabled={busy || active} onClick={go}>
            {busy ? "Merging…" : "Merge now"}
          </button>
        </div>
      ) : (
        <div className="wgit-fa">
          <span className="wgit-note">Local only. A conflict leaves the target as it is.</span>
          <span className="sp" />
          <button className="btn sm ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn sm pri" disabled={dirty || active || (strategy === "squash" && !msg.trim())} onClick={() => setConfirm(true)}>
            Merge…
          </button>
        </div>
      )}
    </div>
  );
}
