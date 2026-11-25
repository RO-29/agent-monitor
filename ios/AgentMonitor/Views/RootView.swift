import SwiftUI

struct RootView: View {
    @Environment(AppStore.self) private var store

    var body: some View {
        @Bindable var store = store
        TabView(selection: $store.tab) {
            WorkspaceView()
                .tabItem { Label("Threads", systemImage: "bubble.left.and.text.bubble.right") }
                .badge(store.needsYouThreadCount)
                .tag(AppTab.threads)

            PermissionsView()
                .tabItem { Label("Approvals", systemImage: "checkmark.shield") }
                .badge(store.approvalsCount + store.remoteNeedCount)
                .tag(AppTab.approvals)

            AlertsView()
                .tabItem { Label("Alerts", systemImage: "bell") }
                .badge(store.alertsUnread)
                .tag(AppTab.alerts)

            SettingsView()
                .tabItem { Label("Settings", systemImage: "gearshape") }
                .badge(store.pendingTalks.count)
                .tag(AppTab.settings)
        }
        .sheet(isPresented: $store.showNewThread) {
            NewThreadSheet()
                .presentationDetents([.large])
                .presentationDragIndicator(.visible)
                .presentationBackground(Tok.bg)
        }
        .toastHost()
        .task { await store.loadOrchAgents() }
        #if DEBUG
        .task { DebugRoute.apply(store) }
        .sheet(isPresented: .constant(DebugRoute.screen?.hasPrefix("usage") == true)) {
            NavigationStack { ScrollView { UsageStrip().padding(16) }.background(Tok.bg).navigationTitle("Usage") }
                .presentationDetents([.large])
        }
        #endif
    }
}

/// Live-connection status: a dot, plus a label when not live. Tapping it reconnects.
struct ConnectionChip: View {
    @Environment(AppStore.self) private var store
    var showLabel = false
    var onlyWhenDown = false
    var body: some View {
        if onlyWhenDown && store.connection == .connected { EmptyView() } else { chip }
    }
    private var chip: some View {
        Button { store.connect() } label: {
            HStack(spacing: 6) {
                StatusDot(color: dotColor, size: 8)
                if showLabel || store.connection != .connected {
                    Text(short).font(.footnote).foregroundStyle(Tok.fg2).lineLimit(1).fixedSize()
                }
            }
            .padding(.horizontal, 4)
            .frame(minHeight: 44)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(store.connection == .connected)
        .accessibilityLabel(store.connection.label)
        .accessibilityHint(store.connection == .connected ? "" : "Reconnects to the daemon")
    }
    private var short: String {
        switch store.connection {
        case .connected: return "Live"
        case .connecting: return "Connecting"
        case .disconnected: return "Offline"
        }
    }
    private var dotColor: Color {
        switch store.connection {
        case .connected: return Tok.ok
        case .connecting: return Tok.need
        case .disconnected: return Tok.err
        }
    }
}

#if DEBUG
/// Screenshot routes for the simulator: `-screen threads|approvals|alerts|settings|settings:<route>|chat:<id>|details:<id>`.
enum DebugRoute {
    static var screen: String? { UserDefaults.standard.string(forKey: "screen") }
    static var wantsDetails: Bool { screen?.hasPrefix("details:") ?? false }

    /// Sample screens never call the daemon for their sample parts.
    static var sample: Bool { screen?.contains("sample") ?? false }

