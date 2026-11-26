package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var reNodeVersion = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?$`)

func nodeVer(s string) ([3]int, bool) {
	m := reNodeVersion.FindStringSubmatch(strings.TrimSpace(s))
	var v [3]int
	if m == nil {
		return v, false
	}
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// nvmInstalled lists installed node versions under home, newest first.
func nvmInstalled(home string) []string {
	ents, _ := os.ReadDir(filepath.Join(home, ".nvm", "versions", "node"))
	var names []string
	for _, e := range ents {
		if _, ok := nodeVer(e.Name()); ok && e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, _ := nodeVer(names[i])
		b, _ := nodeVer(names[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return false
	})
	return names
}

// nvmDefaultBin is the bin dir of the node version that `nvm` picks for a new shell, or "".
func nvmDefaultBin(home string) string {
	inst := nvmInstalled(home)
	if len(inst) == 0 {
		return ""
	}
	name := "default"
	for i := 0; i < 8; i++ {
		b, err := os.ReadFile(filepath.Join(home, ".nvm", "alias", filepath.FromSlash(name)))
		if err != nil {
			break
		}
		name = strings.TrimSpace(string(b))
		if _, ok := nodeVer(name); ok || name == "node" || name == "stable" || name == "" {
			break
		}
	}
	pick := inst[0]
	if _, ok := nodeVer(name); ok {
		pick = ""
		prefix := strings.TrimPrefix(name, "v")
		for _, v := range inst {
			if t := strings.TrimPrefix(v, "v"); t == prefix || strings.HasPrefix(t, prefix+".") {
				pick = v
				break
			}
		}
	} else if name != "node" && name != "stable" && name != "" && name != "default" {
		return ""
	}
	if pick == "" {
		return ""
	}
	return filepath.Join(home, ".nvm", "versions", "node", pick, "bin")
}

// agentBinDirs is base plus the nvm default bin dir, where npm-installed agents such as pi live.
func agentBinDirs(home string, base ...string) []string {
	out := append([]string{}, base...)
	if d := nvmDefaultBin(home); d != "" {
		out = append(out, d)
	}
	return out
}

// binPathEnv puts the dir of an nvm-installed binary first on PATH: its `#!/usr/bin/env node` needs that node.
// It returns nil (inherit the environment) for any other binary.
func binPathEnv(env []string, bin string) []string {
	dir := filepath.Dir(bin)
	if !strings.Contains(dir, string(filepath.Separator)+".nvm"+string(filepath.Separator)+"versions"+string(filepath.Separator)) {
		return nil
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	path := findEnv(env, "PATH")
	if path != "" {
		path = ":" + path
	}
	return append(out, "PATH="+dir+path)
}

func findEnv(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}
