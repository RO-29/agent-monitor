package main

import (
	"slices"
	"strings"
	"testing"
)

const (
	newPath   = "/Users/rohit/Desktop/repositories/agent-monitor/bin/claude-hook.sh"
	stalePath = "/Users/rohit/agent-monitor/bin/claude-hook.sh"
	otherPath = "/Users/rohit/staging-deploy-hl-au/dr_learning_hook.sh"
)

func hookEntry(cmd string, timeout int) map[string]any {
	return map[string]any{"type": "command", "command": cmd, "timeout": timeout}
}

func hookBucket(matcher string, entries ...any) map[string]any {
	return map[string]any{"matcher": matcher, "hooks": append([]any{}, entries...)}
}

// commandsIn flattens the commands registered across every bucket, in order,
// so a test can assert on what actually fires for the event rather than on
// which bucket happens to hold it.
func commandsIn(buckets []any) []string {
	var out []string
	for _, b := range buckets {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		hl, _ := bm["hooks"].([]any)
		for _, h := range hl {
			if hm, ok := h.(map[string]any); ok {
				c, _ := hm["command"].(string)
				out = append(out, c)
			}
		}
	}
	return out
}

func TestMergeHookBuckets(t *testing.T) {
	cases := []struct {
		name        string
		buckets     []any
		wantStatus  string
		wantCmds    []string
		wantBuckets int
		wantTimeout int // expected timeout on our entry, 0 to skip the check
	}{
		{
			name:        "no buckets at all: creates one",
			wantStatus:  "added",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
			wantTimeout: 2000,
		},
		{
			name:        "already correct: untouched",
			buckets:     []any{hookBucket("", hookEntry(newPath, 2000))},
			wantStatus:  "skipped",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
		},
		{
			name:        "stale checkout path: repointed, tuned timeout preserved",
			buckets:     []any{hookBucket("", hookEntry(stalePath, 5000))},
			wantStatus:  "replaced",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
			wantTimeout: 5000,
		},
		{
			name:        "unrelated hook: preserved, ours appended",
			buckets:     []any{hookBucket("", hookEntry(otherPath, 1000))},
			wantStatus:  "added",
			wantCmds:    []string{otherPath, newPath},
			wantBuckets: 1,
		},
		{
			name:        "stale and current in one bucket: stale dropped",
			buckets:     []any{hookBucket("", hookEntry(stalePath, 2000), hookEntry(newPath, 2000))},
			wantStatus:  "replaced",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
		},
		{
			// The shape that actually occurs: other tools append their own
			// bucket, so an event ends up with several. Sweeping only the
			// first would leave the stale entry firing at a deleted script.
			name: "stale in a second always-match bucket: found and consolidated",
			buckets: []any{
				hookBucket("", hookEntry(otherPath, 1000)),
				hookBucket("", hookEntry(stalePath, 2000)),
			},
			wantStatus:  "replaced",
			wantCmds:    []string{otherPath, newPath},
			wantBuckets: 1,
		},
		{
			// "" and "*" mean the same thing for these matcher-less events,
			// so an entry parked in a "*" bucket must not be duplicated.
			name:        "stale in a \"*\" bucket: repointed in place, not duplicated",
			buckets:     []any{hookBucket("*", hookEntry(stalePath, 2000))},
			wantStatus:  "replaced",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
		},
		{
			// A "*" bucket that also holds someone else's hook survives the
			// sweep, so if we didn't recognize it as always-match we'd add a
			// second, redundant bucket beside it — and another on the next
			// reinstall. Behaviourally equivalent, but the file grows.
			name: "populated \"*\" bucket is reused, not shadowed by a new one",
			buckets: []any{
				hookBucket("*", hookEntry(otherPath, 1000), hookEntry(stalePath, 2000)),
			},
			wantStatus:  "replaced",
			wantCmds:    []string{otherPath, newPath},
			wantBuckets: 1,
		},
		{
			name: "two stale entries across two buckets: collapsed to one",
			buckets: []any{
				hookBucket("", hookEntry(stalePath, 2000)),
				hookBucket("*", hookEntry("/tmp/other-checkout/bin/claude-hook.sh", 2000)),
			},
			wantStatus:  "replaced",
			wantCmds:    []string{newPath},
			wantBuckets: 1, // the emptied "*" bucket is pruned
		},
		{
			name:        "ours in a matcher'd bucket: moved to the always-match one",
			buckets:     []any{hookBucket("Edit|Write", hookEntry(stalePath, 2000))},
			wantStatus:  "replaced",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
		},
		{
			name: "unrelated bucket with a matcher is left alone",
			buckets: []any{
				hookBucket("Edit|Write", hookEntry(otherPath, 1000)),
				hookBucket("", hookEntry(stalePath, 2000)),
			},
			wantStatus:  "replaced",
			wantCmds:    []string{otherPath, newPath},
			wantBuckets: 2,
		},
		{
			name:        "same basename outside bin/: not ours",
			buckets:     []any{hookBucket("", hookEntry("/Users/rohit/scripts/claude-hook.sh", 1000))},
			wantStatus:  "added",
			wantCmds:    []string{"/Users/rohit/scripts/claude-hook.sh", newPath},
			wantBuckets: 1,
		},
		{
			name:        "garbage entries pass through",
			buckets:     []any{hookBucket("", "not-a-map")},
			wantStatus:  "added",
			wantCmds:    []string{newPath},
			wantBuckets: 1,
		},
		{
			name:        "garbage bucket passes through",
			buckets:     []any{"not-a-bucket"},
			wantStatus:  "added",
			wantCmds:    []string{newPath},
			wantBuckets: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, status := mergeHookBuckets(tc.buckets, newPath)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if len(got) != tc.wantBuckets {
				t.Errorf("buckets = %d, want %d (%#v)", len(got), tc.wantBuckets, got)
			}
			cmds := commandsIn(got)
			if strings.Join(cmds, "\n") != strings.Join(tc.wantCmds, "\n") {
				t.Fatalf("commands = %q, want %q", cmds, tc.wantCmds)
			}
			// Whatever the shape, exactly one of ours must be registered.
			ours := 0
			for _, c := range cmds {
				if isOurHookCommand(c) && c == newPath {
					ours++
				}
			}
			if ours != 1 {
				t.Fatalf("%d agent-monitor entries registered, want exactly 1: %q", ours, cmds)
			}
			if tc.wantTimeout != 0 {
				for _, b := range got {
					bm := b.(map[string]any)
					for _, h := range bm["hooks"].([]any) {
						hm, ok := h.(map[string]any)
						if !ok {
							continue
						}
						if c, _ := hm["command"].(string); c == newPath {
							if to, _ := hm["timeout"].(int); to != tc.wantTimeout {
								t.Fatalf("timeout = %v, want %d", hm["timeout"], tc.wantTimeout)
							}
						}
					}
				}
			}
		})
	}
}

