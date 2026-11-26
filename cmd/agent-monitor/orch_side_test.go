package main

import (
	"strings"
	"testing"
	"time"
)

func TestOrchSideQuestionStartsAReadOnlyChild(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()

	mode, err := e.m.Send(th.ID, "/side what does the retry do?")
	if err != nil || mode != "side" {
		t.Fatalf("Send = %q, %v", mode, err)
	}
	var prompt string
	var kid OThread
	for i := 0; i < 50 && prompt == ""; i++ {
		kids, _ := e.m.store.Subtree(th.ID)
		if len(kids) == 1 {
			kid = kids[0]
			if d, err := e.m.store.ThreadDetail(kid.ID); err == nil && len(d.Runs) > 0 {
				prompt = d.Runs[0].Prompt
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if kid.Title != "Side: what does the retry do?" || !threadReadOnly(&kid) {
		t.Fatalf("child = %+v", kid)
	}
	for _, want := range []string{"User: hello", "Side question: what does the retry do?", "change nothing"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q lacks %q", prompt, want)
		}
	}
}

func TestSideCommand(t *testing.T) {
	for in, want := range map[string]string{"/side why?": "why?", "  /side  x ": "x", "/side": "", "/sidebar x": "", "ask /side x": ""} {
		if got, _ := sideCommand(in); got != want {
			t.Errorf("sideCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
