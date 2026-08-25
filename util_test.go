package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func mustMkHookScript(t *testing.T, repo string) {
	t.Helper()
	bin := filepath.Join(repo, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude-hook.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestIsRepoRoot(t *testing.T) {
	tmp := t.TempDir()
	if isRepoRoot(tmp) {
		t.Fatalf("%s shouldn't look like a repo root before bin/claude-hook.sh exists", tmp)
	}
	mustMkHookScript(t, tmp)
	if !isRepoRoot(tmp) {
		t.Fatalf("%s should look like a repo root once bin/claude-hook.sh exists", tmp)
	}
}

func TestResolveRepoDir(t *testing.T) {
	repoRoot := t.TempDir()
	mustMkHookScript(t, repoRoot)

	notARepo := t.TempDir()

	envRepo := t.TempDir()
	mustMkHookScript(t, envRepo)

	envInvalid := t.TempDir()

	cases := []struct {
		name       string
		env        string
		candidates []string
		want       string
		wantErr    bool
	}{
		{
			name:       "binary built at repo root: first candidate validates",
			candidates: []string{repoRoot, filepath.Dir(repoRoot)},
			want:       repoRoot,
		},
		{
			name:       "binary built to <repo>/bin: parent candidate validates",
			candidates: []string{filepath.Join(repoRoot, "bin"), repoRoot},
			want:       repoRoot,
		},
		{
			name:       "go run temp dir and installed-binary dir both fail: falls back to HOME candidate",
			candidates: []string{notARepo, filepath.Dir(notARepo), repoRoot},
			want:       repoRoot,
		},
		{
			name:       "no candidate validates: clear error, no silent wrong guess",
			candidates: []string{notARepo, filepath.Dir(notARepo)},
			wantErr:    true,
		},
		{
			name:       "AGENT_MONITOR_REPO override wins over candidates",
			env:        envRepo,
			candidates: []string{repoRoot},
			want:       envRepo,
		},
		{
			name:       "AGENT_MONITOR_REPO override set but invalid: error names the env var",
			env:        envInvalid,
			candidates: []string{repoRoot},
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRepoDir(tc.env, tc.candidates)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dir %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// resolveRepoDir is table-tested above against explicit candidates; this
// covers repoDir itself — i.e. that the candidates it assembles actually
// include the working directory, which is the half of the fix that stops a
// stale ~/agent-monitor from winning.
func TestRepoDirUsesWorkingDirectory(t *testing.T) {
	realRepo := realPath(t, t.TempDir())
	mustMkHookScript(t, realRepo)

	// A stale checkout at the old hardcoded location, to lose to cwd.
	fakeHome := t.TempDir()
	staleRepo := filepath.Join(fakeHome, "agent-monitor")
	mustMkHookScript(t, staleRepo)

	t.Setenv("HOME", fakeHome)
	t.Setenv("AGENT_MONITOR_REPO", "")

	t.Run("cwd is the checkout", func(t *testing.T) {
		t.Chdir(realRepo)
		got, err := repoDir()
		if err != nil {
			t.Fatalf("repoDir: %v", err)
		}
		if realPath(t, got) != realRepo {
			t.Fatalf("got %q, want %q — the HOME fallback shouldn't beat cwd", got, realRepo)
		}
	})

	t.Run("cwd is a subdirectory of the checkout", func(t *testing.T) {
		sub := filepath.Join(realRepo, "web", "static")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		got, err := repoDir()
		if err != nil {
			t.Fatalf("repoDir: %v", err)
		}
		if realPath(t, got) != realRepo {
			t.Fatalf("got %q, want %q — should walk up to the checkout root", got, realRepo)
		}
	})

	t.Run("outside any checkout: falls back to HOME", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := repoDir()
		if err != nil {
			t.Fatalf("repoDir: %v", err)
		}
		if realPath(t, got) != realPath(t, staleRepo) {
			t.Fatalf("got %q, want the HOME fallback %q", got, staleRepo)
		}
	})

	t.Run("AGENT_MONITOR_REPO overrides cwd", func(t *testing.T) {
		t.Chdir(realRepo)
		t.Setenv("AGENT_MONITOR_REPO", staleRepo)
		got, err := repoDir()
		if err != nil {
			t.Fatalf("repoDir: %v", err)
		}
		if realPath(t, got) != realPath(t, staleRepo) {
			t.Fatalf("got %q, want the override %q", got, staleRepo)
		}
	})
}

// realPath resolves symlinks so comparisons survive macOS's /var -> /private/var.
func realPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}

func TestIsEphemeralExe(t *testing.T) {
	goRun := filepath.Join(os.TempDir(), "go-build511827956", "b001", "exe", "agent-monitor")
	if !isEphemeralExe(goRun) {
		t.Fatalf("%q is a go run build artifact and should be treated as ephemeral", goRun)
	}
	for _, real := range []string{
		"/usr/local/bin/agent-monitor",
		"/Users/rohit/agent-monitor/agent-monitor",
		// Directories that merely start with the same letters are not Go's.
		"/Users/rohit/src/go-buildtools/agent-monitor",
		"/Users/rohit/go-build-helpers/bin/agent-monitor",
	} {
		if isEphemeralExe(real) {
			t.Fatalf("%q is a durable install path, not ephemeral", real)
		}
	}
	// Both of Go's own shapes: the cache root and a `go run` temp tree.
	for _, seg := range []string{"go-build", "go-build511827956"} {
		if !isGoBuildDir(seg) {
			t.Fatalf("%q is one of Go's build directories", seg)
		}
	}
}

func TestResolveExeBin(t *testing.T) {
	repoRoot := t.TempDir()
	mustMkHookScript(t, repoRoot)
	builtBin := filepath.Join(repoRoot, "agent-monitor")

	okRepo := func() (string, error) { return repoRoot, nil }
	noRepo := func() (string, error) { return "", errNoRepo }

	installed := "/usr/local/bin/agent-monitor"
	if got, err := resolveExeBin(installed, okRepo); err != nil || got != installed {
		t.Fatalf("durable exe path: got (%q, %v), want (%q, nil)", got, err, installed)
	}

	// `go run` builds to a temp dir that's deleted the moment install exits.
	// Persisting it would leave every later session spawning a missing file,
	// and a failed MCP spawn is silent — so refuse rather than write it.
	goRun := filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "agent-monitor")
	if got, err := resolveExeBin(goRun, okRepo); err == nil {
		t.Fatalf("go run exe with no built binary in the checkout: got %q, want error", got)
	}

	if err := os.WriteFile(builtBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveExeBin(goRun, okRepo); err != nil || got != builtBin {
		t.Fatalf("go run exe with a built binary: got (%q, %v), want (%q, nil)", got, err, builtBin)
	}

	if got, err := resolveExeBin(goRun, noRepo); err == nil {
		t.Fatalf("go run exe and no locatable checkout: got %q, want error", got)
	}
}

var errNoRepo = fmt.Errorf("no checkout found")

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"/a", "/b", "/a", "/c", "/b"})
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (order must be preserved)", got, want)
		}
	}
}
