package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResumePidsFindsOnlyTheAgentResumingTheSession(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	old := psCommandsFn
	defer func() { psCommandsFn = old }()
	psCommandsFn = func() string {
		return "  10 /x/am-helper run claude --resume " + id + "\n" +
			"  11 /x/.local/bin/claude --resume " + id + "\n" +
			"  12 /x/.local/bin/claude --resume 99999999-2222-3333-4444-555555555555\n" +
			"  13 /bin/zsh -c claude --resume " + id + "\n" +
			"  14 codex resume " + id + "\n" +
			"  15 /x/claude --resume " + id + " --fork-session\n" +
			"  16 /x/claude --resume=" + id + "\n"
	}
	if got := resumePids(ToolClaude, id); len(got) != 2 || got[0] != 11 || got[1] != 16 {
		t.Fatalf("claude: %v", got)
	}
	if got := resumePids(ToolCodex, id); len(got) != 1 || got[0] != 14 {
		t.Fatalf("codex: %v", got)
	}
}

func TestTranscriptPermissionModeUsesTheLastMode(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	auto := write("a.jsonl", `{"permissionMode":"default"}`+"\n"+`{"permissionMode":"auto"}`+"\n")
	def := write("b.jsonl", `{"permissionMode":"auto"}`+"\n"+`{"permissionMode":"default"}`+"\n")
	bypass := write("c.jsonl", `{"permissionMode":"bypassPermissions"}`+"\n")
	if transcriptPermissionMode(auto) != "auto" || transcriptPermissionMode(def) != "" || transcriptPermissionMode(bypass) != "" || transcriptPermissionMode(filepath.Join(dir, "none")) != "" {
		t.Fatal("wrong mode")
	}
}
