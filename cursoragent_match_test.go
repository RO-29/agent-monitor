package main

import (
	"strings"
	"testing"
)

func TestCursorAgentMatchesOnlyRealRuns(t *testing.T) {
	cases := map[string]bool{
		"/Users/u/.local/bin/cursor-agent --use-system-ca /Users/u/.local/share/cursor-agent/versions/1/index.js -p --trust --mode ask fix the tests": true,
		"cursor-agent": true,
		"/Users/u/.local/bin/cursor-agent --use-system-ca /Users/u/.local/share/cursor-agent/versions/1/index.js --version":                          false,
		"bash /Users/u/.local/bin/cursor-agent --version":                                                                                            false,
		"/Users/u/.local/share/cursor-agent/versions/1/node /Users/u/.local/share/cursor-agent/versions/1/index.js worker-server":                    false,
		"/bin/zsh -c source snap.sh && cursor-agent -p hello":                                                                                        false,
		"/Users/u/Library/Application Support/Cursor/User/globalStorage/anysphere.cursor-agent-worker/agent-cli/.local/bin/cursor-agent --api-key x": false,
		"/Users/u/.local/bin/cursor-agent acp":                                                                                                       false,
		"/Users/u/.local/bin/cursor-agent status":                                                                                                    false,
		"/Users/u/.local/bin/cursor-agent -p update the README":                                                                                      true,
		"grep cursor-agent": false,
	}
	for cmd, want := range cases {
		if got := cursorAgentMatches(cmd); got != want {
			t.Errorf("cursorAgentMatches(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestRedactCmdSecrets(t *testing.T) {
	in := "cursor-agent --api-key crsr_abcdef0123456789 --token=zzz --model x --auth-token yyy run sk-ant0123456789abcd"
	out := redactCmdSecrets(in)
	for _, leak := range []string{"crsr_abcdef", "zzz", "yyy", "sk-ant0123"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q still in %q", leak, out)
		}
	}
	if !strings.Contains(out, "--model x") || !strings.Contains(out, "--api-key <redacted>") {
		t.Fatalf("unexpected: %q", out)
	}
}
