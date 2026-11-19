/** Built-in slash commands the terminal agents accept; Insert puts "/name " in the box. */
// Only commands that run without a full-screen dialog: a dialog cannot be driven from a phone.
export const CLAUDE_COMMANDS: [string, string][] = [
  ["compact", "Compact the conversation"], ["clear", "Clear the conversation"], ["context", "Show context use"],
  ["cost", "Show the session cost"], ["usage", "Show plan usage"], ["review", "Review a pull request"],
  ["init", "Write a CLAUDE.md for this repo"], ["todos", "Show the todo list"], ["exit", "Exit the session"],
];
export const CODEX_COMMANDS: [string, string][] = [
  ["compact", "Compact the conversation"], ["new", "Start a new chat"], ["diff", "Show the git diff"],
  ["init", "Write an AGENTS.md"], ["exit", "Exit the session"],
];
export const agentCommands = (agent: string) => (agent === "codex" ? CODEX_COMMANDS : agent === "claude" ? CLAUDE_COMMANDS : []);
