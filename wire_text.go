package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

const wireName = "agent-monitor"

func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func tomlMCPBlock(cmd string, args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = tomlQuote(a)
	}
	return "[mcp_servers." + wireName + "]\ncommand = " + tomlQuote(cmd) + "\nargs = [" + strings.Join(q, ", ") + "]\n"
}

var (
	tomlHeaderRe = regexp.MustCompile(`(?m)^\[mcp_servers\.(?:agent-monitor|"agent-monitor")\][ \t]*$`)
	tomlOtherRe  = regexp.MustCompile(`(?m)^[ \t]*(?:\[mcp_servers\.(?:agent-monitor|"agent-monitor")\.|"?agent-monitor"?[ \t]*=|mcp_servers[ \t]*=)`)
)

// tomlNamed is the plain-text check: our name on an mcp_servers line or as a key, in any quoting.
func tomlNamed(src string) bool {
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimLeft(l, " \t")
		if strings.Contains(l, "mcp_servers") && strings.Contains(l, wireName) || tomlOtherRe.MatchString(t) {
			return true
		}
		for _, q := range []string{wireName, `"` + wireName + `"`, "'" + wireName + "'"} {
			if r := strings.TrimLeft(strings.TrimPrefix(t, q), " \t"); r != t && (strings.HasPrefix(r, "=") || strings.HasPrefix(r, ".")) {
				return true
			}
		}
	}
	return false
}

// tomlEntry reads command and args of our table; ok is false when the shape is not the plain one.
func tomlEntry(src string) (found bool, cmd string, args []string, ok bool) {
	loc := tomlHeaderRe.FindStringIndex(src)
	if loc == nil {
		return false, "", nil, false
	}
	body := src[loc[1]:]
	for _, l := range strings.Split(body, "\n")[1:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			break
		}
		k, v, has := strings.Cut(t, "=")
		if !has {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "command":
			s, good := tomlString(v)
			if !good {
				return true, "", nil, false
			}
			cmd = s
		case "args":
			if json.Unmarshal([]byte(v), &args) != nil {
				return true, "", nil, false
			}
		}
	}
	return true, cmd, args, cmd != ""
}

func tomlString(v string) (string, bool) {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1], true
	}
	var s string
	return s, json.Unmarshal([]byte(v), &s) == nil && s != ""
}

func yamlMCPBlock(cmd string, args []string) string {
	return "mcp_servers:\n  " + wireName + ":\n    command: " + string(jsonText(cmd)) + "\n    args: " + string(jsonText(args)) + "\n"
}

var yamlTopRe = regexp.MustCompile(`(?m)^mcp_servers[ \t]*:`)

// yamlEntry reads command and args under mcp_servers.agent-monitor, the plain block or flow forms only.
func yamlEntry(src string) (found bool, cmd string, args []string, ok bool) {
	lines := strings.Split(src, "\n")
	top := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "mcp_servers") && yamlTopRe.MatchString(l) {
			top = true
			continue
		}
		if top && l != "" && l[0] != ' ' && l[0] != '\t' && l[0] != '#' {
			top = false
		}
		t := strings.TrimSpace(l)
		if !top || (t != wireName+":" && t != `"`+wireName+`":`) {
			continue
		}
		ind := len(l) - len(strings.TrimLeft(l, " "))
		found = true
		for j := i + 1; j < len(lines); j++ {
			m := lines[j]
			mt := strings.TrimSpace(m)
			if mt == "" || mt[0] == '#' {
				continue
			}
			if len(m)-len(strings.TrimLeft(m, " ")) <= ind {
				break
			}
			k, v, has := strings.Cut(mt, ":")
			if !has {
				continue
			}
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "command":
				cmd = yamlScalar(v)
			case "args":
				if v == "" {
					for _, a := range lines[j+1:] {
						at := strings.TrimSpace(a)
						if !strings.HasPrefix(at, "- ") {
							break
						}
						args = append(args, yamlScalar(strings.TrimSpace(at[2:])))
					}
				} else if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
					inner := strings.TrimSpace(v[1 : len(v)-1])
					if inner != "" {
						for _, p := range strings.Split(inner, ",") {
							args = append(args, yamlScalar(strings.TrimSpace(p)))
						}
					}
				} else {
					return true, "", nil, false
				}
			}
		}
		return true, cmd, args, cmd != ""
	}
	return found, cmd, args, false
}

func yamlScalar(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		var s string
		if v[0] == '"' && json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
		return v[1 : len(v)-1]
	}
	return v
}
