import SwiftUI

// New thread (NewThread.tsx): prompt, attachments, folder, agent, where it runs and its options.
// Terminal starts go to POST /api/orch/terminal, headless ones to POST /api/orch/threads.
struct NewThreadSheet: View {
    @Environment(AppStore.self) private var store
    @Environment(\.dismiss) private var dismiss

    enum Where: String { case terminal, headless }

    @State private var prompt = ""
    @State private var tray = AttachmentTray()
    @State private var cwd = ""
    @State private var agent = "claude"
    @State private var whereSel: Where = .terminal
    @State private var perm = "manual"
    @State private var sandbox = "read-only"
    @State private var model = ""
    @State private var worktree = false
    @State private var busy = false
    @State private var error: String?
    @State private var starting: String?
    @State private var hostID: String?
    @State private var hostPrompt: OHostPrompt?
    @State private var screenTail: [String] = []
    @State private var answering = false
    @State private var answerError: String?
    @State private var declined = false
    @FocusState private var promptFocused: Bool
    @State private var dictation = Dictation()
    @State private var draftReady = false
    private static let draftKey = "new"

    private static let permModes: [(String, String, String)] = [
        ("manual", "Ask each time", "Each edit and command waits for your answer."),
        ("acceptEdits", "Accept edits", "File edits go through. Commands still wait for you."),
        ("plan", "Plan first", "The agent plans. You approve the plan before it edits."),
        ("auto", "Auto", "The agent asks only for actions it judges risky."),
    ]

    private var agents: [OAgentInfo] {
        store.orchAgents ?? [OAgentInfo(id: "claude", name: "Claude Code", kind: "claude", installed: true),
                             OAgentInfo(id: "codex", name: "Codex", kind: "codex", installed: true)]
    }
    private var info: OAgentInfo? { agents.first { $0.id == agent } }
    private var kind: String { info?.kind ?? (agent == "claude" || agent == "codex" ? agent : "acp") }
    private var canTerminal: Bool { kind == "claude" || kind == "codex" }
    private var place: Where { canTerminal ? whereSel : .headless }
    private var can: Bool {
        !busy && !cwd.trimmingCharacters(in: .whitespaces).isEmpty && !prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !tray.uploading && !tray.failed && info?.installed != false
    }

