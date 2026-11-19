package main

import (
	"net"
	"net/http"
	"net/http/pprof"
)

// registerLocalPprof serves Go's profiler to this Mac only; a request from any other address gets 404.
func registerLocalPprof(mux *http.ServeMux) {
	local := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/debug/pprof/", local(pprof.Index))
	mux.HandleFunc("/debug/pprof/profile", local(pprof.Profile))
	mux.HandleFunc("/debug/pprof/heap", local(pprof.Index))
	mux.HandleFunc("/debug/pprof/goroutine", local(pprof.Index))
}
