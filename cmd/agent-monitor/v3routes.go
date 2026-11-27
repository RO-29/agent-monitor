package main

import "net/http"

// registerV3Routes wires the v3 API. Each part owns its own file.
func registerV3Routes(mux *http.ServeMux) {
	registerApprovalRoutes(mux) // approvals.go
	registerOrchRoutes(mux)     // orch_api.go: driven Codex threads; needs the approval store
	registerOTARoutes(mux)      // ios_ota.go: iPhone install files for ios/ota.sh
	registerIOSRenewRoutes(mux) // ios_renew.go: the iPhone asks for a build before its profile ends
	registerLocalPprof(mux)     // pprof_local.go: profiler for this Mac only
	registerNeedsYouRoutes(mux) // needs_you.go: the one list of requests every surface draws
	registerPromptRoutes(mux)   // prompt_history.go: prompts you typed, from the agents' history files
	registerPushRoutes(mux)     // push_bark.go: lock-screen alerts through the Bark app
	registerNotifyRoutes(mux)   // notify.go: notification centre, SSE, Web Push; needs orchMgr
	registerSearchRoutes(mux)   // search.go
	registerOpsRoutes(mux)      // ops.go: child links, cross-agent traces, monitors, intel
	registerDeviceRoutes(mux)   // devices.go: per-device tokens
	registerSecretRoutes(mux)   // secrets.go: request_secret cards
	registerTermRoutes(mux)     // term.go: browser terminals on a pty
	registerHostRoutes(mux)     // host_registry.go: session hosts and terminal sessions
	registerUsageRoutes(mux)    // usage_limits.go: plan usage windows and local token totals
	registerCommandRoutes(mux)  // session_cmds.go: ! command history across sessions
	registerWiringRoutes(mux)   // wire_run.go: agent wiring status, first-run confirmation, agent registry
	registerVoiceRoutes(mux)    // voice.go: dictation cleanup through ~/voice-dictation/clean.sh
}