    var body: some View {
        NavigationStack {
            Group {
                if let starting { startingView(starting) } else { form }
            }
            .navigationTitle("New Thread")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(starting == nil ? "Cancel" : "Close") { dismiss() }
                }
            }
            .navigationDestination(for: String.self) { _ in FolderPicker(selection: $cwd) }
        }
        .toastHost()
        .onChange(of: prompt) { _, _ in saveDraft() }
        .onChange(of: tray.ids) { _, _ in saveDraft() }
        .task {
            if !draftReady, starting == nil, let d = DraftStore.shared.get(Self.draftKey) {
                prompt = d.text
                tray.restore(d.attachments)
            }
            #if DEBUG
            draftReady = !DebugRoute.sample
            #else
            draftReady = true
            #endif
            if cwd.isEmpty { cwd = store.newThreadFolder ?? store.recentFolders().first ?? "" }
            if store.orchAgents == nil { await store.loadOrchAgents() }
            #if DEBUG
            if DebugRoute.screen == "new:sample" {
                prompt = "Add a share button to the thread screen. Use the screenshot and the notes for the layout."
                tray.addSamples()
            }
            if DebugRoute.screen == "new:sample-trust" {
                starting = "Starting Claude Code in tmux session am-42"
                hostPrompt = OHostPrompt(kind: "trust", agent: "claude", folder: "/Users/me/agent-monitor-v3",
                                         text: "Do you trust the files in this folder? Claude Code may read, write or run files here. Only continue in folders you trust.",
                                         options: [.init(id: "trust", label: "Yes, I trust this folder"), .init(id: "deny", label: "No, exit")])
            }
            #endif
        }
    }

    // MARK: form

    private var form: some View {
        Form {
            Section {
                TextField("What should the agent do?", text: $prompt, axis: .vertical)
                    .font(.body)
                    .lineLimit(4...12)
                    .focused($promptFocused)
                    .padding(.vertical, 4)
                DictationBar(dictation: dictation, text: $prompt)
                    .listRowInsets(EdgeInsets(top: 4, leading: 10, bottom: 4, trailing: 10))
                if !tray.isEmpty {
                    AttachmentChips(tray: tray)
                        .listRowInsets(EdgeInsets(top: 8, leading: 12, bottom: 8, trailing: 12))
                }
                HStack(spacing: 4) {
                    AttachButton(tray: tray, size: 40)
                    Text(tray.isEmpty ? "Add photos or files" : tray.uploadSummary ?? "\(tray.items.count) attached")
                        .font(.subheadline).foregroundStyle(Tok.muted)
                    Spacer()
                    MicButton(dictation: dictation, text: $prompt, size: 40)
                }
                .listRowInsets(EdgeInsets(top: 2, leading: 8, bottom: 2, trailing: 12))
            }
            .listRowBackground(Tok.panel)

            Section {
                NavigationLink(value: "folder") {
                    HStack(spacing: 12) {
                        Image(systemName: "folder.fill").foregroundStyle(Tok.accent).font(.title3)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(cwd.isEmpty ? "Choose a folder" : (cwd as NSString).lastPathComponent)
                                .font(.body.weight(.medium)).foregroundStyle(Tok.fg)
                            if !cwd.isEmpty {
                                Text(cwd).font(.caption.monospaced()).foregroundStyle(Tok.muted).lineLimit(1).truncationMode(.head)
                            }
                        }
                    }
                    .padding(.vertical, 2)
                }
            } header: { Text("Folder") }
            .listRowBackground(Tok.panel)

            Section {
                agentGrid
                    .listRowInsets(EdgeInsets(top: 10, leading: 12, bottom: 10, trailing: 12))
            } header: { Text("Agent") } footer: {
                if store.orchAgents == nil { Text("Showing Claude Code and Codex until the daemon lists its agents.") }
            }
            .listRowBackground(Tok.panel)

            Section {
                Picker("Where", selection: $whereSel) {
                    Text("Terminal").tag(Where.terminal)
                    Text("Headless").tag(Where.headless)
                }
                .pickerStyle(.segmented)
                .disabled(!canTerminal)
                .listRowInsets(EdgeInsets(top: 10, leading: 12, bottom: 10, trailing: 12))
            } header: { Text("Where it runs") } footer: {
                Text(!canTerminal ? "\(info?.name ?? agentName(agent)) runs headless only."
                     : place == .terminal ? "A new tmux session on the Mac. Attach any time, answer here or there."
                     : "agent-monitor drives the agent. No terminal.")
            }
            .listRowBackground(Tok.panel)

            Section("Options") {
                if kind == "codex" {
                    Picker("Sandbox", selection: $sandbox) {
                        Text("Read only").tag("read-only")
                        Text("Workspace write").tag("workspace-write")
                    }
                    TextField("Model (Codex default)", text: $model)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                } else {
                    Picker("Permissions", selection: $perm) {
                        ForEach(kind == "acp" ? Self.permModes.filter { $0.0 == "manual" || $0.0 == "acceptEdits" } : Self.permModes, id: \.0) {
                            Text($0.1).tag($0.0)
                        }
                    }
                    if kind == "claude" {
                        Picker("Model", selection: $model) {
                            Text("Default").tag("")
                            Text("Opus").tag("opus")
                            Text("Sonnet").tag("sonnet")
                        }
                    }
                }
                if place == .headless {
                    Toggle("New git worktree", isOn: $worktree)
                }
            }
            .listRowBackground(Tok.panel)

            if let error {
                Section {
                    Label(error, systemImage: "exclamationmark.triangle.fill")
                        .font(.subheadline).foregroundStyle(Tok.err)
                }
                .listRowBackground(Tok.errSoft)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .scrollDismissesKeyboard(.interactively)
        .safeAreaInset(edge: .bottom, spacing: 0) { startBar }
        .onChange(of: agent) { _, _ in model = ""; if kind == "acp" && perm != "acceptEdits" { perm = "manual" } }
    }

    private var agentGrid: some View {
        LazyVGrid(columns: [GridItem(.flexible(), spacing: 8), GridItem(.flexible(), spacing: 8)], spacing: 8) {
            ForEach(agents) { a in
                let on = a.id == agent
                Button { agent = a.id } label: {
                    HStack(spacing: 8) {
                        AgentGlyph(agent: a.id, size: 18)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(a.kind == "acp" ? a.name : agentName(a.id)).font(.subheadline.weight(.semibold)).foregroundStyle(Tok.fg)
                                .lineLimit(2).minimumScaleFactor(0.85).multilineTextAlignment(.leading)
                            if !a.installed { Text("Not installed").font(.caption).foregroundStyle(Tok.muted) }
                        }
                        Spacer(minLength: 0)
                        if on { Image(systemName: "checkmark.circle.fill").foregroundStyle(Tok.accent) }
                    }
                    .padding(.horizontal, 10)
                    .frame(maxWidth: .infinity, minHeight: 48)
                    .background(on ? Tok.accentSoft : Tok.panel2, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).stroke(on ? Tok.accent : Tok.line, lineWidth: on ? 1.5 : 1))
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .disabled(!a.installed)
                .opacity(a.installed ? 1 : 0.5)
                .accessibilityAddTraits(on ? .isSelected : [])
            }
        }
    }

    private var startBar: some View {
        Button { Task { await start() } } label: {
            HStack(spacing: 8) {
                if busy { ProgressView().tint(Tok.panel) }
                VStack(spacing: 1) {
                    Text(busy ? "Starting" : place == .terminal ? "Start in a Terminal" : "Start Thread")
                    if let up = tray.uploadSummary, !busy {
                        Text(up).font(.caption.weight(.medium)).monospacedDigit().opacity(0.85)
                    } else if tray.failed && !busy {
                        Text("Retry or remove the failed file").font(.caption.weight(.medium)).opacity(0.85)
                    }
                }
            }
            .frame(maxWidth: .infinity)
        }
        .buttonStyle(.am(.pri, big: true, full: true))
        .disabled(!can)
        .padding(.horizontal, 16).padding(.top, 10).padding(.bottom, 8)
        .background(.bar)
    }

    private func startingView(_ msg: String) -> some View {
        ScrollView {
            VStack(spacing: 16) {
                if let p = hostPrompt {
                    trustCard(p)
                } else if !declined {
                    ProgressView().controlSize(.large)
                }
                Text(msg).font(.headline).foregroundStyle(Tok.fg).multilineTextAlignment(.center)
                if !declined {
                    Text("The thread opens when the agent links its session. It also appears in Threads.")
                        .font(.subheadline).foregroundStyle(Tok.muted).multilineTextAlignment(.center)
                }
                if hostPrompt == nil, timedOut, !screenTail.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Terminal screen").font(.caption.weight(.semibold)).foregroundStyle(Tok.muted)
                        Text(screenTail.joined(separator: "\n"))
                            .font(.caption.monospaced()).foregroundStyle(Tok.fg2)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .textSelection(.enabled)
                    }
                    .padding(12)
                    .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                }
            }
            .padding(32)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Tok.bg)
    }

    private var timedOut: Bool { starting?.hasPrefix("Still starting") == true }

    private func trustCard(_ p: OHostPrompt) -> some View {
        let folder = ((p.folder ?? cwd) as NSString).lastPathComponent
        let trust = p.kind == "trust"
        return VStack(alignment: .leading, spacing: 10) {
            Label(trust ? "\(agentName(p.agent)) asks whether you trust \(folder)" : "\(agentName(p.agent)): \(p.title ?? "a question before it starts")",
                  systemImage: trust ? "lock.shield" : "questionmark.circle")
                .font(.headline).foregroundStyle(Tok.fg)
            if let f = p.folder {
                Text(f).font(.caption.monospaced()).foregroundStyle(Tok.muted).lineLimit(1).truncationMode(.head)
            }
            Text(shortExplanation(p.text)).font(.subheadline).foregroundStyle(Tok.fg2)
            if let answerError {
                Label(answerError, systemImage: "exclamationmark.triangle.fill").font(.caption).foregroundStyle(Tok.err)
            }
            // The host's own option labels, verbatim and in screen order.
            VStack(spacing: 8) {
                if p.options.isEmpty {
                    Button("Trust") { Task { await answer("trust") } }.buttonStyle(.am(.allow, big: true, full: true))
                    Button("Cancel") { Task { await answer("cancel") } }.buttonStyle(.am(.deny, big: true, full: true))
                } else {
                    ForEach(p.options, id: \.id) { o in
                        Button { Task { await answer(o.id) } } label: {
                            Text(o.label).multilineTextAlignment(.center)
                        }
                        .buttonStyle(.am(o.id == "deny" || (trust && o.id != "trust") ? .deny : .allow, big: true, full: true))
                    }
                }
            }
            .disabled(answering)
        }
        .padding(14)
        .background(Tok.needSoft, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 14, style: .continuous).stroke(Tok.need, lineWidth: 1))
    }

    /// The dialog text without the line that repeats the question, cut to two sentences.
    private func shortExplanation(_ text: String) -> String {
        let parts = text.components(separatedBy: ". ").filter { !$0.lowercased().contains("quick safety check") }
        let s = parts.prefix(2).joined(separator: ". ")
        return s.count > 220 ? String(s.prefix(217)) + "..." : s
    }

    private func answer(_ option: String) async {
        guard let id = hostID else { return }
        answering = true
        answerError = nil
        defer { answering = false }
        do {
            try await APIClient.shared.answerHost(id, option: option)
            hostPrompt = nil
            if option == "cancel" || option == "deny" {
                declined = true
                starting = "Cancelled. \(agentName(agent)) did not start in this folder."
            }
        } catch {
            answerError = "Could not answer: \(error.localizedDescription)"
        }
    }

    // MARK: draft

    private func saveDraft() {
        guard draftReady, starting == nil else { return }
        DraftStore.shared.set(Self.draftKey, text: prompt, attachments: tray.uploadedList)
    }

    private func clearDraft() {
        draftReady = false
        prompt = ""
        DraftStore.shared.clear(Self.draftKey)
    }

    // MARK: start

    private func start() async {
        guard can else { return }
        busy = true
        error = nil
        defer { busy = false }
        await dictation.settle(into: $prompt)
        let dir = cwd.trimmingCharacters(in: .whitespaces)
        let text = prompt.trimmingCharacters(in: .whitespacesAndNewlines)
        var body: [String: Any] = ["agent": agent, "cwd": dir, "prompt": text]
        if !tray.ids.isEmpty { body["attachments"] = tray.ids }
        let m = model.trimmingCharacters(in: .whitespaces)
        if !m.isEmpty { body["model"] = m }
        if kind == "codex" { body["sandbox"] = sandbox } else { body["permissionMode"] = perm }
        do {
            if place == .terminal {
                body["agent"] = kind
                if kind == "codex" { body["approvalPolicy"] = "on-request" }
                let t = try await APIClient.shared.startTerminal(body)
                starting = "Starting \(info?.name ?? agentName(agent)) in tmux session \(t.session)"
                tray.clear()
                clearDraft()
                await follow(t.hostId)
            } else {
                if worktree { body["workspace"] = "worktree" }
                let id = try await APIClient.shared.createThread(body)
                tray.clear()
                clearDraft()
                dismiss()
                await store.loadThreads()
                store.openThread(id)
            }
        } catch let e as APIError where e.status == 402 {
            error = "Today's spend limit is reached. Raise it on the Mac in Settings, Limits. \(e.message)"
        } catch {
            self.error = "Could not start the thread: \(error.localizedDescription)"
        }
    }

    /// Waits for the host to report the linked session, then opens it. After 40 s it says "Still starting" but
    /// keeps checking (up to 30 min), so a first-run menu that shows late still gets its card.
    private func follow(_ hostID: String) async {
        self.hostID = hostID
        let begin = Date()
        var t0 = Date()
        var slow = false
        while !Task.isCancelled, !declined, Date().timeIntervalSince(begin) < 1800 {
            if let h = try? await APIClient.shared.hostInfo(hostID) {
                if let aid = h.agentId, !aid.isEmpty {
                    dismiss()
                    await store.loadThreads()
                    store.openThread(aid)
                    return
                }
                if h.prompt != hostPrompt, !answering { hostPrompt = h.prompt; answerError = nil }
                if let tail = h.screenTail { screenTail = tail }
                if h.prompt != nil || answering { t0 = Date() }
            }
            if !slow, Date().timeIntervalSince(t0) >= 40 {
                slow = true
                starting = "Still starting. The thread appears in Threads when the agent links its session."
            }
            try? await Task.sleep(nanoseconds: slow ? 2_000_000_000 : 1_000_000_000)
        }
        if !declined { hostPrompt = nil }
    }
}

