import { useEffect } from "react";
import { Icon } from "../../lib/icons";

const MCP_JSON = `{
  "mcpServers": {
    "agent-monitor": {
      "command": "/path/to/agent-monitor",
      "args": ["mcp-perm-server"]
    }
  }
}`;

export default function HelpOverlay({ onClose }: { onClose: () => void }) {
  useEffect(() => {
    const k = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, [onClose]);
  return (
    <div className="overlay" onClick={onClose}>
      <div className="panel" onClick={(e) => e.stopPropagation()}>
        <div className="row" style={{ justifyContent: "space-between" }}>
          <h2>agent-monitor · help</h2>
          <button className="btn icon" onClick={onClose} aria-label="close">
            <Icon name="x" />
          </button>
        </div>
        <h3>Answer permission prompts from the browser</h3>
        <p className="t2" style={{ margin: "4px 0", fontSize: 12 }}>
          Register the daemon as an MCP server (<span className="mono">agent-monitor install</span> writes this), then start Claude Code with the permission tool flag:
        </p>
        <pre className="code">{MCP_JSON}</pre>
        <pre className="code" style={{ marginTop: 6 }}>claude --permission-prompt-tool mcp__agent-monitor__permission_prompt</pre>
        <h3>Per-tool support</h3>
        <table>
          <thead>
            <tr>
              <th>Tool</th>
              <th>Sessions</th>
              <th>Trace · chapters</th>
              <th>Pane bridge</th>
              <th>Permissions</th>
            </tr>
          </thead>
          <tbody>
            <tr><td>Claude Code</td><td>full (hooks + transcript tail)</td><td>full (compact, clear, subagents, cost)</td><td>tmux</td><td>MCP + pane</td></tr>
            <tr><td>Codex</td><td>full (rollout tail)</td><td>full (compacted, turn_context, token_count)</td><td>tmux</td><td>pane watcher</td></tr>
            <tr><td>Cursor IDE</td><td>approximate (chat store walk)</td><td>—</td><td>—</td><td>—</td></tr>
            <tr><td>cursor-agent</td><td>live process only</td><td>—</td><td>tmux</td><td>pane watcher</td></tr>
            <tr><td>opencode</td><td>full (storage)</td><td>—</td><td>tmux</td><td>pane watcher</td></tr>
          </tbody>
        </table>
        <h3>Vocabulary</h3>
        <p className="t2" style={{ fontSize: 12, margin: "4px 0" }}>
          <b>Thread</b> = sessions linked by an explicit continuation (/clear, resume, shared handoff file). <b>Segment</b> = the part of a session between two boundaries (start · compact · clear · resume).
          <b> Chapter</b> = the card for one segment: the point, intent changes, outcome, learnings, open items, outputs. <b>Span</b> = one row on the trace: a prompt, a turn, a tool call, or a subagent run.
        </p>
        <h3>Notifications</h3>
        <ol className="t2" style={{ fontSize: 12, margin: "4px 0", paddingLeft: 18 }}>
          <li>Press Notify in the rail; the browser asks for permission once.</li>
          <li>If the browser says "denied": macOS System Settings → Notifications → your browser → Allow, then reload.</li>
          <li>Chrome: click the lock icon in the address bar → Notifications → Allow.</li>
          <li>Sound needs one click on the page first (browser autoplay rule).</li>
        </ol>
        <h3>Daemon</h3>
        <pre className="code">{`agent-monitor            # foreground daemon on :7777
agent-monitor stop       # stop ONLY the daemon (keeps MCP children alive)
agent-monitor restart
agent-monitor run claude # launch an agent in tmux with a registered pane
AGENT_MONITOR_BIND=<tailscale-ip> agent-monitor   # expose over the tailnet`}</pre>
        <h3>Keys</h3>
        <p className="t2" style={{ fontSize: 12, margin: "4px 0" }}>
          <span className="kbd">/</span> search · <span className="kbd">Esc</span> close / back · <span className="kbd">?</span> this help
        </p>
      </div>
    </div>
  );
}
