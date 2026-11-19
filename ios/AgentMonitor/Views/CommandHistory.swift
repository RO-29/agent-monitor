import SwiftUI

/// One `!` command a terminal ran (GET /api/session/{id}/commands, /api/commands/recent).
struct CmdItem: Decodable, Hashable, Identifiable {
    var cmd: String
    var at: Int64
    var exitCode: Int?
    var output: String?
    var bg: Bool?
    var sessionId: String?
    var cwd: String?
    var id: String { "\(sessionId ?? "")|\(at)|\(cmd)" }
}

struct CmdList: Decodable {
    var items: [CmdItem] { lossy?.compactMap(\.value) ?? [] }
    private var lossy: [Lossy<CmdItem>]?
    private enum CodingKeys: String, CodingKey { case lossy = "items" }
}

extension APIClient {
    func sessionCommands(_ sessionID: String, limit: Int = 100) async throws -> [CmdItem] {
        try await get("/api/session/\(enc(sessionID))/commands?limit=\(limit)", as: CmdList.self).items
    }
    func recentCommands(limit: Int = 50) async throws -> [CmdItem] {
        try await get("/api/commands/recent?limit=\(limit)", as: CmdList.self).items
    }
}

/// Bash history for a Claude terminal: this session first, then recent commands from every session.
struct CommandHistorySheet: View {
    @Environment(\.dismiss) private var dismiss
    let sessionID: String
    /// Sends "!" + command to this session; nil when the terminal cannot take it.
    let run: ((String) async -> Void)?

    @State private var mine: [CmdItem] = []
    @State private var recent: [CmdItem] = []
    @State private var loaded = false
    @State private var missing = false
    @State private var error: String?
    @State private var confirm: CmdItem?

    var body: some View {
        NavigationStack {
            List {
                if missing {
                    Section {
                        Label("No command history for this session.", systemImage: "clock.badge.questionmark")
                            .font(.subheadline).foregroundStyle(Tok.muted)
                    }
                    .listRowBackground(Tok.panel)
                }
                if let error { Section { InlineError(message: error) }.listRowBackground(Tok.panel) }
                if !mine.isEmpty {
                    Section("This session") { ForEach(mine) { row($0, here: true) } }
                        .listRowBackground(Tok.panel)
                }
                let others = recent.filter { r in !mine.contains { $0.cmd == r.cmd && $0.at == r.at } }
                if !others.isEmpty {
                    Section("Recent, all sessions") { ForEach(others) { row($0, here: false) } }
                        .listRowBackground(Tok.panel)
                }
            }
            .scrollContentBackground(.hidden)
            .background(Tok.bg)
            .overlay {
                if !loaded { ProgressView() } else if !missing && error == nil && mine.isEmpty && recent.isEmpty {
                    ContentUnavailableView("No commands yet", systemImage: "terminal",
                                           description: Text("Type ! and a command in the message box to run it in the terminal."))
                }
            }
            .navigationTitle("Command History")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
            .confirmationDialog("Run this command again?", isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } }),
                                titleVisibility: .visible, presenting: confirm) { c in
                Button("Run in the Terminal") {
                    Task { await run?(c.cmd); dismiss() }
                }
            } message: { c in Text("! " + c.cmd) }
        }
        .toastHost()
        .task { await load() }
    }

    private func row(_ c: CmdItem, here: Bool) -> some View {
        CommandRow(item: c, showWhere: !here, onCopy: { copyToPasteboard(c.cmd, label: "Command copied") },
                   onRun: run == nil ? nil : { confirm = c })
    }

    private func load() async {
        #if DEBUG
        if DebugRoute.sample { (mine, recent) = Self.samples(); loaded = true; return }
        #endif
        do {
            mine = try await APIClient.shared.sessionCommands(sessionID)
        } catch let e as APIError where e.status == 404 || e.status == 405 || e.status == 501 {
            // 404: no transcript yet (or an older daemon); 501: this agent records no commands.
            missing = mine.isEmpty
        } catch {
            self.error = error.localizedDescription
        }
        if let r = try? await APIClient.shared.recentCommands() { recent = r }
        loaded = true
    }

    #if DEBUG
    static func samples() -> ([CmdItem], [CmdItem]) {
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        let mine = [
            CmdItem(cmd: "git status --short", at: now - 60_000, exitCode: 0, output: " M ios/AgentMonitor/Views/ThreadComposer.swift\n?? ios/AgentMonitor/Voice.swift"),
            CmdItem(cmd: "xcodebuild -scheme AgentMonitor build | tail -3", at: now - 400_000, exitCode: 65, output: "error: cannot find 'Dictation' in scope\n** BUILD FAILED **"),
        ]
        let recent = [CmdItem(cmd: "go test ./... -run Voice", at: now - 900_000, exitCode: 0, output: "ok  agent-monitor 1.2s",
                              sessionId: "claude:other", cwd: "/Users/me/agent-monitor")]
        return (mine, recent)
    }
    #endif
}