// Re-running install must not keep mutating the file.
func TestMergeHookBucketsIdempotent(t *testing.T) {
	buckets := []any{
		hookBucket("", hookEntry(otherPath, 1000)),
		hookBucket("*", hookEntry(stalePath, 2000)),
	}
	got, status := mergeHookBuckets(buckets, newPath)
	if status != "replaced" {
		t.Fatalf("first pass status = %q, want replaced", status)
	}
	first := strings.Join(commandsIn(got), "\n")
	for i := range 3 {
		got, status = mergeHookBuckets(got, newPath)
		if status != "skipped" {
			t.Fatalf("pass %d status = %q, want skipped", i+2, status)
		}
		if now := strings.Join(commandsIn(got), "\n"); now != first {
			t.Fatalf("pass %d changed the result:\n got %q\nwant %q", i+2, now, first)
		}
	}
}

func TestUpdateTOMLCommand(t *testing.T) {
	header := "[mcp_servers.agent-monitor]"

	cases := []struct {
		name        string
		src         string
		newCmd      string
		wantChanged bool
		wantLines   []string // lines that must survive verbatim in the output
	}{
		{
			name: "stale command line rewritten",
			src: "[mcp_servers.other]\ncommand = \"/other\"\n\n" +
				header + "\ncommand = \"/old/agent-monitor\"\nargs    = [\"mcp-perm-server\"]\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
		},
		{
			name:        "already up to date: no change reported",
			src:         header + "\ncommand = \"/new/agent-monitor\"\nargs    = [\"mcp-perm-server\"]\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: false,
		},
		{
			name: "command line in a different section is left alone",
			src: header + "\ncommand = \"/new/agent-monitor\"\n\n" +
				"[mcp_servers.other]\ncommand = \"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: false,
			wantLines:   []string{`command = "/old/agent-monitor"`},
		},
		{
			// A prefix match would rewrite this key too, leaving two
			// `command =` lines — a duplicate key is a hard TOML parse
			// error, so the whole config (every MCP server) stops loading.
			name:        "neighbouring key with a command prefix is untouched",
			src:         header + "\ncommand = \"/old/agent-monitor\"\ncommand_timeout = 30\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{"command_timeout = 30"},
		},
		{
			// Contains(header) gates the caller, so a header with a trailing
			// comment used to report "already registered" and silently keep
			// the dead path.
			name:        "header with trailing comment still matches",
			src:         header + " # agent-monitor\ncommand = \"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
		},
		{
			name:        "no-space assignment is normalized",
			src:         header + "\ncommand=\"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
		},
		{
			// An array element on its own line starts with "[" but is not a
			// section header. Treating it as one used to end the block early,
			// so the command below was never seen and install reported
			// "already registered" while leaving the dead path in place.
			name: "multi-line array before command doesn't end the section",
			src: header + "\nenv_pairs = [\n  [\"A\", \"1\"],\n  [\"B\", \"2\"],\n]\n" +
				"command = \"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{`  ["A", "1"],`, `command = "/new/agent-monitor"`},
		},
		{
			name:        "multi-line inline table before command",
			src:         header + "\nenv = {\n  A = \"1\",\n}\ncommand = \"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{`command = "/new/agent-monitor"`},
		},
		{
			// A bracket inside a quoted value must not open a continuation.
			name:        "brackets inside a string are not structure",
			src:         header + "\nnote = \"see [docs] # here\"\ncommand = \"/old/agent-monitor\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{`note = "see [docs] # here"`, `command = "/new/agent-monitor"`},
		},
		{
			// Reporting "already registered" on a section with no command
			// leaves codex a server it can't spawn.
			name:        "section without a command key gets one inserted",
			src:         header + "\nargs    = [\"mcp-perm-server\"]\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{header, `command = "/new/agent-monitor"`, `args    = ["mcp-perm-server"]`},
		},
		{
			name:        "missing command is inserted into our section, not another",
			src:         "[mcp_servers.other]\ncommand = \"/opt/other\"\n\n" + header + "\nargs = []\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: true,
			wantLines:   []string{`command = "/opt/other"`, `command = "/new/agent-monitor"`},
		},
		{
			name:        "no section at all: nothing inserted",
			src:         "[mcp_servers.other]\ncommand = \"/opt/other\"\n",
			newCmd:      `command = "/new/agent-monitor"`,
			wantChanged: false,
			wantLines:   []string{`command = "/opt/other"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := updateTOMLCommand(tc.src, header, tc.newCmd)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (out=%q)", changed, tc.wantChanged, out)
			}
			for _, want := range tc.wantLines {
				if !containsLine(out, want) {
					t.Fatalf("line %q didn't survive: %q", want, out)
				}
			}
		})
	}
}

func containsLine(s, line string) bool {
	return slices.Contains(strings.Split(s, "\n"), line)
}