/// Recent folders from the thread list, plus a typed path.
private struct FolderPicker: View {
    @Environment(AppStore.self) private var store
    @Environment(\.dismiss) private var dismiss
    @Binding var selection: String
    @State private var typed = ""

    var body: some View {
        List {
            Section("Recent") {
                ForEach(store.recentFolders(20), id: \.self) { f in
                    Button { selection = f; dismiss() } label: {
                        HStack(spacing: 12) {
                            Image(systemName: "folder").foregroundStyle(Tok.accent)
                            VStack(alignment: .leading, spacing: 2) {
                                Text((f as NSString).lastPathComponent).font(.body.weight(.medium)).foregroundStyle(Tok.fg)
                                Text(f).font(.caption.monospaced()).foregroundStyle(Tok.muted).lineLimit(1).truncationMode(.head)
                            }
                            Spacer()
                            if f == selection { Image(systemName: "checkmark").foregroundStyle(Tok.accent).fontWeight(.semibold) }
                        }
                        .frame(minHeight: 44)
                    }
                }
            }
            .listRowBackground(Tok.panel)
            Section {
                TextField("/Users/you/project", text: $typed)
                    .font(.body.monospaced())
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                    .submitLabel(.done)
                    .onSubmit(useTyped)
                Button("Use This Folder", action: useTyped)
                    .disabled(typed.trimmingCharacters(in: .whitespaces).isEmpty)
            } header: { Text("Other folder") } footer: { Text("A full path on the Mac.") }
            .listRowBackground(Tok.panel)
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Folder")
        .navigationBarTitleDisplayMode(.inline)
    }

    private func useTyped() {
        let t = typed.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty else { return }
        selection = t
        dismiss()
    }
}