    @MainActor static func apply(_ store: AppStore) {
        guard let s = screen else { return }
        let parts = s.split(separator: ":", maxSplits: 1).map(String.init)
        let arg = parts.count > 1 ? parts[1] : ""
        switch parts[0] {
        case "approvals":
            store.tab = .approvals
            if arg == "sample-ny" || arg == "sample-empty" || arg == "sample-prompt" {
                Task {
                    try? await Task.sleep(nanoseconds: 3_000_000_000)
                    if arg == "sample-empty" { store.debugNeedsYou = []; return }
                    injectSamples(store, only: "sample-high")
                    store.debugNeedsYou = sampleNeedsYou(store).filter { arg != "sample-prompt" || $0.kind == "prompt" }
                }
            } else if arg.hasPrefix("sample") { Task { try? await Task.sleep(nanoseconds: 3_000_000_000); injectSamples(store, only: arg) } }
        case "home":
            store.tab = .threads
            if arg.hasPrefix("sample") {
                Task {
                    try? await Task.sleep(nanoseconds: 3_000_000_000)
                    injectSamples(store, only: arg)
                    if arg == "sample-answer" { store.approvalSheet = "sample-5" }
                    if arg == "sample-sending", let a = store.approvalsByID["sample-1"] {
                        store.pendingDecisions[a.id] = PendingDecision(approval: a, body: .allow(), label: "Allow", start: Date().addingTimeInterval(3600))
                    }
                }
            }
        case "alerts": store.tab = .alerts
        case "new":
            store.tab = .threads
            store.showNewThread = true
        case "settings":
            store.tab = .settings
            let r: [String: SettingsRoute] = ["server": .server, "agents": .agents, "talks": .talks, "panes": .panes, "alerts": .alerts]
            if let route = r[arg] { store.settingsPath = [route] }
        case "linkedpage":
            store.tab = .threads
            let id = arg.hasPrefix("sample") || arg.isEmpty ? "claude:sample" : arg
            store.workspacePath = [id, LinkedAgentsPage.route(id)]
        case "notify":
            // Simulates a tapped notification arriving before the app wired its handler.
            Task {
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                Notifier.shared.handle(arg.hasPrefix("approval") ? .approvals("sample-6") : .thread("claude:sample"))
                if arg.hasPrefix("approval") { injectSamples(store) }
            }
        case "chat", "details", "linked", "toast", "history", "fullscreen", "select":
            store.tab = .threads
            store.workspacePath = [arg.hasPrefix("sample") || arg.isEmpty ? "claude:sample" : arg]
            if parts[0] == "toast" {
                Task { for _ in 0..<12 { try? await Task.sleep(nanoseconds: 1_000_000_000); copyToPasteboard("git push origin feat/ios", label: "Command copied") } }
            }
        default: store.tab = .threads
        }
    }

    static let sampleThread = V3Thread(
        id: "claude:sample", title: "Add attachments to the composer", cwd: "/Users/me/agent-monitor-v3", ref: "feat/ios",
        tools: ["claude"], sessions: ["claude:sample"], state: .running,
        startedAt: Int64(Date().timeIntervalSince1970 * 1000) - 3_600_000, lastAt: Int64(Date().timeIntervalSince1970 * 1000) - 20_000,
        turns: 12, model: "claude-opus-4", childCount: 3, liveActivity: "Editing ThreadComposer.swift", repo: "agent-monitor-v3")

