package main

import (
	"net"
	"net/http"
	"os"
	"strings"
)

// hostAllowed blocks DNS rebinding: a rebinding page reaches us under its own DNS name, never an IP literal.
// Extra names (MagicDNS, tailscale serve) come from AGENT_MONITOR_ALLOWED_HOSTS, comma separated.
func hostAllowed(host string) bool {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "" {
		return false
	}
	if h == "localhost" || net.ParseIP(h) != nil {
		return true
	}
	for _, a := range strings.Split(os.Getenv("AGENT_MONITOR_ALLOWED_HOSTS"), ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" && a == h {
			return true
		}
	}
	return false
}

func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host) {
			writeJSONStatus(w, http.StatusMisdirectedRequest, map[string]any{"error": "unknown Host; add it to AGENT_MONITOR_ALLOWED_HOSTS"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
