import { createContext, useState } from "react";
import { api } from "../../api/client";
import type { PermissionRequest, Session } from "../../api/types";
import { showToast } from "../../app/toast";
import { shortCwd } from "../../lib/format";
import { dropPerm } from "../../lib/ws";
import { ApiError, v3 } from "../api/client";
import { dropNeedsYou, refreshNeedsYou, useNeedsYou, type NeedsYouItem } from "../api/needsYou";
import type { Approval, DecideBody } from "../api/types";
import { agentName, applyDecision, loadApprovals, useV3 } from "../live";
import { confirmKey } from "../keys";
import { sentToTerminal } from "./TermShown";

export type Line = { k: "a" | "r" | "h" | "c"; t: string };
const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : Array.isArray(v) ? v.join(" ") : JSON.stringify(v, null, 2));

/** Diff lines for Edit, MultiEdit, Write and apply_patch inputs; null when the tool is not a file change. */
export function diffLines(tool: string, input: Record<string, unknown>, max = 200): Line[] | null {
  const cap = (ls: Line[]) => (ls.length > max ? [...ls.slice(0, max), { k: "h" as const, t: `… ${ls.length - max} more lines` }] : ls);
  const fp = str(input.file_path || input.path);
  if (tool === "Edit" && (input.old_string !== undefined || input.new_string !== undefined)) {
    return cap([{ k: "h", t: `@@ ${shortCwd(fp)}` }, ...str(input.old_string).split("\n").map((t) => ({ k: "r" as const, t: "- " + t })), ...str(input.new_string).split("\n").map((t) => ({ k: "a" as const, t: "+ " + t }))]);
  }
  if (tool === "MultiEdit" && Array.isArray(input.edits)) {
    const out: Line[] = [{ k: "h", t: `@@ ${shortCwd(fp)}` }];
    for (const e of input.edits as Record<string, unknown>[]) {
      out.push(...str(e.old_string).split("\n").map((t) => ({ k: "r" as const, t: "- " + t })));
      out.push(...str(e.new_string).split("\n").map((t) => ({ k: "a" as const, t: "+ " + t })));
    }
    return cap(out);
  }
  if (tool === "Write" && input.content !== undefined) {
    return cap([{ k: "h", t: `@@ new content · ${shortCwd(fp)}` }, ...str(input.content).split("\n").map((t) => ({ k: "a" as const, t: "+ " + t }))]);
  }
  const patch = tool === "apply_patch" ? str(input.patch || input.input || input.command) : "";
  if (patch) {
    return cap(
      patch.split("\n").map((t): Line => {
        if (/^(\*\*\*|@@)/.test(t)) return { k: "h", t };
        if (t.startsWith("+")) return { k: "a", t };
        if (t.startsWith("-")) return { k: "r", t };
        return { k: "c", t };
      }),
    );
  }
  return null;
}
export function DiffView({ lines }: { lines: Line[] }) {
  return (
    <div className="diff">
      {lines.map((l, i) => (
        <div key={i} className={l.k === "c" ? "" : l.k}>
          {l.t || " "}
        </div>
      ))}
    </div>
  );
}

/** The daemon decides: alwaysOk is false for high risk and for chained or piped shell commands. */
export function canAlways(a: Approval): boolean {
  return a.kind === "permission" && (a.alwaysOk ?? a.risk !== "high");
}

/** Approval ids whose command is being edited in a card; shortcuts must not approve the original. */

/** High risk and plans need a second press of the same key; the first press only explains. */
export function needsSecond(a: Approval, key: string): boolean {
  if (a.risk !== "high" && a.kind !== "plan") return false;
  if (confirmKey(a.id, key)) return false;
  showToast(a.kind === "plan" ? "Approve this plan?" : "High-risk request", `Press ${key} again within 4 seconds to ${a.kind === "plan" ? "approve the plan" : "allow it"}, or use the button.`);
  return true;
}

/** Keyboard path: allow or always-allow without the card. Questions need the card. */
export async function quickDecide(a: Approval, how: "allow" | "always") {
  if (sentToTerminal(a)) return showToast("Sent to the terminal", "Waiting for the terminal to confirm.");
  if (a.kind === "question") return showToast("Pick an answer", "Questions need an option or a typed answer.");
  if (how === "always" && !canAlways(a)) return showToast("No always here", "High-risk, chained or piped commands, questions and plans are answered once.");
  const b: DecideBody = a.kind === "plan" ? { behavior: "allow", updatedInput: a.input } : { behavior: "allow", always: how === "always" || undefined };
  try {
    const r = await v3.decide(a.id, b);
    applyDecision(r && r.id ? r : { ...a, state: "allowed" });
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) showToast("Already answered", "Another surface or the terminal answered first.");
    else showToast("Decision failed", (e as Error).message, { kind: "alert" });
    void loadApprovals();
  }
}