    static func sampleItems() -> [String: WItem] {
        let now = Int64(Date().timeIntervalSince1970 * 1000) - 300_000
        var n: Int64 = 0
        func it(_ kind: String, _ text: String, command: String? = nil, status: String = "completed", exit: Int? = nil,
                output: String? = nil, files: [WFile]? = nil, att: [OAttachment]? = nil) -> WItem {
            n += 1
            return WItem(id: "s\(n)", runId: "r1", kind: kind, status: status, text: text, command: command, exitCode: exit,
                         output: output, files: files, startedAt: now + n * 9_000, endedAt: now + n * 9_000 + 1_400, attachments: att)
        }
        let list = [
            it("user", "Add photo and file attachments to the chat composer. The screenshot shows the spacing I want.",
               att: [OAttachment(id: "up_a", mime: "image/jpeg", name: "composer-mock.jpg", size: 1_840_000, url: "")]),
            it("reasoning", "The composer needs a picker menu, chips with progress, and the upload call."),
            it("command", "", command: "rg -n \"cli-queue/send\" web/src ios", output: "web/src/v3/api/queue.ts:108\nios/AgentMonitor/APIClient+V3.swift:69"),
            it("other", "ios/AgentMonitor/Views/ThreadComposer.swift", command: "Read"),
            it("fileChange", "", files: [WFile(path: "ios/AgentMonitor/Views/Attachments.swift", kind: "add", add: 212, del: 0),
                                         WFile(path: "ios/AgentMonitor/Views/ThreadComposer.swift", kind: "update", add: 48, del: 31)]),
            it("agent", "The composer now has a **+** button with Photo Library, Take Photo and Choose File. Each file uploads at once and shows its progress.\n\nRun the simulator build to check it:\n\n```\n$ xcodebuild -scheme AgentMonitor -destination 'platform=iOS Simulator,name=iPhone 17' build\n```"),
            it("command", "", command: "git push origin feat/ios", status: "completed", exit: 0, output: "To github.com:me/agent-monitor.git\n   26e90b1..4f1c2aa  feat/ios -> feat/ios"),
            it("user", "Compare the three upload options and check what the web does."),
            it("other", "Find the web upload code", command: "Task",
               output: "## Findings\n\nThe web uploads in `web/src/v3/api/uploads.ts`.\n\n- One request per file\n  - progress from `xhr.upload`\n- Max **25 MB**\n\nSee https://developer.apple.com/documentation/photokit for the picker."),
            it("agent", "## Options\n\n| Surface | What I found | State |\n|---|---|---|\n| **Codex CLI** | Codex runs a background memories session in the same pane and registers it under its own id, so sends fail. | Fixed |\n| | A session showed running after it stopped. | Fixed |\n| **Cursor CLI** | One run showed as 3 threads: the process, the chat, and a fake Claude session from its hooks. | Fixed: one thread |\n| **Codex desktop** | Not installed. | Cannot test |\n\n| Option | Size limit | Progress | Retry |\n|---|---|---|---|\n| Multipart POST | 25 MB | yes | manual |\n| Chunked PUT | none | yes | automatic |\n| Base64 in JSON | 5 MB | no | no |\n\n> The web client already uses **multipart**, so the phone should match it.\n\n1. Keep multipart.\n   - Add retry on the chip.\n   - Show progress per file.\n2. Skip chunking for now.\n\nDocs: [URLSession uploads](https://developer.apple.com/documentation/foundation/urlsession).\n\n```swift\nlet (data, resp) = try await session.upload(for: req, from: body)\n```"),
        ]
        return Dictionary(uniqueKeysWithValues: list.map { ($0.id, $0) })
    }

    static func sampleGit() -> OGit {
        OGit(isRepo: true, branch: "feat/ios", head: "4f1c2aa9", upstream: "origin/feat/ios", ahead: 2, behind: 0, dirty: 3,
             defaultBranch: "main", files: [OGitFile(path: "ios/AgentMonitor/Views/Attachments.swift", status: "added", add: 212, del: 0),
                                            OGitFile(path: "ios/AgentMonitor/Views/ThreadComposer.swift", status: "modified", add: 48, del: 31),
                                            OGitFile(path: "ios/AgentMonitor/Views/Old.swift", status: "deleted", add: 0, del: 40)])
    }

    static func sampleKids() -> [SessionChild] {
        let json = """
        [{"key":"k1","kind":"subagent","agent":"claude","agentId":"a1","subagentType":"Explore","description":"Find the web upload code","status":"completed","startedAt":1,"endedAt":42000,"hasTranscript":true},
         {"key":"k2","kind":"subagent","agent":"claude","agentId":"a2","subagentType":"general-purpose","description":"Write the document picker","status":"running","startedAt":1,"background":true,"hasTranscript":true},
         {"key":"k3","kind":"agent","agent":"codex","sessionId":"codex:sample-review","description":"Review the attachment chips","status":"running","startedAt":1},
         {"key":"k4","kind":"subagent","agent":"claude","agentId":"a4","subagentType":"Explore","description":"List the composer tests","status":"completed","startedAt":1,"endedAt":18000,"hasTranscript":true},
         {"key":"k5","kind":"subagent","agent":"claude","agentId":"a5","subagentType":"Plan","description":"Plan the upload retry","status":"failed","startedAt":1,"endedAt":9000,"hasTranscript":true}]
        """
        return (try? JSONDecoder().decode([SessionChild].self, from: Data(json.utf8))) ?? []
    }

