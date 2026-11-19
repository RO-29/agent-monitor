import type { Approval } from "../api/types";

/** True while a card decision was typed into the terminal and waits for the terminal to confirm it. */
export const sentToTerminal = (a?: Approval) => !!a && a.source === "terminal-shown" && !!a.sentAt;

/** One line under a terminal-shown approval: where else it shows, or that the card answer went there. */
export function TermShownNote({ a }: { a?: Approval }) {
  if (!a || a.source !== "terminal-shown") return null;
  if (a.sentAt) {
    return (
      <div className="termshown sent" role="status">
        <span className="spin-dot" aria-hidden /> Sent to the terminal
      </div>
    );
  }
  return <div className="termshown">Also shown in the terminal</div>;
}
