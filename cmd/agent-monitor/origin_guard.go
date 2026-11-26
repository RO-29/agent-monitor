package main

import (
	"net/http"
	"net/url"
	"strings"
)

// sameOriginRequest rejects browser requests sent from another origin (other localhost ports included).
// Native clients (hooks, curl, the iOS and Mac apps) send no Origin and no Sec-Fetch-Site, so they pass.
func sameOriginRequest(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check := true
		switch r.Method {
		case http.MethodOptions:
			check = false
		case http.MethodGet, http.MethodHead:
			// GETs to pages stay open for navigations; GETs to the API and /ws run actions or stream data.
			check = strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws"
		}
		if check && !sameOriginRequest(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "cross-origin request refused"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