/** "Allow these N": pending requests that one Always rule would cover; the list shows before anything is sent. */
export function AllowGroup({ a }: { a: Approval }) {
  const { approvals } = useV3();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  if (!a.group || a.state !== "pending") return null;
  const same = [...approvals.values()].filter((x) => x.group === a.group && x.state === "pending" && x.risk !== "high" && !sentToTerminal(x));
  if (same.length < 2) return null;
  const all = async () => {
    setBusy(true);
    let failed = 0;
    for (const x of same) {
      try {
        const r = await v3.decide(x.id, { behavior: "allow" });
        applyDecision(r && r.id ? r : { ...x, state: "allowed" });
      } catch {
        failed++;
      }
    }
    setBusy(false);
    setOpen(false);
    if (failed) showToast(`${same.length - failed} allowed, ${failed} not`, "Another surface may have answered first.", { kind: "alert" });
    else showToast(`Allowed ${same.length} requests`, a.groupPattern || a.toolName);
    void loadApprovals();
  };
  if (!open)
    return (
      <button className="btn sm" onClick={() => setOpen(true)} aria-expanded={false}>
        Allow these {same.length}…
      </button>
    );
  return (
    <div className="agroup" role="group" aria-label={`${same.length} similar requests`}>
      <div className="xs muted">
        {same.length} requests match <code>{a.groupPattern || a.toolName}</code> in {shortCwd(a.cwd)}. Each one is allowed once; no rule is added.
      </div>
      <ul>
        {same.map((x) => (
          <li key={x.id}>
            <code>{x.summary}</code>
          </li>
        ))}
      </ul>
      <div className="row" style={{ gap: 6 }}>
        <button className="btn sm pri" disabled={busy} onClick={() => void all()}>
          Allow all {same.length}
        </button>
        <button className="btn sm ghost" disabled={busy} onClick={() => setOpen(false)}>
          Cancel
        </button>
      </div>
    </div>
  );
}

/** Lets a keyboard handler open the deny box of a card it does not render. */
export const DenyTarget = createContext<{ id: string | null; clear: () => void }>({ id: null, clear: () => {} });



/** Old MCP permission request (/api/permissions), answered through the legacy endpoint. */
export function LegacyPermCard({ p, compact }: { p: PermissionRequest; compact?: boolean }) {
  const [busy, setBusy] = useState(false);
  const one = str(p.input.command || p.input.file_path || p.input.url || p.input.query) || JSON.stringify(p.input, null, 2);
  const go = async (b: "allow" | "deny") => {
    setBusy(true);
    try {
      await api.respondPerm(p.id, b);
      dropPerm(p.id);
    } catch (e) {
      showToast("Respond failed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className={`appr${compact ? " compact" : ""}`}>
      <div className="why">
        <span className="risk md">MCP prompt</span>
        <span>{p.toolName}</span>
        {p.cwd && <span className="muted">in {shortCwd(p.cwd)}</span>}
      </div>
      <pre>{one}</pre>
      <div className="acts">
        <button className="btn allow" disabled={busy} onClick={() => go("allow")}>
          Allow
        </button>
        <button className="btn deny" disabled={busy} onClick={() => go("deny")}>
          Deny
        </button>
      </div>
    </div>
  );
}

/** A session that shows a permission prompt in its terminal, with no agent-monitor request; it reads GET /api/needs-you. */
export function PaneAnswerCard({ s, onOpenPane, compact }: { s: Session; onOpenPane: () => void; compact?: boolean }) {
  const ny = useNeedsYou();
  const item = ny.items.find((i) => i.kind === "prompt" && i.sessionId === s.id);
  if (ny.status !== "loading" && !item) return null;
  return <PromptCard item={item || { kind: "prompt", key: `tp:${s.id}`, sessionId: s.id, agent: s.tool, at: s.lastActivityAt, answerable: false, message: s.permissionMessage || s.lastMessage }} onOpenPane={onOpenPane} compact={compact} />;
}

const PROMPT_ANSWERS: { label: string; answer: "yes" | "no"; cls: string }[] = [
  { label: "Yes", answer: "yes", cls: "allow" },
  { label: "No", answer: "no", cls: "deny" },
];

/** A needs-you item of kind "prompt": the agent's question, answered by keys sent to its terminal. */
export function PromptCard({ item, onOpenPane, compact, onSent }: { item: NeedsYouItem; onOpenPane?: () => void; compact?: boolean; onSent?: (r: "ok" | string) => void }) {
  const [busy, setBusy] = useState(false);
  const sid = item.sessionId || "";
  const go = async (answer: "yes" | "no") => {
    if (!item.answerable || !sid) return;
    setBusy(true);
    try {
      await api.answerPrompt(sid, answer, item.promptKey);
      dropNeedsYou(item.key);
      onSent?.("ok");
    } catch (e) {
      if (onSent) onSent((e as Error).message);
      else showToast("Not sent", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
      void refreshNeedsYou();
    }
  };
  const dismiss = async () => {
    if (!sid) return;
    setBusy(true);
    try {
      await api.answerPrompt(sid, "dismiss");
      dropNeedsYou(item.key);
    } catch (e) {
      showToast("Not dismissed", (e as Error).message, { kind: "alert" });
    } finally {
      setBusy(false);
      void refreshNeedsYou();
    }
  };
  return (
    <div className={`appr${compact ? " compact" : ""}`}>
      <div className="why">
        <span className="risk md">Needs you</span>
        <span>{agentName(item.agent || "claude")} asks in the terminal</span>
      </div>
      {item.message && <pre>{item.message.slice(0, 600)}</pre>}
      {!item.answerable && <div className="muted xs">{item.why || "Answer it in the terminal."}</div>}
      <div className="acts">
        {PROMPT_ANSWERS.map((k) => (
          <button key={k.label} className={`btn ${k.cls}`} disabled={busy || !item.answerable} onClick={() => void go(k.answer)}>
            {k.label}
          </button>
        ))}
        {onOpenPane && (
          <button className="btn ghost" onClick={onOpenPane}>
            Open terminal
          </button>
        )}
        {!item.answerable && (
          <button className="btn ghost" disabled={busy} onClick={() => void dismiss()}>
            Dismiss
          </button>
        )}
      </div>
    </div>
  );
}
