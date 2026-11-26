import { ScrollJump, useKeepPlace } from "./ScrollJump";
import { FillCtx } from "../../components/Markdown";
import { CostBadge } from "../../components/SpendBits";
import { insertFor } from "../../../lib/insert";
import { drivenNeedIds, useNeedsYou } from "../../api/needsYou";
// Chat for a driven (orchestrator) thread: items grouped per run, inline approvals, composer.
import { DetailsBtn, ListShowBtn, TopBarItem, TopReveal } from "./barToggles";
import { splitFileNote } from "../../../lib/fileNote";
import { clearSent, useDraft } from "../../../lib/draft";
import { useDocTitle } from "../../../lib/title";
import { usd } from "../../components/Spend";
import { createContext, Fragment, useContext, useEffect, useMemo, useRef, useState } from "react";
import { NewLine, PendingBubbles, sinceText, useNewMark, usePendingSends } from "./sendMarks";
import { Link, useNavigate } from "react-router-dom";
import { showToast } from "../../../app/toast";
import { fmtDuration, fmtUsd, shortCwd } from "../../../lib/format";
import { ApiError } from "../../api/client";
import { EditTitle, PaneCtx, startRename } from "../../components/EditTitle";
import { copyWithToast } from "../../components/CopyBtn";
import { agentCls, agentLabel, agentVar, allowOnce, closeThread, continueInParent, interruptThread, pinThread, renameThread, sendMessage, settleThread, spendBlockOf, toMs, useOrch, type ODetail, type OItem, type ORun, type OThread, type SpendBlock } from "../../api/orch";
import { Markdown } from "../../components/Markdown";
import { Boundary } from "../../components/Boundary";
import { agentName, useV3 } from "../../live";
import { childMap, cmdLabel, descendants, drivenLabel, drivenThreads, permModeLabel, WORKSPACE_LABEL, workspaceOf } from "./rows";
import { askFromApproval, askFromRequest, AskAnnouncer, WsApproval, type AskInfo } from "./WsApproval";
import { DiffView } from "./Diff";
import { StepTrail, TrailBar, trailSegments } from "./StepTrail";
import { gitKey, isLoopback, runDiff, useDiffNav, useGit, type DiffNav } from "./git";
import { openIn } from "./GitBox";
import { drivenTerm, TermDock, toggleTerm, useTermOpen } from "./TermDock";
import { Composer, extOf, useAttachments, type PickCommand } from "./Composer";
import { IMAGE_TYPES, PLAYABLE } from "../../api/p7";
import { LinkedChip, LinkedPage, type LinkedEntry } from "./linked";
import { NewThreadBtn } from "./NewThread";
import { queueApi, useQueue } from "../../api/queue";
import { QueueList, type QueueListHandle } from "./QueueList";
import { ReviewCard, ReviewStart } from "./Review";
import { HitCtx, useHitFocus, type HitReq } from "./hit";
import { useLive } from "../../../lib/ws";
import { useWatchedFeed } from "./watched";
import { CmdCopy, MsgCopy, OutCopy, ResumeChip, SelectBar, SelectBox, SelectCtx, useSelectCtx, useSelection } from "./MsgActions";
import { handoverOf, serverDrivenRow, terminalTarget } from "./handover";
import { ChildChips } from "./children";
import { AgentIcon, agentKey } from "../../components/AgentIcon";

export const isReviewItem = (it: OItem) => it.kind === "review" || (it.kind === "other" && it.command === "review");

/** The chat's agent, for the icon on its answers. */
export const ChatAgentCtx = createContext("");

const clockOf = (v?: number | string) => {
  const ms = toMs(v);
  return ms ? new Date(ms).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : "";
};

function PersonIcon() {
  return (
    <svg viewBox="0 0 16 16" width={12} height={12} fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden>
      <circle cx="8" cy="5.2" r="2.7" />
      <path d="M2.8 14c.6-2.8 2.6-4.3 5.2-4.3s4.6 1.5 5.2 4.3" strokeLinecap="round" />
    </svg>
  );
}

/** Who wrote a message and when: a person for yours, the agent's glyph for its answers. */
export function MsgMeta({ it, mine }: { it: OItem; mine?: boolean }) {
  const ctx = useContext(ChatAgentCtx);
  const agent = ctx || agentKey(it.threadId);
  const at = clockOf(it.startedAt);
  return (
    <div className={`wmeta ${mine ? "wmeta-u" : ""}`} title={toMs(it.startedAt) ? new Date(toMs(it.startedAt)).toLocaleString() : undefined}>
      {mine ? <span className="wmeta-i" role="img" aria-label="You"><PersonIcon /></span> : <AgentIcon agent={agent} size={12} />}
      <span>{mine ? "You" : agentName(agent)}</span>
      {at && <span className="muted">· {at}</span>}
    </div>
  );
}

export function ItemRow({ it }: { it: OItem }) {
  const hl = useContext(HitCtx) === it.id;
  if (isReviewItem(it)) return null;
  return (
    <div className={`wit ${hl ? "whit" : ""}`} data-iid={it.id}>
      <ItemBody it={it} hl={hl} />
      <ChildChips itemId={it.id} />
    </div>
  );
}