/// A command with its exit status; the output folds open below.
struct CommandRow: View {
    let item: CmdItem
    var showWhere = false
    let onCopy: () -> Void
    let onRun: (() -> Void)?
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text("!").font(.body.monospaced().weight(.bold)).foregroundStyle(Tok.need)
                Text(item.cmd).font(.subheadline.monospaced()).foregroundStyle(Tok.fg).lineLimit(open ? nil : 2)
                    .textSelection(.enabled)
                Spacer(minLength: 0)
            }
            HStack(spacing: 6) {
                if let e = item.exitCode {
                    Text(e == 0 ? "exit 0" : "exit \(e)").foregroundStyle(e == 0 ? Tok.ok : Tok.err)
                }
                if item.bg == true { Text("background") }
                Text(relativeTime(item.at))
                if showWhere, let c = item.cwd, !c.isEmpty { Text("· " + (c as NSString).lastPathComponent).lineLimit(1) }
                Spacer()
                if let o = item.output, !o.isEmpty {
                    Button { withAnimation(.snappy) { open.toggle() } } label: {
                        Label(open ? "Hide output" : "Output", systemImage: "chevron.right")
                            .labelStyle(.titleAndIcon)
                    }
                    .buttonStyle(.borderless)
                }
                Button(action: onCopy) { Image(systemName: "doc.on.doc").frame(width: 32, height: 28) }
                    .buttonStyle(.borderless).accessibilityLabel("Copy command")
                if let onRun {
                    Button(action: onRun) { Image(systemName: "arrow.clockwise").frame(width: 32, height: 28) }
                        .buttonStyle(.borderless).accessibilityLabel("Run again")
                }
            }
            .font(.caption.monospacedDigit())
            .foregroundStyle(Tok.muted)
            if open, let o = item.output {
                CodeBlock(text: o, maxHeight: 240)
                    .contextMenu { CopyMenuItems(markdown: o, label: "Copy Output", isCode: true) }
            }
        }
        .padding(.vertical, 4)
        .swipeActions(edge: .trailing) {
            Button(action: onCopy) { Label("Copy", systemImage: "doc.on.doc") }.tint(Tok.accent)
            if let onRun { Button(action: onRun) { Label("Run Again", systemImage: "arrow.clockwise") }.tint(Tok.ok) }
        }
        .contextMenu {
            Button(action: onCopy) { Label("Copy Command", systemImage: "terminal") }
            if let o = item.output, !o.isEmpty {
                Button { copyToPasteboard(o) } label: { Label("Copy Output", systemImage: "doc.on.doc") }
            }
            if let onRun { Button(action: onRun) { Label("Run Again", systemImage: "arrow.clockwise") } }
        }
    }
}

/// Settings, Command History: every ! command from every session, with filter, copy and run-again in its own session.
struct AllCommandsView: View {
    @Environment(AppStore.self) private var store
    @State private var items: [CmdItem] = []
    @State private var loaded = false
    @State private var error: String?
    @State private var query = ""
    @State private var confirm: CmdItem?

    private var shown: [CmdItem] {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        return q.isEmpty ? items : items.filter { $0.cmd.lowercased().contains(q) || ($0.cwd ?? "").lowercased().contains(q) }
    }

    var body: some View {
        List {
            if let error { Section { InlineError(message: error) }.listRowBackground(Tok.panel) }
            ForEach(shown) { c in
                CommandRow(item: c, showWhere: true, onCopy: { copyToPasteboard(c.cmd, label: "Command copied") },
                           onRun: c.sessionId == nil ? nil : { confirm = c })
                    .listRowBackground(Tok.panel)
                    .swipeActions(edge: .leading) {
                        if let sid = c.sessionId {
                            Button { store.openThread(sid) } label: { Label("Open Thread", systemImage: "bubble.left") }.tint(Tok.accent)
                        }
                    }
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .searchable(text: $query, prompt: "Command or folder")
        .overlay {
            if !loaded { ProgressView() } else if error == nil && shown.isEmpty {
                ContentUnavailableView(query.isEmpty ? "No commands yet" : "No match", systemImage: "terminal",
                                       description: Text(query.isEmpty ? "Type ! and a command in a thread's message box." : "No command contains that text."))
            }
        }
        .navigationTitle("Command History")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("Run this command again?", isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } }),
                            titleVisibility: .visible, presenting: confirm) { c in
            Button("Run in Its Terminal") { Task { await run(c) } }
        } message: { c in Text("! " + c.cmd) }
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        do {
            items = try await APIClient.shared.recentCommands(limit: 200)
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
        loaded = true
    }

    private func run(_ c: CmdItem) async {
        guard let sid = c.sessionId else { return }
        do {
            let queued = try await APIClient.shared.cliSend(sid, text: "!" + c.cmd)
            ToastCenter.shared.show(queued ? "The agent is busy; the command runs next." : "Sent to the terminal")
        } catch {
            ToastCenter.shared.show(error.localizedDescription, icon: "exclamationmark.triangle.fill")
        }
    }
}
