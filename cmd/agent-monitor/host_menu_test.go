package main

import (
	"os"
	"strings"
	"testing"
)

func readScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/screens/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStartMenuMCPDialog(t *testing.T) {
	p := detectStartPrompt(readScreen(t, "claude-mcp-start.txt"), "claude")
	if p == nil {
		t.Fatal("MCP start dialog not found")
	}
	if p.Kind != "mcp" || !strings.HasPrefix(p.Title, "New MCP server found") || !strings.Contains(p.Text, "require approval") {
		t.Fatalf("kind %q title %q text %q", p.Kind, p.Title, p.Text)
	}
	ids := []string{}
	for _, o := range p.Options {
		ids = append(ids, o.ID)
	}
	if strings.Join(ids, ",") != "option1,option2,deny" || p.cursor != 2 {
		t.Fatalf("options %v cursor %d", ids, p.cursor)
	}
	if p.Options[1].Label != "Use this and all future MCP servers in this project" {
		t.Fatalf("label %q", p.Options[1].Label)
	}
}

func TestStartMenuIgnoresNormalScreens(t *testing.T) {
	for _, f := range []string{"claude-idle.txt", "claude-slash.txt", "codex-idle.txt", "codex-draft.txt"} {
		if p := detectStartPrompt(readScreen(t, f), ""); p != nil {
			t.Errorf("%s: unexpected start menu %+v", f, p)
		}
	}
}

func TestStartMenuKeepsTrustDialog(t *testing.T) {
	p := detectStartPrompt(sprintf(claudeTrustScreen, "❯", " "), "claude")
	if p == nil || p.Kind != "trust" {
		t.Fatalf("trust dialog: %+v", p)
	}
}
