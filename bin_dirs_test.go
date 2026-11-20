package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nvmHome(t *testing.T, versions []string, aliases map[string]string) string {
	t.Helper()
	h := t.TempDir()
	for _, v := range versions {
		if err := os.MkdirAll(filepath.Join(h, ".nvm", "versions", "node", v, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range aliases {
		p := filepath.Join(h, ".nvm", "alias", filepath.FromSlash(k))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(v+"\n"), 0o644)
	}
	return h
}

func TestNvmDefaultBin(t *testing.T) {
	vs := []string{"v9.11.2", "v22.22.2", "v22.3.0", "v20.19.0"}
	bin := func(h, v string) string { return filepath.Join(h, ".nvm", "versions", "node", v, "bin") }
	h := nvmHome(t, vs, map[string]string{"default": "v20.19.0"})
	if got := nvmDefaultBin(h); got != bin(h, "v20.19.0") {
		t.Fatalf("alias to a version: %s", got)
	}
	h = nvmHome(t, vs, map[string]string{"default": "lts/*", "lts/*": "lts/iron", "lts/iron": "v22.3.0"})
	if got := nvmDefaultBin(h); got != bin(h, "v22.3.0") {
		t.Fatalf("alias chain: %s", got)
	}
	h = nvmHome(t, vs, map[string]string{"default": "22"})
	if got := nvmDefaultBin(h); got != bin(h, "v22.22.2") {
		t.Fatalf("major alias picks newest 22.x numerically: %s", got)
	}
	h = nvmHome(t, vs, nil)
	if got := nvmDefaultBin(h); got != bin(h, "v22.22.2") {
		t.Fatalf("no alias, newest numerically: %s", got)
	}
	h = nvmHome(t, vs, map[string]string{"default": "node"})
	if got := nvmDefaultBin(h); got != bin(h, "v22.22.2") {
		t.Fatalf("node alias: %s", got)
	}
	if nvmDefaultBin(t.TempDir()) != "" {
		t.Fatal("no nvm")
	}
}

func TestPiInNvmIsInstalled(t *testing.T) {
	h := nvmHome(t, []string{"v22.22.2"}, map[string]string{"default": "v22.22.2"})
	pi := filepath.Join(h, ".nvm", "versions", "node", "v22.22.2", "bin", "pi")
	os.WriteFile(pi, []byte("#!/bin/sh\n"), 0o755)
	u := &usageLimits{binDirs: defaultBinDirs(h)}
	if !u.installed("pi") {
		t.Fatal("pi in the nvm default bin dir is not found")
	}
}

func TestBinPathEnv(t *testing.T) {
	env := []string{"A=1", "PATH=/usr/bin:/bin"}
	got := binPathEnv(env, "/h/.nvm/versions/node/v22.22.2/bin/pi")
	if findEnv(got, "PATH") != "/h/.nvm/versions/node/v22.22.2/bin:/usr/bin:/bin" || findEnv(got, "A") != "1" {
		t.Fatalf("env = %v", got)
	}
	if strings.Count(strings.Join(got, " "), "PATH=") != 1 {
		t.Fatalf("PATH twice: %v", got)
	}
	if binPathEnv(env, "/opt/homebrew/bin/codex") != nil {
		t.Fatal("other dirs inherit the environment")
	}
}