function ItemBody({ it, hl }: { it: OItem; hl: boolean }) {
  const [open, setOpen] = useState(false);
  const [cmdAll, setCmdAll] = useState(false);
  const full = useCmdPref();
  useEffect(() => {
    if (hl && (it.kind === "command" || it.kind === "reasoning" || it.kind === "mcp" || it.kind === "web" || it.kind === "other")) setOpen(true);
  }, [hl, it.kind]);
  const dur = it.endedAt ? toMs(it.endedAt) - toMs(it.startedAt) : 0;
  if (it.kind === "command" && !full) {
    const fail = it.status === "failed" || (it.exitCode !== undefined && it.exitCode !== 0);
    const running = it.status === "started";
    return (
      <div className={`witem ${fail ? "fail" : ""}`}>
        <div className="wih-row">
          <button className="wih" onClick={() => setOpen(!open)} aria-expanded={open}>
            <span className={`wic ${running ? "run wpulse" : fail ? "err" : "ok"}`}>{running ? "●" : fail ? "✗" : "✓"}</span>
            <code className="trunc" title={it.command || it.text}>{cmdLabel(it.command || it.text || "")}</code>
            <span className="wim">{running ? "running" : fail ? `exit ${it.exitCode ?? "?"}` : dur > 0 ? fmtDuration(dur) : ""}</span>
            <span className="wchev">{open ? "▾" : "▸"}</span>
          </button>
          <CmdCopy cmd={(it.command || it.text || "").trim()} out={it.output} />
        </div>
        {open && (
          <>
            {(it.command || "").includes("\n") || (it.command || "").length > 90 || cmdLabel(it.command || "") !== it.command ? <pre className="wcmd-c">{it.command}</pre> : null}
            <pre className="wout">{it.output?.trim() || "No output"}</pre>
          </>
        )}
      </div>
    );
  }
  if (it.kind === "command") {
    const fail = it.status === "failed" || (it.exitCode !== undefined && it.exitCode !== 0);
    const running = it.status === "started";
    const cmd = (it.command || it.text || "").trim();
    const desc = it.command && it.text && it.text.trim() !== cmd ? it.text.trim() : "";
    const out = (it.output || "").replace(/\s+$/, "");
    const lines = out ? out.split("\n") : [];
    const cmdLines = cmd.split("\n");
    const longCmd = cmdLines.length > 6 || cmd.length > 600;
    return (
      <div className={`witem wcmd ${fail ? "fail" : ""}`}>
        <div className="wcmd-h">
          <span className={`wic ${running ? "run wpulse" : fail ? "err" : "ok"}`}>{running ? "●" : fail ? "✗" : "✓"}</span>
          <span className="wcmd-d">{desc || "Command"}</span>
          <span className="wim">{running ? "running" : fail ? `exit ${it.exitCode ?? "?"}` : dur > 0 ? fmtDuration(dur) : ""}</span>
          <CmdCopy cmd={cmd} out={out} />
        </div>
        <pre className={`wcmd-c ${longCmd && !cmdAll ? "clip" : ""}`}>
          <span className="wcmd-p">$ </span>
          {longCmd && !cmdAll ? cmdLines.slice(0, 6).join("\n").slice(0, 600) : cmd}
        </pre>
        {longCmd && (
          <button className="wcmd-more" onClick={() => setCmdAll(!cmdAll)}>
            {cmdAll ? "Show less" : "Show the whole command"}
          </button>
        )}
        {lines.length > 0 && (
          <>
            <pre className="wcmd-o">{open || lines.length <= 6 ? out : "…\n" + lines.slice(-6).join("\n")}</pre>
            {lines.length > 6 && (
              <button className="wcmd-more" onClick={() => setOpen(!open)} aria-expanded={open}>
                {open ? "Show the last 6 lines" : `Show all ${lines.length} lines of output`}
              </button>
            )}
          </>
        )}
        {!running && lines.length === 0 && <div className="wcmd-none">No output</div>}
      </div>
    );
  }
  if (it.kind === "fileChange") {
    const files = it.files || [];
    const add = files.reduce((n, f) => n + (f.add || 0), 0);
    const del = files.reduce((n, f) => n + (f.del || 0), 0);
    return (
      <div className={`witem ${it.status === "declined" || it.status === "failed" ? "fail" : ""}`}>
        <button className="wih" onClick={() => setOpen(!open)} aria-expanded={open}>
          <span className="wic ed">±</span>
          <span className="trunc">
            {it.status === "declined" ? "Edit declined" : "Edited"} {files.length === 1 ? <code>{files[0].path}</code> : `${files.length} files`}
          </span>
          <span className="wim">
            <span className="add">+{add}</span> <span className="del">−{del}</span>
          </span>
          <span className="wchev">{open ? "▾" : "▸"}</span>
        </button>
        {open && (
          <div className="wfiles">
            {files.map((f) => (
              <div key={f.path} className="f">
                <span className="p">{f.path}</span>
                <span className="n">
                  {f.kind === "add" && <span className="muted">new </span>}
                  {f.kind === "delete" && <span className="muted">deleted </span>}
                  <span className="add">+{f.add || 0}</span> <span className="del">−{f.del || 0}</span>
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    );
  }
  if (it.kind === "agent")
    return (
      <div className={`wagent ${it.status === "started" ? "streaming" : ""}`}>
        <MsgMeta it={it} />
        {it.text ? <Markdown text={it.text} collapseAt={0} /> : <span className="wcaret" />}
        {it.text && it.status !== "started" && <MsgCopy text={it.text} />}
      </div>
    );
  if (it.kind === "error")
    return (
      <div className="werr">
        {it.text || "Error"}
        {(it.repeat || 0) > 1 && (
          <span className="werr-n" title={it.endedAt ? `Last at ${new Date(it.endedAt).toLocaleString()}` : undefined}>
            {" "}×{it.repeat}
          </span>
        )}
      </div>
    );
  if (it.kind === "reasoning") {
    const first = (it.text || "").trim().split("\n")[0];
    return (
      <div className={`wthink ${open ? "open" : ""}`}>
        <button className="wih" onClick={() => setOpen(!open)} aria-expanded={open}>
          <span className={`wic ${it.status === "started" ? "run wpulse" : ""}`}>✻</span>
          <span className="trunc">
            <span className="muted">{it.status === "started" ? "Thinking" : "Thought"}</span> {!open && first}
          </span>
          <span className="wchev">{open ? "▾" : "▸"}</span>
        </button>
        {open && <div className="wthink-b">{it.text || "No text"}</div>}
      </div>
    );
  }
  if (it.kind === "plan") {
    const todos = parseTodos(it.text);
    if (!todos) return <div className="wnote"><span className="muted">Plan</span> {it.text}</div>;
    const done = todos.filter((t) => t.status === "completed").length;
    return (
      <div className="wtodo">
        <div className="wtodo-h">
          <span className="wic">☰</span>
          <span>Plan</span>
          <span className="wim">
            {done}/{todos.length} done
          </span>
        </div>
        <ul>
          {todos.map((t, i) => (
            <li key={i} className={t.status}>
              <span className="bx" aria-hidden>
                {t.status === "completed" ? "✓" : t.status === "in_progress" ? "●" : ""}
              </span>
              <span>{t.status === "in_progress" && t.activeForm ? t.activeForm : t.content}</span>
            </li>
          ))}
        </ul>
      </div>
    );
  }
  if (it.kind === "other" && GIT_CMDS.has(it.command || "")) {
    const failed = it.status === "failed";
    const skip = failed && it.command === "checkpoint";
    return (
      <div className={`witem wgitem ${failed ? (skip ? "warn" : "fail") : ""}`}>
        <button className="wih" onClick={() => setOpen(!open)} aria-expanded={open}>
          <span className={`wic ${failed ? (skip ? "wn" : "err") : "git"}`}>{skip ? "!" : failed ? "✗" : "⎇"}</span>
          <span className="trunc">
            <span className="wtl">{skip ? "Checkpoint skipped" : it.command}</span> {(it.text || "").split("\n")[0].replace(/^checkpoint skipped:?\s*/i, "")}
          </span>
          <span className="wchev">{open ? "▾" : "▸"}</span>
        </button>
        {open && <OutBox text={[it.text, it.output].filter(Boolean).join("\n\n")} />}
      </div>
    );
  }
  const tool = toolLabel(it);
  const running = it.status === "started";
  const fail = it.status === "failed" || it.status === "declined";
  return (
    <div className={`witem ${fail ? "fail" : ""}`}>
      <button className="wih" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className={`wic ${running ? "run wpulse" : fail ? "err" : tool.cls}`}>{running ? "●" : fail ? "✗" : tool.icon}</span>
        <span className="trunc">
          <span className="wtl">{tool.label}</span> {tool.detail}
        </span>
        {it.status === "declined" && <span className="wim">declined</span>}
        <span className="wchev">{open ? "▾" : "▸"}</span>
      </button>
      {open && <OutBox text={[it.text, it.output].filter(Boolean).join("\n\n")} />}
    </div>
  );
}

/** Tool output with a hover Copy menu. */
function OutBox({ text }: { text: string }) {
  return (
    <div className="wout-w">
      <pre className="wout">{text || "No output"}</pre>
      <OutCopy text={text} />
    </div>
  );
}

const GIT_CMDS = new Set(["git commit", "git push", "gh pr create", "restore", "git worktree remove", "checkpoint"]);

interface Todo {
  content: string;
  status: "pending" | "in_progress" | "completed";
  activeForm?: string;
}
/** Reads a TodoWrite payload ({todos:[...]} or an array) or a markdown checklist; null when the text is prose. */
export function parseTodos(text: string): Todo[] | null {
  const t = (text || "").trim();
  if (!t) return null;
  if (t.startsWith("{") || t.startsWith("[")) {
    try {
      const j = JSON.parse(t) as unknown;
      const arr = Array.isArray(j) ? j : (j as { todos?: unknown; plan?: unknown }).todos || (j as { plan?: unknown }).plan;
      if (Array.isArray(arr) && arr.length) {
        return arr.map((x) => {
          const o = (typeof x === "string" ? { content: x } : x) as Record<string, string>;
          const st = o.status === "completed" || o.status === "in_progress" ? o.status : o.status === "inProgress" ? "in_progress" : "pending";
          return { content: o.content || o.step || o.text || "", status: st, activeForm: o.activeForm };
        });
      }
    } catch {
      /* not JSON */
    }
  }
  const lines = t.split("\n").map((l) => l.trim()).filter(Boolean);
  const re = /^(?:[-*]|\d+[.)])\s+(?:\[([ xX~>])\]\s*)?(.+)$/;
  if (lines.length < 2 || !lines.every((l) => re.test(l))) return null;
  return lines.map((l) => {
    const m = l.match(re)!;
    const box = (m[1] || " ").toLowerCase();
    return { content: m[2], status: box === "x" ? "completed" : box === "~" || box === ">" ? "in_progress" : "pending" };
  });
}

// Claude tool items: command holds the tool name when the backend knows it (mcp__server__tool, WebFetch, Task).
function toolLabel(it: OItem): { icon: string; cls: string; label: string; detail: string } {
  const name = it.command || "";
  const first = (it.text || "").trim().split("\n")[0];
  if (it.kind === "mcp") {
    const m = name.match(/^mcp__(.+?)__(.+)$/);
    return { icon: "◆", cls: "mcp", label: m ? `${m[1]} · ${m[2]}` : name || "MCP", detail: first };
  }
  if (it.kind === "web") {
    const fetch = /fetch/i.test(name) || /^https?:\/\//.test(first);
    return { icon: fetch ? "↗" : "⌕", cls: "web", label: fetch ? "Fetch" : "Web search", detail: first };
  }
  if (name === "Task" || name === "Agent") return { icon: "↳", cls: "sub", label: "Subagent", detail: first };
  if (name === "Read") return { icon: "○", cls: "", label: "Read", detail: first };
  if (name === "Grep" || name === "Glob") return { icon: "⌕", cls: "", label: "Search", detail: first };
  return { icon: "·", cls: "", label: name || "Step", detail: name && first === name ? "" : first };
}

const FOLD_KEY = "am-fold-turns";
function readFold(): boolean {
  try {
    return localStorage.getItem(FOLD_KEY) !== "0";
  } catch {
    return true;
  }
}
const CMD_KEY = "am-cmd-full";
function readCmd(): boolean {
  try {
    return localStorage.getItem(CMD_KEY) === "1";
  } catch {
    return false;
  }
}
/** Bash rows are compact unless the user chose full commands with output. */
export function useCmdPref(): boolean {
  const [v, setV] = useState(readCmd);
  useEffect(() => {
    const on = () => setV(readCmd());
    window.addEventListener("am-cmd", on);
    return () => window.removeEventListener("am-cmd", on);
  }, []);
  return v;
}
function setCmdPref(v: boolean) {
  try {
    localStorage.setItem(CMD_KEY, v ? "1" : "0");
  } catch {}
  window.dispatchEvent(new Event("am-cmd"));
}
const isSlash = (it: OItem) => it.by === "user" && it.text.startsWith("/");
/** A ! command or slash command the user ran in the agent CLI, shown on the user side with its output. */
function UserShell({ it }: { it: OItem }) {
  const [all, setAll] = useState(false);
  const hl = useContext(HitCtx) === it.id;
  const out = (it.output || "").replace(/\s+$/, "");
  const lines = out ? out.split("\n") : [];
  const slash = isSlash(it);
  const args = slash ? (it.command || "").slice(it.text.length).trim() : "";
  return (
    <div className={`wu-wrap wbang ${hl ? "whit" : ""}`} data-iid={it.id}>
      <div className="wu wbang-b">
        {slash ? (
          <div className="wbang-c">
            <span className="chip wslash">{it.text}</span>
            {args && <span className="wslash-a">{args}</span>}
          </div>
        ) : (
          <div className="wbang-c">
            <span className="wcmd-p">! </span>
            {it.command}
            <CmdCopy cmd={(it.command || "").trim()} out={it.output} prompt="!" />
          </div>
        )}
        {it.bg && it.status === "started" && <div className="wcmd-none">Running in the background.{lines.length === 0 && " The output shows here as it is written."}</div>}
        {lines.length > 0 && <pre className={`wbang-o ${all ? "tall" : ""}`}>{out}</pre>}
        {lines.length > 24 && (
          <button className="wcmd-more" onClick={() => setAll(!all)}>
            {all ? "Smaller box" : `Taller box (${lines.length} lines)`}
          </button>
        )}
        {it.status === "started" && !it.bg && <div className="wcmd-none">Running in the terminal. The output shows when the command ends.</div>}
        <ChildChips itemId={it.id} />
      </div>
    </div>
  );
}
/** Finished turns show their steps unless the user chose to fold them. */
export function useFoldPref(): boolean {
  const [v, setV] = useState(readFold);
  useEffect(() => {
    const on = () => setV(readFold());
    window.addEventListener("am-fold", on);
    return () => window.removeEventListener("am-fold", on);
  }, []);
  return v;
}
export function FoldItem() {
  const fold = useFoldPref();
  const full = useCmdPref();
  return (
    <>
      <button role="menuitem" onClick={() => setFoldPref(!fold)}>
        {fold ? "Unfold finished turns" : "Fold finished turns"}
      </button>
      <button role="menuitem" onClick={() => setCmdPref(!full)}>
        {full ? "Compact Bash rows" : "Show Bash commands with output"}
      </button>
    </>
  );
}
export function setFoldPref(v: boolean) {
  try {
    localStorage.setItem(FOLD_KEY, v ? "1" : "0");
  } catch {}
  window.dispatchEvent(new Event("am-fold"));
}

/** Splits a Code Mode script's tool calls from the list: they show inside their script's row. */
function scriptTree(items: OItem[]): { top: OItem[]; kids: Map<string, OItem[]> } {
  const ids = new Set(items.map((i) => i.id));
  const kids = new Map<string, OItem[]>();
  const top: OItem[] = [];
  for (const i of items) {
    if (i.parentId && ids.has(i.parentId)) kids.set(i.parentId, [...(kids.get(i.parentId) || []), i]);
    else top.push(i);
  }
  return { top, kids };
}

function ScriptGroup({ it, kids }: { it: OItem; kids: OItem[] }) {
  const hl = useContext(HitCtx);
  const running = it.status === "started" || kids.some((k) => k.status === "started");
  const failed = kids.filter((k) => k.status === "failed" || (k.exitCode !== undefined && k.exitCode !== 0)).length;
  const [open, setOpen] = useState<boolean | null>(null);
  const shown = open ?? (running || (!!hl && kids.some((k) => k.id === hl)));
  return (
    <div className="wcmdg wscript">
      <ItemRow it={it} />
      <button className="wih wcmdg-h" onClick={() => setOpen(!shown)} aria-expanded={shown}>
        <span className={`wic ${running ? "run wpulse" : failed ? "err" : "ok"}`}>{running ? "●" : failed ? "✗" : "✓"}</span>
        <span className="trunc">
          Script ran {kids.length} {kids.length === 1 ? "tool call" : "tool calls"}
          {failed > 0 && <span className="muted"> · {failed} failed</span>}
        </span>
        <span className="wchev">{shown ? "▾" : "▸"}</span>
      </button>
      {shown && (
        <div className="wcmdg-b">
          {kids.map((k) => (
            <ItemRow key={k.id} it={k} />
          ))}
        </div>
      )}
    </div>
  );
}

const LONG_LINES = 14;
// A long pasted prompt folds to its first lines; terminal output keeps its columns in mono.
function UserText({ text }: { text: string }) {
  const [all, setAll] = useState(false);
  const lines = text.split("\n");
  const long = lines.length > LONG_LINES || text.length > 1500;
  const paste = lines.length >= 3 && lines.filter((l) => /^ {2,}|[│─┌└├╭╰]/.test(l)).length >= 2;
  const shown = long && !all ? lines.slice(0, LONG_LINES).join("\n").slice(0, 1500) : text;
  return (
    <>
      <span className={paste ? "wu-paste" : undefined}>{shown}</span>
      {long && (
        <button type="button" className="wu-more" onClick={(e) => (e.stopPropagation(), setAll(!all))}>
          {all ? "Show less" : `Show all ${lines.length} lines`}
        </button>
      )}
    </>
  );
}

export function RunBlock({ run, items, now, after, diff, onDiff, idle }: { run?: ORun; items: OItem[]; now: number; after?: React.ReactNode; diff?: DiffNav | null; onDiff?: (d: DiffNav) => void; idle?: boolean }) {
  // idle: the session is not running, so a run with no end record must not show as working.
  const running = !idle && (run?.status === "running" || items.some((i) => i.status === "started" && !i.bg));
  const allUsers = items.filter((i) => i.kind === "user" && (i.text || i.attachments?.length));
  // Messages typed while the agent worked show where they came, not folded into the steps.
  // Only a prompt before any agent work opens the turn; a turn opened by / or ! has none.
  const firstWork = items.findIndex((i) => i.kind !== "user" && i.by !== "user");
  const firstPrompt = allUsers.find((i) => i.by !== "schedule" && (firstWork < 0 || items.indexOf(i) < firstWork));
  const users = allUsers.filter((i) => i.by === "schedule" || i === firstPrompt);
  const later = allUsers.filter((i) => !users.includes(i));
  // A slash command that opened the turn shows above its prompt; later user commands follow the answer.
  // A turn opened by a ! command has no prompt: its leading user commands open it.
  let firstUser = items.findIndex((i) => i.kind === "user");
  if (firstUser < 0) firstUser = items.findIndex((i) => i.by !== "user");
  if (firstUser < 0) firstUser = items.length;
  const lead = items.filter((i, k) => i.by === "user" && k < firstUser);
  const bangs = items.filter((i, k) => i.by === "user" && k >= firstUser);
  const rest = items.filter((i) => i.kind !== "user" && i.by !== "user");
  const lastAgentIdx = running ? -1 : rest.map((i) => i.kind).lastIndexOf("agent");
  const work = lastAgentIdx >= 0 ? rest.filter((_, i) => i !== lastAgentIdx) : rest;
  const final = lastAgentIdx >= 0 ? rest[lastAgentIdx] : undefined;
  // A ! command shows where it ran: before the answer it came before, after the one it followed.
  const finalAt = final ? items.indexOf(final) : -1;
  const bangsBefore = finalAt >= 0 ? bangs.filter((b) => items.indexOf(b) < finalAt) : [];
  const bangsAfter = finalAt >= 0 ? bangs.filter((b) => items.indexOf(b) > finalAt) : bangs;
  const laterBefore = finalAt >= 0 ? later.filter((u) => items.indexOf(u) < finalAt) : [];
  const laterAfter = finalAt >= 0 ? later.filter((u) => items.indexOf(u) > finalAt) : later;
  const { top, kids } = scriptTree(work);
  const stepRow = (g: OItem) => (kids.has(g.id) ? <ScriptGroup it={g} kids={kids.get(g.id)!} /> : <ItemRow it={g} />);
  const [open, setOpen] = useState<boolean | null>(null);
  const hl = useContext(HitCtx);
  const sel = useSelectCtx();
  const fold = useFoldPref();
  const expanded = open ?? (running || !fold || (!!hl && work.some((i) => i.id === hl)));
  // What the agent said between steps is for the reader: folding hides the tool calls, not the messages.
  const notes = expanded ? [] : work.filter((i) => i.kind === "agent" && i.text?.trim());
  const start = toMs(run?.startedAt) || toMs(items[0]?.startedAt);
  const end = run?.endedAt ? toMs(run.endedAt) : running ? now : toMs(items[items.length - 1]?.endedAt) || start;
  const took = start && end > start ? fmtDuration(end - start) : "";
  const tail = run?.status === "interrupted" ? " · stopped" : run?.status === "failed" ? " · failed" : "";
  const cost = run?.usage?.costUsd != null && run.status !== "running" ? fmtUsd(run.usage.costUsd) : "";
  const bubble = (u0: OItem) => {
    if (u0.by === "schedule")
      return (
        <div key={u0.id} className="wwake" data-iid={u0.id} title={u0.text}>
          <span className="wwake-t">{wakeTitle(u0.text || "")}</span>
          {u0.wakeRuns && <span className="wwake-n"> · {u0.wakeRuns.n} runs since {new Date(u0.wakeRuns.since).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}, same answer</span>}
        </div>
      );
    const note = splitFileNote(u0.text || "");
    const u = note.files.length ? { ...u0, text: note.text, attachments: [...(u0.attachments || []), ...note.files] } : u0;
    return (
    <div key={u.id}>
    <div className={`wu-wrap ${hl === u.id ? "whit" : ""} ${sel?.on ? "wsel-on" : ""} ${sel?.picked.has(u.id) ? "wsel-y" : ""}`} data-iid={u.id} onClick={sel?.on ? () => sel.toggle(u.id) : undefined}>
      <SelectBox id={u.id} />
      <div className="wu">
        {u.attachments && u.attachments.length > 0 && (
          <span className="wu-att">
            {u.attachments.map((x) => PLAYABLE(x.mime) ? (
              <span key={x.id} className="wu-media" title={x.name || x.mime}>
                {x.mime.startsWith("video/") ? <video src={x.url} controls preload="metadata" /> : <audio src={x.url} controls preload="metadata" />}
                <a href={x.url} target="_blank" rel="noopener" className="trunc">{x.name || "media"}</a>
              </span>
            ) : (
              <a key={x.id} href={x.url} target="_blank" rel="noopener" title={x.name || x.mime} className={IMAGE_TYPES.includes(x.mime) ? "" : "wu-file"}>
                {IMAGE_TYPES.includes(x.mime) ? <img src={x.url} alt={x.name || "attached image"} /> : <><b>{extOf(x.name || `.${x.mime.split("/")[1] || ""}`)}</b><span className="trunc">{x.name || "file"}</span></>}
              </a>
            ))}
          </span>
        )}
        <UserText text={u.text || ""} />
      </div>
      {!sel?.on && u.text?.trim() && <MsgCopy text={u.text} className="wu-cp" title="Copy this prompt" />}
    </div>
    <MsgMeta it={u} mine />
    </div>
    );
  };
  return (
    <>
      {lead.map((b) => (
        <UserShell key={b.id} it={b} />
      ))}
      {users.map(bubble)}
      {work.length > 0 && (
        <div className="wgroup">
          <div className="worked-row">
          <button className="worked" onClick={() => setOpen(!expanded)} aria-expanded={expanded}>
            {running ? <span className="wpulse run-t">●</span> : null}
            {running ? "Working" : "Worked"}
            {took && ` for ${took}`}
            {tail}
            <span className="muted"> · {work.length} {work.length === 1 ? "step" : "steps"}</span>
            {cost && <span className="muted"> · {cost}</span>}
            {!expanded && <TrailBar items={work} now={now} />}
            <span className="wchev">{expanded ? "▾" : "▸"}</span>
          </button>
          {diff && onDiff && (
            <button className="wdiffl" onClick={() => onDiff(diff)} title={`Diff of turn ${run?.index ?? ""}`}>
              Diff
            </button>
          )}
          </div>
          {expanded && (
            <div className="wsteps">
              {trailSegments(top).map((g, n, segs) =>
                Array.isArray(g) ? <StepTrail key={g[0].id} items={g} kids={kids} now={now} live={running && n === segs.length - 1} row={stepRow} /> : <Fragment key={g.id}>{stepRow(g)}</Fragment>,
              )}
            </div>
          )}
        </div>
      )}
      {notes.map((n) => (
        <ItemRow key={n.id} it={n} />
      ))}
      {bangsBefore.map((b) => (
        <UserShell key={b.id} it={b} />
      ))}
      {laterBefore.map(bubble)}
      {final && <ItemRow it={final} />}
      {bangsAfter.map((b) => (
        <UserShell key={b.id} it={b} />
      ))}
      {laterAfter.map(bubble)}
      {work.length === 0 && final && (cost || (diff && onDiff)) && (
        <div className="wrunmeta">
          {cost}
          {diff && onDiff && (
            <button className="wdiffl" onClick={() => onDiff(diff)}>
              Diff
            </button>
          )}
        </div>
      )}
      {after}
    </>
  );
}

/** Items grouped per run, runs in order; items without a run form their own block. */
export function blocksOf(list: OItem[], runList: ORun[]): { key: string; run?: ORun; items: OItem[] }[] {
  const byRun = new Map<string, OItem[]>();
  const order: string[] = [];
  const items = [...list].sort((a, b) => toMs(a.startedAt) - toMs(b.startedAt));
  for (const i of items) {
    const k = i.runId || "";
    if (!byRun.has(k)) {
      byRun.set(k, []);
      order.push(k);
    }
    byRun.get(k)!.push(i);
  }
  const runs = new Map(runList.map((r) => [r.id, r]));
  order.sort((a, b) => (runs.get(a)?.index ?? 1e9) - (runs.get(b)?.index ?? 1e9) || toMs(byRun.get(a)![0].startedAt) - toMs(byRun.get(b)![0].startedAt));
  return foldWakeRepeats(order.map((k) => ({ key: k || "_", run: runs.get(k), items: byRun.get(k)! })));
}

type Block = { key: string; run?: ORun; items: OItem[] };
const wakeOf = (b: Block) => b.items.find((i) => i.kind === "user" && i.by === "schedule");
const answerOf = (b: Block) => [...b.items].reverse().find((i) => i.kind === "agent")?.text?.trim() || "";

// Back-to-back runs of one scheduled task with the same answer show as the newest run plus a count.
function foldWakeRepeats(blocks: Block[]): Block[] {
  const out: Block[] = [];
  for (const b of blocks) {
    const prev = out[out.length - 1];
    const w = wakeOf(b), pw = prev && wakeOf(prev);
    if (w && pw && b.run?.status !== "running" && w.text === pw.text && answerOf(b) && answerOf(b) === answerOf(prev)) {
      const since = pw.wakeRuns?.since ?? toMs(prev.run?.startedAt ?? pw.startedAt);
      const n = (pw.wakeRuns?.n ?? 1) + 1;
      out[out.length - 1] = { ...b, items: b.items.map((i) => (i === w ? { ...i, wakeRuns: { n, since } } : i)) };
      continue;
    }
    out.push(b);
  }
  return out;
}

// "Scheduled task: X watch (long instructions…)" shows as "Scheduled task: X watch"; the tooltip keeps all of it.
function wakeTitle(t: string): string {
  const head = t.indexOf(": ");
  const body = head > 0 ? t.slice(head + 2) : t;
  const cut = body.search(/ \(|\. /);
  const short = cut >= 8 ? body.slice(0, cut) : body;
  return (head > 0 ? t.slice(0, head + 2) : "") + (short.length > 80 ? short.slice(0, 80) + "…" : short);
}

type KidState = "need" | "run" | "done" | "err" | "closed";
export function kidState(c: OThread, needIds: Set<string>): KidState {
  if (needIds.has(c.id) || c.status === "waiting") return "need";
  if (c.status === "running" || c.status === "starting") return "run";
  if (c.status === "error") return "err";
  return c.status === "closed" ? "closed" : "done";
}
export const KID_DOT: Record<KidState, string> = { need: "var(--need)", run: "var(--run)", done: "var(--ok)", err: "var(--err)", closed: "var(--line-2)" };
const KID_LABEL: Record<KidState, string> = { need: "Needs you", run: "Working", done: "Ended", err: "Error", closed: "Closed" };
const kidTime = (c: OThread, st: KidState, now: number) => {
  const a = toMs(c.createdAt);
  const b = st === "run" || st === "need" ? now : toMs(c.updatedAt);
  return a && b > a ? fmtDuration(b - a) : "";
};

function KidRow({ c, kids, needIds, now, depth }: { c: OThread; kids: Map<string, OThread[]>; needIds: Set<string>; now: number; depth: number }) {
  const st = kidState(c, needIds);
  const grand = depth < 3 ? kids.get(c.id) || [] : [];
  return (
    <>
      <Link to={`/w/${encodeURIComponent(c.id)}`} className={`wkid ${depth > 1 ? "deep" : ""}`} style={depth > 1 ? { paddingLeft: 12 + (depth - 1) * 18 } : undefined}>
        <span className={`wdot ${st === "run" || st === "need" ? "wpulse" : ""}`} style={{ background: KID_DOT[st] }} aria-hidden />
        <span className="nm">
          {depth > 1 && <span className="muted">↳ </span>}
          {c.title}
          <small>{WORKSPACE_LABEL[workspaceOf(c)]}</small>
        </span>
        <span className={`wag ${agentCls(c.agent)} wagl`}>{agentLabel(c.agent)}</span>
        <span className="tm">{kidTime(c, st, now)}</span>
        <span className={`kst ${st}`}>{KID_LABEL[st]}</span>
      </Link>
      {grand.map((g) => (
        <KidRow key={g.id} c={g} kids={kids} needIds={needIds} now={now} depth={depth + 1} />
      ))}
    </>
  );
}

export function SubagentCard({ list, kids, needIds, now }: { list: OThread[]; kids: Map<string, OThread[]>; needIds: Set<string>; now: number }) {
  const [open, setOpen] = useState(true);
  const states = list.map((c) => kidState(c, needIds));
  const n = (k: KidState[]) => states.filter((s) => k.includes(s)).length;
  const done = n(["done", "closed"]);
  const need = n(["need", "err"]);
  const run = n(["run"]);
  const meta = [done && `${done} done`, need && `${need} ${need === 1 ? "needs" : "need"} you`, run && `${run} running`].filter(Boolean).join(" · ");
  const start = Math.min(...list.map((c) => toMs(c.createdAt) || now));
  const end = run || need ? now : Math.max(...list.map((c) => toMs(c.updatedAt)));
  return (
    <div className="wsub" role="group" aria-label={`${list.length} subagents`}>
      <button className="wsub-h" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="wstack" aria-hidden>
          {list.slice(0, 5).map((c) => (
            <i key={c.id} style={{ background: agentVar(c.agent) }} />
          ))}
        </span>
        <span className="x">
          <b>
            {list.length} {list.length === 1 ? "subagent" : "subagents"}
          </b>
          <small>{meta}</small>
        </span>
        <span className="tm">
          {end > start ? fmtDuration(end - start) : ""} {open ? "▾" : "▸"}
        </span>
      </button>
      {open && list.map((c) => <KidRow key={c.id} c={c} kids={kids} needIds={needIds} now={now} depth={1} />)}
    </div>
  );
}

export function useTick(on: boolean, ms = 1000) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!on) return;
    const t = window.setInterval(() => setNow(Date.now()), ms);
    return () => window.clearInterval(t);
  }, [on, ms]);
  return now;
}

export function DrivenChat({ detail, onBack, onSide, hit, linkedPage }: { detail: ODetail; onBack?: () => void; onSide?: () => void; hit?: HitReq | null; linkedPage?: boolean }) {
  const v = useV3();
  const git = useGit(detail.thread.id, gitKey(detail));
  const dnav = useDiffNav();
  const local = isLoopback();
  const termOpen = useTermOpen();
  const cps = git.data?.checkpoints || [];
  const noGit = git.status === "nogit" || git.status === "missing" || !!detail.thread.worktree?.removed;
  const orch = useOrch();
  const t = detail.thread;
  const threads = useMemo(() => {
    const m = drivenThreads(orch);
    for (const c of detail.children || []) if (!m.has(c.id)) m.set(c.id, c);
    return m;
  }, [orch, detail.children]);
  const kids = useMemo(() => childMap(threads), [threads]);
  const myKids = kids.get(t.id) || [];
  const desc = useMemo(() => descendants(t.id, kids), [t.id, kids]);
  const ny = useNeedsYou();
  const needIds = useMemo(() => drivenNeedIds(ny.items, v.approvals.values()), [ny.version, v.approvals]); // eslint-disable-line react-hooks/exhaustive-deps
  const parent = t.parentId ? threads.get(t.parentId) : undefined;
  const running = t.status === "running" || t.status === "starting";
  // A "!" command runs outside the agent turn; Stop ends it first.
  const bashRun = detail.runs.some((r) => r.status === "running" && r.id.startsWith("ob_"));
  const waitingOn = myKids.filter((c) => c.status === "running" || c.status === "starting" || c.status === "waiting");
  const now = useTick(running || waitingOn.length > 0 || detail.runs.some((r) => r.status === "running"));
  const feed = useRef<HTMLDivElement>(null);
  const keepPlace = useKeepPlace(feed);
  const [text, setText] = useDraft("d:" + t.id);
  useDocTitle(t.title);
  const [busy, setBusy] = useState(false);

  const blocks = useMemo(() => blocksOf(detail.items, detail.runs), [detail.items, detail.runs]);
  const [atBottom, setAtBottom] = useState(true);
  const sent = usePendingSends(detail.items, t.id);
  const newMark = useNewMark(t.id, detail.items, atBottom);

  // A continued session: its earlier conversation, read-only, and who owns the session now.
  const lv = useLive();
  const ho = handoverOf(t, serverDrivenRow(v.threads, t.id), t.nativeId ? lv.sessions.get(`${t.agent}:${t.nativeId}`) : undefined, detail.runs);
  const [liveRefused, setLiveRefused] = useState(false);
  useEffect(() => setLiveRefused(false), [t.id]);
  const termLive = ho.live || liveRefused;
  const termTo = `/w/${encodeURIComponent(terminalTarget(ho.sid, v.threads))}`;
  const past = useWatchedFeed(t.agent === "claude" || t.agent === "codex" ? ho.sid : "", t.id, false, false);
  const pastBlocks = useMemo(() => {
    const f = past.feed;
    if (!f) return [];
    const born = toMs(t.createdAt);
    const items = f.items.filter((i) => toMs(i.startedAt) < born);
    const ids = new Set(items.map((i) => i.runId));
    return blocksOf(items, f.runs.filter((r) => ids.has(r.id)));
  }, [past.feed, t.createdAt]);

  const ownAsks: AskInfo[] = useMemo(
    () =>
      detail.requests
        .filter((r) => r.state === "open")
        .map((r) => askFromRequest(r, v.approvals.get(r.approvalId), detail.items.find((i) => i.id === r.itemId), t.agent)),
    [detail.requests, detail.items, v.approvals, t.agent],
  );
  // Requests of subagents at any depth, decided here and labelled with the subagent's title.
  const kidAsks: AskInfo[] = useMemo(() => {
    const ids = new Map(desc.map((d) => [d.id, d]));
    const own = new Set(ownAsks.map((a) => a.approvalId));
    return [...v.approvals.values()]
      .filter((a) => a.state === "pending" && a.drivenId && !own.has(a.id) && (ids.has(a.drivenId) || (a.rootId === t.id && a.drivenId !== t.id)))
      .sort((a, b) => a.createdAt - b.createdAt)
      .map((a) => {
        const c = ids.get(a.drivenId!) || threads.get(a.drivenId!);
        return { ...askFromApproval(a), from: { id: a.drivenId!, title: c?.title || "Subagent" } };
      });
  }, [v.approvals, desc, ownAsks, t.id, threads]);
  const asks = useMemo(() => [...ownAsks, ...kidAsks], [ownAsks, kidAsks]);

  // One card per run that delegated: a child goes to the run whose time span holds its start.
  const cards = useMemo(() => {
    const m = new Map<string, OThread[]>();
    const runs = [...detail.runs].sort((a, b) => toMs(a.startedAt) - toMs(b.startedAt));
    for (const c of myKids) {
      const at = toMs(c.createdAt);
      let key = "_end";
      for (const r of runs) if (toMs(r.startedAt) <= at + 1000) key = r.id;
      m.set(key, [...(m.get(key) || []), c]);
    }
    return m;
  }, [myKids, detail.runs]);

  // Stick to the bottom while new content arrives, unless the reader scrolled up.
  const pinned = useRef(true);
  const lastLen = detail.items.reduce((n, i) => n + (i.text?.length || 0), 0) + detail.items.length + asks.length + pastBlocks.length + sent.list.length;
  useEffect(() => {
    const el = feed.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [lastLen, t.id]);
  useEffect(() => {
    pinned.current = true;
  }, [t.id]);
  const hlId = useHitFocus({ hit: hit || null, items: detail.items, runs: detail.runs, scroller: feed, pinned });
  const sel = useSelection(t.id);
  const allBlocks = useMemo(() => [...pastBlocks, ...blocks], [pastBlocks, blocks]);


  const closed = t.status === "closed";
  const wtGone = !!t.worktree?.removed;
  const att = useAttachments(t.id);
  const q = useQueue(t.id);
  const qref = useRef<QueueListHandle>(null);
  // While the agent works (or a terminal holds the session) a message waits in the queue; it never jumps the line.
  const queueing = q.status !== "missing" && !closed && (running || t.status === "waiting" || termLive || q.items.some((i) => i.state === "queued" || i.state === "sending"));
  const send = async () => {
    const body = text.trim();
    if ((!body && !att.ids.length) || busy || closed || wtGone || att.uploading) return;
    if (att.failed) return void showToast("A file did not upload", "Retry it or remove it, then send.", { kind: "alert" });
    setBusy(true);
    try {
      // A side question and a ! command never wait for the agent: neither starts a turn in this thread.
      const side = /^\/side\s+\S/.test(body) && !att.ids.length;
      const bang = body.startsWith("!") && !att.ids.length;
      if (side) {
        await sendMessage(t.id, body);
        setText(clearSent(body));
        showToast("Side question sent", `A read-only ${agentName(t.agent)} answers it. This thread keeps working.`);
        return;
      }
      if (queueing && !bang) {
        await queueApi.add(t.id, body, { attachments: att.ids });
        setText(clearSent(body));
        att.clear();
        return;
      }
      await sendMessage(t.id, body, att.ids);
      sent.add(body, false);
      setText(clearSent(body));
      att.clear();
      pinned.current = true;
    } catch (e) {
      if (spendBlockOf(e)) return;
      if (e instanceof ApiError && e.status === 409 && (e.body as { error?: string } | undefined)?.error === "live") setLiveRefused(true);
      else if (e instanceof ApiError && e.status === 409) showToast("Cannot steer now", `${agentName(t.agent)} cannot take a message while this turn runs. Wait, or press Stop.`);
      else showToast("Send failed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  const stop = async (cascade?: boolean) => {
    try {
      await interruptThread(t.id, cascade);
    } catch (e) {
      showToast("Stop failed", (e as Error).message, { kind: "alert" });
    }
  };
  const [confirmClose, setConfirmClose] = useState(false);
  const [reviewing, setReviewing] = useState(false);
  useEffect(() => setReviewing(false), [t.id]);
  const nav = useNavigate();
  const pane = useContext(PaneCtx);
  const canReview = !closed && !noGit && t.review?.state !== "running";
  const commands: PickCommand[] = [
    ...(canReview ? [{ key: "review", label: "Review the changes", run: () => setReviewing(true) }] : []),
    ...(noGit ? [] : [{ key: "diff", label: "Show the thread diff", run: () => dnav.open({ from: "base", to: "worktree", label: "Thread" }) }]),
    ...(running || bashRun ? [{ key: "stop", label: bashRun ? "Stop the running ! command" : "Stop the running turn", run: () => void stop() }] : []),
    { key: "terminal", label: termOpen ? "Hide the terminal" : "Open a terminal here", run: toggleTerm },
    { key: "side", label: "Ask a side question (the agent keeps working)", run: () => setText("/side ") },
    { key: "rename", label: "Rename this thread", run: () => startRename(pane) },
    { key: t.pinned ? "unpin" : "pin", label: t.pinned ? "Unpin this thread" : "Pin this thread", run: () => void pinThread(t.id, !t.pinned).catch((e: Error) => showToast("Pin failed", e.message, { kind: "alert" })) },
    { key: t.group === "settled" ? "unsettle" : "settle", label: t.group === "settled" ? "Move back to the active list" : "Settle this thread", run: () => void settleThread(t.id, t.group !== "settled").catch((e: Error) => showToast("Settle failed", e.message, { kind: "alert" })) },
    ...(closed ? [] : [{ key: "close", label: "Close this thread", run: () => setConfirmClose(true) }]),
    { key: "new", label: "Start a new thread", run: () => nav("/w/new") },
    { key: "template", label: "Templates", run: () => nav("/settings/templates") },
  ];
  const openDesc = desc.filter((d) => d.status !== "closed");
  const close = async () => {
    setConfirmClose(false);
    try {
      await closeThread(t.id);
    } catch (e) {
      showToast("Close failed", (e as Error).message, { kind: "alert" });
    }
  };
  const linked = useMemo<LinkedEntry[]>(() => {
    const dot = (c: OThread) => ({ need: "need", run: "run wpulse", done: "done", err: "err", closed: "" })[kidState(c, needIds)];
    const out: LinkedEntry[] = [];
    if (parent) out.push({ key: "p:" + parent.id, group: "Parent", label: "parent", title: parent.title, sub: agentLabel(parent.agent), dot: dot(parent), href: `/w/${encodeURIComponent(parent.id)}` });
    for (const c of desc) out.push({ key: "c:" + c.id, group: "Subagents", label: "subagent", title: c.title, sub: `${agentLabel(c.agent)} · ${c.status}`, dot: dot(c), href: `/w/${encodeURIComponent(c.id)}` });
    if (ho.sid && (ho.terminal || pastBlocks.length > 0)) out.push({ key: "s:" + ho.sid, group: "Threads", label: "terminal session", title: "The terminal session of this thread", sub: termLive ? "live" : "ended", href: termTo });
    return out;
  }, [parent, desc, needIds, ho.sid, ho.terminal, pastBlocks.length, termLive, termTo]);
  const block = orch.blocked.get(t.id);
  const label = drivenLabel(t, ownAsks.length, kidAsks.length ? "need" : waitingOn.length ? "working" : null);
  const stCls = asks.length || t.status === "waiting" ? "need" : running || waitingOn.length ? "run" : t.status === "error" ? "err" : "ok";
  const usedCards = new Set<string>();
  const cardFor = (key: string) => {
    const list = cards.get(key);
    if (!list) return null;
    usedCards.add(key);
    return <SubagentCard list={list} kids={kids} needIds={needIds} now={now} />;
  };
  // The review card sits after the run that holds the review item, else at the end of the feed.
  const reviewRun = t.review ? [...detail.items].reverse().find(isReviewItem)?.runId : undefined;
  const reviewInBlock = !!reviewRun && blocks.some((b) => b.run?.id === reviewRun);
  const reviewCard = t.review ? <ReviewCard t={t} now={now} onDiff={noGit ? undefined : dnav.open} onAgain={canReview ? () => setReviewing(true) : undefined} /> : null;

  const fillBox = (raw: string, lang: string) => {
    setText((cur: string) => insertFor(raw, lang, false, cur));
    // The open diff is a modal: keep the focus in it, or its keys type into the hidden box.
    if (!document.querySelector(".wdiff")) requestAnimationFrame(() => document.querySelector<HTMLTextAreaElement>(".wchat textarea")?.focus());
  };
  return (
    <FillCtx.Provider value={fillBox}>
    <section className="wchat" aria-label="Thread">
      <TopReveal />
      <div className="wbar">
        {onBack && (
          <button className="btn sm ghost wback" onClick={onBack} aria-label="Back to threads">
            ‹
          </button>
        )}
        <ListShowBtn />
        <div className="wcrumb">
          <EditTitle title={t.title} save={(v) => renameThread(t.id, v)} />
          {parent && (
            <Link className="rp wparent trunc hidephone" to={`/w/${encodeURIComponent(parent.id)}`} title={`Subagent of ${parent.title}`}>
              ↳ {parent.title}
            </Link>
          )}
        </div>
        {stCls !== "ok" || closed ? <span className={`wstat ${closed ? "mute" : stCls}`}>{label}</span> : null}
        <LinkedChip list={linked} page={`/w/${encodeURIComponent(t.id)}/linked`} />
        <CostBadge cost={detail.runs.reduce((n, r) => n + (r.usage?.costUsd || 0), 0)} />
        <span className="sp" />
        <div className="wbar-a">
          <button className="btn sm ghost wbar-diff" disabled={noGit} title={noGit ? (git.status === "missing" ? "This daemon has no git routes yet" : "Not a git repository") : "Everything this thread changed, from its start to the working tree"} onClick={() => dnav.open({ from: "base", to: "worktree", label: "Thread" })}>
            Diff
          </button>
          <NewThreadBtn />
          <BarMenu>
            <TopBarItem />
            <NarrowBarItems status={stCls !== "ok" || closed ? { cls: closed ? "mute" : stCls, l: label } : null} onSide={onSide} />
            <button role="menuitem" className="wmenu-narrow" disabled={noGit} onClick={() => dnav.open({ from: "base", to: "worktree", label: "Thread" })}>
              Diff
            </button>
            <button role="menuitem" onClick={toggleTerm}>
              {termOpen ? "Hide terminal" : "Terminal"}
              <kbd className="wkbd hidephone">ctrl+`</kbd>
            </button>
            <FoldItem />
            <button role="menuitem" onClick={sel.start} disabled={!allBlocks.length}>
              Select prompts to export
            </button>
            {local && (
              <>
                <button role="menuitem" onClick={() => openIn(t.id, "terminal")}>
                  Open a terminal here
                </button>
                <button role="menuitem" onClick={() => openIn(t.id, "editor")}>
                  Open in editor
                </button>
              </>
            )}
            {canReview && (
              <button role="menuitem" onClick={() => setReviewing(true)}>
                Review…
                <kbd className="wkbd hidephone">/review</kbd>
              </button>
            )}
            {!closed && (
              <button role="menuitem" onClick={() => setConfirmClose(true)}>
                Close thread
              </button>
            )}
          </BarMenu>
          {onSide && <DetailsBtn onSide={onSide} />}
        </div>
      </div>
      {linkedPage ? (
        <LinkedPage list={linked} back={`/w/${encodeURIComponent(t.id)}`} />
      ) : (
      <>
      {reviewing && canReview && <ReviewStart t={t} onDone={() => setReviewing(false)} />}
      {confirmClose && !closed && (
        <div className="wconfirm" role="group" aria-label="Confirm close">
          <span>
            {openDesc.length
              ? `Close this thread and its ${openDesc.length} ${openDesc.length === 1 ? "subagent" : "subagents"}? Their processes stop too. Worktrees and branches stay.`
              : `Close this thread? The ${agentName(t.agent)} process stops.`}
          </span>
          <button className="btn sm deny" onClick={close}>
            {openDesc.length ? `Close all ${openDesc.length + 1}` : "Close thread"}
          </button>
          <button className="btn sm ghost" onClick={() => setConfirmClose(false)}>
            Cancel
          </button>
        </div>
      )}
      <SelectCtx.Provider value={sel}>
      <HitCtx.Provider value={hlId}>
      <ChatAgentCtx.Provider value={t.agent}>
      <div
        className={`wfeed ${sel.on ? "wselecting" : ""}`}
        ref={feed}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
          if (pinned.current !== atBottom) setAtBottom(pinned.current);
        }}
      >
        {parent && (
          <div className="wsubof">
            <span className="muted">↳ Subagent of</span>
            <Link to={`/w/${encodeURIComponent(parent.id)}`} className="trunc">
              {parent.title}
            </Link>
            <span className="muted nowrap">· {WORKSPACE_LABEL[workspaceOf(t)]}</span>
          </div>
        )}
        {past.loadEarlier && (
          <button className="wearlier" disabled={past.earlierBusy} onClick={() => (keepPlace(), past.loadEarlier?.())}>
            {past.earlierBusy ? "Loading earlier turns…" : "Show earlier turns"}
          </button>
        )}
        {pastBlocks.length > 0 && (
          <div className="wpast" aria-label="Earlier conversation of this session">
            {pastBlocks.map((b) => (
              <RunBlock key={"past:" + b.key} run={b.run} items={b.items} now={now} />
            ))}
          </div>
        )}
        {ho.sid && (pastBlocks.length > 0 || past.loadEarlier) && <div className="wcontinued"><span>Continued in agent-monitor</span></div>}
        {blocks.length === 0 &&
          (t.status === "starting" || detail.runs.some((r) => r.status === "running") ? (
            <div className="state">Starting {agentName(t.agent)}…</div>
          ) : ho.terminal ? null : (
            <div className="state">{ho.sid ? "Send a message to continue this session." : "Send a message to start."}</div>
          ))}
        {blocks.map((b, bi) => (
          <Fragment key={b.key}>
          {b.key === newMark && bi > 0 && <NewLine since={sinceText(detail.items, newMark)} />}
          <RunBlock run={b.run} items={b.items.filter((i) => !isReviewItem(i))} now={now} after={b.run ? <>{cardFor(b.run.id)}{reviewInBlock && b.run.id === reviewRun && reviewCard}</> : undefined} diff={b.run && git.status === "ok" ? runDiff(cps, b.run, b.items.some((i) => i.kind === "fileChange" && i.status === "completed")) : null} onDiff={dnav.open} />
          </Fragment>
        ))}
        <PendingBubbles list={sent.list} drop={sent.drop} />
        {t.error && t.status === "error" && !running && <ErrorCard t={t} runs={detail.runs} items={detail.items} />}
        {[...cards.keys()].filter((k) => !usedCards.has(k)).map((k) => (
          <SubagentCard key={k} list={cards.get(k)!} kids={kids} needIds={needIds} now={now} />
        ))}
        {!reviewInBlock && reviewCard}
        <AskAnnouncer asks={asks} />
        {asks.map((a) => (
          <Boundary key={a.approvalId} what="This request">
            <WsApproval ask={a} />
          </Boundary>
        ))}
        {waitingOn.length > 0 && !closed && (
          <div className="wwait">
            <span className="wpulse run-t">◌</span>
            <span>
              Waiting on {waitingOn.length} {waitingOn.length === 1 ? "subagent" : "subagents"}
            </span>
            <span className="lk trunc">{waitingOn.map((c) => c.title).join(", ")}</span>
            <button className="btn sm ghost" onClick={() => stop(true)} title="Interrupt this thread and every subagent under it">
              Stop
            </button>
          </div>
        )}
      </div>
      <ScrollJump el={feed} />
      </ChatAgentCtx.Provider>
      </HitCtx.Provider>
      </SelectCtx.Provider>
      <SelectBar sel={sel} blocks={allBlocks} title={t.title} />
      <QueueList
        ref={qref}
        threadId={t.id}
        q={q}
        sendLabel={running ? "Steer now" : "Send now"}
        pausedNote={q.paused === "terminal" && q.items.length ? "Paused: a terminal uses this session. Queued messages wait until you send one now." : undefined}
        onEdit={(body) => setText((cur) => (cur.trim() ? `${cur}\n${body}` : body))}
      />
      <Composer
        value={text}
        onChange={setText}
        onSubmit={send}
        onEmptyUp={() => qref.current?.selectNewest() ?? false}
        disabled={closed || wtGone}
        autoFocus={!closed && !wtGone && detail.runs.length === 0 && !ho.terminal}
        placeholder={closed ? "This thread is closed." : wtGone ? `The worktree was removed. The branch ${t.worktree?.branch || ""} stays.` : queueing ? (narrowScreen() ? "Queue a message. /side asks now" : "Queue a message. /side asks now, ! runs a command") : `Ask a follow-up. @ files${t.agent === "claude" || t.agent === "codex" ? ", $ skills" : ""}, / commands`}
        cwd={t.worktree && !t.worktree.removed ? t.worktree.path : t.cwd}
        agent={t.agent}
        files
        skills={t.agent === "claude" || t.agent === "codex"}
        commands={commands}
        bash
        attach={att}
        top={
          <>
            {block && !closed && <SpendBlocked id={t.id} b={block} />}
            {(ho.terminal || liveRefused) && !closed && (
              <div className="wmoved" role="status">
                <span>{termLive ? "The terminal is using this session. Messages here wait until it ends." : "Continued in a terminal. Newer turns are in the terminal session."}</span>
                <Link className="btn sm pri" to={termTo}>
                  Open the terminal session
                </Link>
              </div>
            )}
            {wtGone && !closed && <ParentCheckout t={t} />}
          </>
        }
      >
          <span className="wchip">
            <span className={`wag ${agentCls(t.agent)}`} />
            {agentLabel(t.agent)}
            {t.model ? ` · ${t.model}` : ""}
          </span>
          <span className="wchip">{t.agent === "codex" ? t.sandbox || "read-only" : permModeLabel(t.permissionMode || "manual")}</span>
          <ResumeChip tool={t.agent} sessionId={t.nativeId} cwd={t.worktree && !t.worktree.removed ? t.worktree.path : t.cwd} />
          <span className="sp" />
          {(running || bashRun) && (
            <button className="btn sm" onClick={() => stop()} title={bashRun ? "Stop the running ! command" : "Interrupt the running turn"}>
              ■ Stop
            </button>
          )}
          <button className="wsend" onClick={send} disabled={busy || closed || wtGone || !!block || att.uploading || (!text.trim() && !att.ids.length)} aria-label={queueing ? "Queue" : "Send"} title={att.uploading ? "Wait for the files to upload" : queueing ? "Queue (⌘↵). Select it and press ctrl+s to steer now" : "Send (⌘↵)"}>
            ↑
          </button>
      </Composer>
      </>
      )}
      <TermDock target={drivenTerm(t.id, t.cwd)} />
      {dnav.cur && <DiffView threadId={t.id} nav={dnav.cur} onClose={dnav.close} onFile={dnav.setFile} />}
    </section>
    </FillCtx.Provider>
  );
}

function SpendBlocked({ id, b }: { id: string; b: SpendBlock }) {
  const [busy, setBusy] = useState(false);
  const day = b.scope === "day";
  const go = async () => {
    setBusy(true);
    try {
      await allowOnce(id);
      showToast("One more turn allowed", "Send your message now. The limit applies again after this turn.");
    } catch (e) {
      showToast("Could not allow the turn", (e as Error).message, { kind: "alert" });
    }
    setBusy(false);
  };
  return (
    <div className="wblock" role="alert">
      <div className="x">
        <b>{day ? "Today's spend limit is reached" : "This thread reached its spend limit"}</b>
        <span>
          {usd(b.spent)} of {usd(b.limit)} spent. agent-monitor stopped the turn and holds new ones.
        </span>
      </div>
      <div className="a">
        <button className="btn sm pri" disabled={busy} onClick={go}>
          {busy ? "Allowing…" : "Allow this thread once more"}
        </button>
        <Link className="btn sm ghost" to="/settings/limits">
          Change limits
        </Link>
      </div>
    </div>
  );
}

function ParentCheckout({ t }: { t: OThread }) {
  const [busy, setBusy] = useState(false);
  const go = async () => {
    setBusy(true);
    try {
      await continueInParent(t.id);
    } catch (e) {
      showToast("Could not move the thread", (e as Error).message, { kind: "alert" });
    }
    setBusy(false);
  };
  return (
    <div className="wmoved">
      <span>
        The worktree is gone. The branch <code>{t.worktree?.branch}</code> stays. Continue in <code>{shortCwd(t.worktree?.origin || t.cwd)}</code> to keep talking to this agent there.
      </span>
      <button className="btn sm pri" disabled={busy} onClick={go}>
        {busy ? "Moving…" : "Continue in the parent checkout"}
      </button>
    </div>
  );
}

/** A quiet ⋯ menu for the less used thread actions. */
export function BarMenu({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const off = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("mousedown", off);
    window.addEventListener("keydown", key);
    return () => {
      window.removeEventListener("mousedown", off);
      window.removeEventListener("keydown", key);
    };
  }, [open]);
  return (
    <div className="wopen" ref={ref}>
      <button className="btn sm ghost" aria-haspopup="menu" aria-expanded={open} aria-label="More actions" onClick={() => setOpen(!open)}>
        ⋯
      </button>
      {open && (
        <div className="wopen-m" role="menu" onClick={() => setOpen(false)}>
          {children}
        </div>
      )}
    </div>
  );
}

/** Menu items that stand in for the header's status label, Details and + on narrow screens. */
export function NarrowBarItems({ status, onSide }: { status?: { cls: string; l: string } | null; onSide?: () => void }) {
  const nav = useNavigate();
  return (
    <>
      {status && (
        <div className={`wmenu-narrow wmenu-st wstat ${status.cls}`} role="presentation">
          {status.l}
        </div>
      )}
      {onSide && (
        <button role="menuitem" className="wmenu-narrow" onClick={onSide}>
          Details
        </button>
      )}
      <button role="menuitem" className="wmenu-narrow" onClick={() => nav("/w/new")}>
        New thread
      </button>
    </>
  );
}

export function NotFoundChat({ status, onBack }: { status: string; onBack?: () => void }) {
  return (
    <section className="wchat">
      {onBack && (
        <div className="wbar">
          <button className="btn sm ghost wback" onClick={onBack} aria-label="Back to threads">
            ‹
          </button>
        </div>
      )}
      <div className="wfeed">
        {status === "loading" ? (
          <div className="skel">
            <i style={{ width: "60%" }} />
            <i style={{ width: "85%" }} />
            <i style={{ width: "40%" }} />
          </div>
        ) : status === "missing" ? (
          <div className="state">
            <b>Orchestrator API not available on this daemon</b>
            <div>This build of agent-monitor does not serve /api/orch yet. Watched threads still work.</div>
          </div>
        ) : (
          <div className="state">
            <b>Thread not found</b>
            <Link to="/w">Back to the Workspace</Link>
          </div>
        )}
      </div>
    </section>
  );
}

/** The error that ended the thread, at the end of the chat where the reader is, with a retry of the last prompt. */
function ErrorCard({ t, runs, items }: { t: OThread; runs: ORun[]; items: OItem[] }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const usable = (p?: string) => !!p?.trim() && !p.trim().startsWith("!") && !p.trim().startsWith("/side ");
  const last =
    [...runs].sort((a, b) => b.index - a.index).find((r) => usable(r.prompt))?.prompt ||
    [...items].reverse().find((i) => i.kind === "user" && usable(i.text))?.text ||
    "";
  const lines = t.error!.split("\n");
  const long = lines.length > 6;
  const retry = async () => {
    setBusy(true);
    try {
      await sendMessage(t.id, last);
    } catch (e) {
      showToast("Retry failed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="werrcard" role="alert">
      <div className="werrcard-h">
        <span className="werrcard-t">{agentName(t.agent)} stopped with an error</span>
        <button className="btn sm ghost" onClick={() => void copyWithToast(t.error!).then((ok) => ok && showToast("Error copied"))}>
          Copy error
        </button>
      </div>
      <pre className="werrcard-m">{open || !long ? t.error : lines.slice(0, 6).join("\n")}</pre>
      {long && (
        <button className="btn sm ghost" onClick={() => setOpen(!open)}>
          {open ? "Show less" : `Show all ${lines.length} lines`}
        </button>
      )}
      <div className="werrcard-a">
        {last && (
          <button className="btn sm pri" disabled={busy} onClick={() => void retry()} title={last}>
            {busy ? "Sending…" : "Try again"}
          </button>
        )}
        <span className="muted">{last ? "Sends your last message again." : "Send a message to start the agent again."}</span>
      </div>
    </div>
  );
}

const narrowScreen = () => typeof window !== "undefined" && window.matchMedia("(max-width: 760px)").matches;