    /// One of each needs-you kind: a held approval, a question on step 2 of 2, a prompt, and a prompt nobody can reach.
    @MainActor static func sampleNeedsYou(_ store: AppStore) -> [NeedsYouItem] {
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        let cwd = "/Users/me/agent-monitor-v3"
        let q = TermQuestion(pending: true, tool: "AskUserQuestion", toolUseId: "tu_sample",
            questions: [
                TermQuestionItem(question: "Which database should the tests use?", header: "Database", multiSelect: false,
                                 options: [TermOption(label: "SQLite"), TermOption(label: "Postgres")]),
                TermQuestionItem(question: "Should the old rows be kept?", header: "Cleanup", multiSelect: false,
                                 options: [TermOption(label: "Keep them", description: "Mark them as archived"),
                                           TermOption(label: "Delete them", description: "Free the space now")])],
            sendable: true, onScreen: true, step: 1)
        var out: [NeedsYouItem] = []
        if let a = store.approvalsByID["sample-6"] {
            out.append(NeedsYouItem(kind: "approval", key: a.id, sessionId: a.sessionId, agent: a.agent, cwd: a.cwd, at: now - 240_000, answerable: true, approval: a))
        }
        out.append(NeedsYouItem(kind: "question", key: "tq:claude:s2:tu_sample", sessionId: "claude:s2", agent: "claude", cwd: "/Users/me/billing-api",
                                title: "Move the tests to a real database", at: now - 120_000, answerable: true, question: q))
        out.append(NeedsYouItem(kind: "prompt", key: "tp:codex:s3", sessionId: "codex:s3", agent: "codex", cwd: cwd,
                                title: "Clean the build folder", at: now - 60_000, answerable: true,
                                message: "Allow command: rm -rf build/ ?"))
        out.append(NeedsYouItem(kind: "prompt", key: "tp:cursor:s4", sessionId: "cursor:s4", agent: "cursor", cwd: "/Users/me/site",
                                title: "Update the landing page", at: now - 20_000, answerable: false,
                                why: "answer it in the terminal: agent-monitor cannot reach this session",
                                message: "Run npm install?"))
        return out
    }

    /// Local sample cards for layout checks; they exist only in this process and never reach the daemon.
    /// `sample-q` keeps only the questions and `sample-high` only the high-risk permission.
    @MainActor static func injectSamples(_ store: AppStore, only: String = "sample") {
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        func make(_ id: String, _ kind: String, tool: String, risk: String, input: [String: JSONValue], summary: String, options: Int = 2) -> Approval {
            let labels = ["Grouped by status", "Newest first", "By folder", "By agent", "Pinned first", "Alphabetical"]
            let opts: [JSONValue] = labels.prefix(options).enumerated().map { i, l in
                .object(["label": .string(l), "description": .string(i == 0 ? "Needs you, Working, Ready, Settled" : "One flat list")])
            }
            return Approval(id: id, agent: "claude", sessionId: "claude:sample", cwd: "/Users/me/agent-monitor-v3", kind: kind,
                     toolName: tool, input: input, summary: summary, risk: risk,
                     riskReason: risk == "high" ? "Deletes files outside the repository." : "",
                     suggestions: nil, questions: kind == "question" ? [.object([
                        "question": .string("Which layout should the thread list use?"), "header": .string("Layout"),
                        "multiSelect": .bool(false),
                        "options": .array(opts)])] : nil,
                     plan: kind == "plan" ? "## Plan\n1. Move Agents, Talks and Panes under Settings.\n2. Use native lists with swipe actions.\n3. Raise the chat prose to 17 pt." : nil,
                     createdAt: now - 45_000, holdUntil: now + 600_000, state: "pending")
        }
        for a in [make("sample-1", "permission", tool: "Bash", risk: "medium", input: ["command": .string("git push origin feat/orchestrator")], summary: "git push"),
                  make("sample-2", "question", tool: "AskUserQuestion", risk: "low", input: [:], summary: "Which layout?"),
                  make("sample-3", "plan", tool: "ExitPlanMode", risk: "low", input: [:], summary: "Plan"),
                  make("sample-4", "permission", tool: "mcp__ch-us-staging__run_query", risk: "low",
                       input: ["query": .string("SELECT count() FROM events")], summary: "SELECT count() FROM events"),
                  make("sample-6", "permission", tool: "Bash", risk: "high", input: ["command": .string("rm -rf ~/old-builds")], summary: "rm -rf"),
                  make("sample-5", "question", tool: "AskUserQuestion", risk: "low", input: [:], summary: "Which order?", options: 6)] {
            if only == "sample-q" && a.kind != "question" { continue }
            if only == "sample-high" && a.id != "sample-6" { continue }
            store.approvalsByID[a.id] = a
        }
    }
}
#endif
