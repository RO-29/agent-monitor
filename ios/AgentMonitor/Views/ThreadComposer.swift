import SwiftUI

// Message box. Watched sessions: POST /api/session/{id}/cli-queue/send types the message into the
// terminal, and a busy Claude Code queues it. Driven threads: POST /api/orch/threads/{id}/messages.
struct Composer: View {
    let target: ChatTarget
    let agent: String
    let busyCLI: Bool
    var model: String? = nil
    var cwd: String = ""
    var nativeID: String = ""
    var closed = false
    /// Opens the command history sheet (Claude terminal threads).
    var onHistory: (() -> Void)? = nil
    /// The thread id: drafts follow the thread, not the session it is on now.
    var draftID: String? = nil
    /// The machine the thread runs on; peers go through the primary server's proxy.
    var api: APIClient = .shared

    @State private var text = ""
    @State private var sending = false
    @State private var acting = false
    @State private var status: String?
    @State private var error: String?
    @State private var pane: WatchedPane?
    @State private var queue: CLIQueueView?
    @State private var tray = AttachmentTray()
    @FocusState private var focused: Bool
    @State private var sentCount = 0
    @State private var dictation = Dictation()
    @State private var restoredKey: String?
    @State private var hist: [String] = []
    @State private var histIdx = -1
    @State private var histTyped = ""
    @State private var histBash = true
    @State private var promptHist: [String] = []
    @State private var promptAt = Date.distantPast
    @State private var showPrompts = false
    @State private var histAt = Date.distantPast

    private var sessionID: String? { if case .watched(let s) = target { return s } else { return nil } }
    private var drivenID: String? { if case .driven(let d) = target { return d } else { return nil } }
    private var cliName: String {
        switch agentKey(agent) {
        case "claude": return "Claude Code"
        case "codex": return "Codex"
        default: return agentName(agent)
        }
    }
    private var reachable: Bool { drivenID != nil ? !closed : (pane?.sendable ?? true) }
    private var hasContent: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !tray.ids.isEmpty }
    private var canSend: Bool { reachable && !sending && !tray.uploading && hasContent }
    private var draftKey: String { "chat:" + (draftID ?? sessionID ?? drivenID ?? "") }
    /// Claude Code and Codex run a line that starts with ! as a shell command in their terminal.
    private var bashCapable: Bool { sessionID != nil && ["claude", "codex"].contains(agentKey(agent)) }
    private var bangTyped: Bool { text.hasPrefix("!") }
    private var bashMode: Bool { bashCapable && bangTyped }
    /// The text Clear removed, kept 5 s for Undo.
    @State private var clearedText: String?
    @State private var slashPanel: SlashPanel?
    /// This agent's usage credits, shown under the status line.
    @State private var credits: String?
    @State private var skillCmds: [(String, String)] = []
    /// While the box holds "/word" with no space: the agent's built-in commands and skills that match.
    private var slashHits: [(String, String)] {
        guard bashCapable, text.hasPrefix("/"), !text.contains(" "), !text.contains("\n") else { return [] }
        let q = text.dropFirst().lowercased()
        let all = agentSlashCommands(agentKey(agent)) + skillCmds
        var seen = Set<String>()
        return all.filter { q.isEmpty || $0.0.lowercased().contains(q) }.filter { seen.insert($0.0).inserted }.prefix(8).map { $0 }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            let sl = (reachable ? queue?.statusLine ?? [] : []) + (credits.map { [$0] } ?? [])
            if !sl.isEmpty {
                Text(sl.joined(separator: "\n")).font(.caption.monospaced()).foregroundStyle(.secondary)
                    .lineLimit(5).frame(maxWidth: .infinity, alignment: .leading)
            }
            if let d = queue?.draft, !d.isEmpty, reachable {
                VStack(alignment: .leading, spacing: 6) {
                    Text("IN THE TERMINAL BOX").font(Typo.ui(10.5, .semibold)).tracking(0.5).foregroundStyle(Tok.muted)
                    Text(d).font(Typo.mono(13)).foregroundStyle(Tok.fg).lineLimit(4)
                        .frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
                    HStack(spacing: 8) {
                        Spacer()
                        Button("Edit here") {
                            Task {
                                if await act("clear-draft", nil) { text = withInsert(text, d) }
                            }
                        }.buttonStyle(.am(.ghost)).disabled(acting)
                        Button("Clear") { Task { _ = await act("clear-draft", nil) } }.buttonStyle(.am(.ghost)).disabled(acting)
                    }
                }
                .padding(10)
                .background(Tok.panel, in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Tok.line2, style: StrokeStyle(lineWidth: 1, dash: [4, 3])))
                .padding(.horizontal, 4)
            }
            if let q = queue, q.supported, let qi = q.items, !qi.isEmpty {
                queueBox(q, qi)
            }
            if !reachable && error == nil && status == nil {
                Label(closed ? "This thread is closed." : "This session runs in a terminal agent-monitor cannot reach.",
                      systemImage: closed ? "lock" : "terminal")
                    .font(.footnote).foregroundStyle(Tok.muted).padding(.horizontal, 6)
            }
            if let error {
                Text(error).font(.footnote).foregroundStyle(Tok.err).padding(.horizontal, 6)
            } else if let status {
                Text(status).font(.footnote).foregroundStyle(Tok.muted).padding(.horizontal, 6)
            }
            if !tray.isEmpty {
                AttachmentChips(tray: tray).padding(.horizontal, 4)
            }
            if busyCLI, let t = draftID, reachable {
                DoneBellChip(threadID: t).padding(.horizontal, 6)
            }
            DictationBar(dictation: dictation, text: $text)
            if bashMode || (focused && (text.isEmpty || histIdx >= 0)) {
                HStack(spacing: 8) {
                    if bashMode {
                        Text("BASH").font(Typo.mono(11, .bold)).tracking(0.5)
                            .padding(.horizontal, 6).padding(.vertical, 2)
                            .background(Tok.needSoft, in: RoundedRectangle(cornerRadius: 4))
                            .foregroundStyle(Tok.need)
                        Text("in the terminal").font(.footnote).foregroundStyle(Tok.fg2).lineLimit(1)
                    } else {
                        Text("Earlier prompts").font(.footnote).foregroundStyle(Tok.fg2).lineLimit(1)
                    }
                    Spacer()
                    // Up and Down walk earlier ! commands like a shell; Down past the newest restores what was typed.
                    Button { walkHistory(1) } label: { Image(systemName: "chevron.up") }
                        .buttonStyle(.am(.ghost)).accessibilityLabel("Older command")
                        .disabled(histIdx + 1 >= histList.count)
                    Button { walkHistory(-1) } label: { Image(systemName: "chevron.down") }
                        .buttonStyle(.am(.ghost)).accessibilityLabel("Newer command")
                        .disabled(histIdx < 0)
                    if bashMode, let onHistory {
                        Button(action: onHistory) { Image(systemName: "clock.arrow.circlepath") }
                            .buttonStyle(.am(.ghost)).accessibilityLabel("Command history")
                    } else if !bashMode {
                        Button { showPrompts = true } label: { Image(systemName: "clock.arrow.circlepath") }
                            .buttonStyle(.am(.ghost)).accessibilityLabel("Earlier prompts")
                    }
                }
                .padding(.horizontal, 6)
            }
            if !slashHits.isEmpty {
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(slashHits, id: \.0) { c in
                        Button { text = "/\(c.0) " } label: {
                            HStack(spacing: 8) {
                                Text("/\(c.0)").font(Typo.mono(13.5, .semibold)).foregroundStyle(Tok.fg)
                                Text(c.1).font(.footnote).foregroundStyle(Tok.muted).lineLimit(1)
                                Spacer(minLength: 0)
                            }
                            .padding(.horizontal, 12).padding(.vertical, 8).contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                    }
                }
                .background(Tok.panel, in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).stroke(Tok.line, lineWidth: 1))
                .padding(.horizontal, 4)
                .task(id: agent + cwd) {
                    guard skillCmds.isEmpty, !cwd.isEmpty,
                          let list = try? await api.skills(agent: agentKey(agent), cwd: cwd) else { return }
                    skillCmds = list.compactMap { s in
                        let name = (s.insert ?? "/" + s.name).trimmingCharacters(in: CharacterSet(charactersIn: "/$ "))
                        return name.isEmpty ? nil : (name, s.description ?? "skill")
                    }
                }
            }
            if !text.isEmpty || clearedText != nil {
                HStack(spacing: 8) {
                    Spacer()
                    if text.isEmpty, let c = clearedText {
                        Button("Undo clear") { text = c; clearedText = nil }.buttonStyle(.am(.ghost))
                    } else {
                        Button { copyToPasteboard(text) } label: { Label("Copy", systemImage: "doc.on.doc") }
                            .buttonStyle(.am(.ghost))
                        Button {
                            let c = text
                            clearedText = c; text = ""
                            Task { try? await Task.sleep(for: .seconds(5)); if clearedText == c { clearedText = nil } }
                        } label: { Label("Clear", systemImage: "xmark.circle") }
                            .buttonStyle(.am(.ghost))
                    }
                }
                .font(.footnote).padding(.horizontal, 6)
            }
            HStack(alignment: .bottom, spacing: 6) {
                AttachButton(tray: tray)
                    .disabled(!reachable)
                HStack(alignment: .bottom, spacing: 0) {
                    TextField(placeholder, text: $text, axis: .vertical)
                        .font(bashMode ? .body.monospaced() : .body)
                        .foregroundStyle(Tok.fg)
                        .lineLimit(1...6)
                        .focused($focused)
                        .disabled(!reachable)
                        .padding(.leading, 14).padding(.vertical, 10)
                        .frame(minHeight: 44)
                    MicButton(dictation: dictation, text: $text, size: 40)
                        .disabled(!reachable)
                        .padding(.trailing, 2).padding(.bottom, 2)
                }
                .background(bashMode ? Tok.needSoft : Tok.panel, in: RoundedRectangle(cornerRadius: 22, style: .continuous))
                .overlay(RoundedRectangle(cornerRadius: 22, style: .continuous)
                    .stroke(bashMode ? Tok.need : focused ? Tok.accent : Tok.line2, lineWidth: 1))
                if busyCLI, sessionID != nil, reachable {
                    // Esc in the terminal stops the turn, or closes an open prompt.
                    Button { Task { await pressEsc() } } label: {
                        Image(systemName: "stop.fill").font(.system(size: 14, weight: .bold))
                            .foregroundStyle(Tok.fg)
                            .frame(width: 36, height: 36)
                            .background(Tok.panel3, in: Circle())
                            .frame(width: 44, height: 44)
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .disabled(acting)
                    .accessibilityLabel("Stop with Esc")
                }
                Button { Task { await send() } } label: {
                    ZStack {
                        Circle().fill(canSend ? Tok.accent : Tok.panel3)
                        if sending {
                            ProgressView().controlSize(.small).tint(Tok.onAccent)
                        } else {
                            Image(systemName: "arrow.up").font(.system(size: 17, weight: .bold))
                                .foregroundStyle(canSend ? Tok.onAccent : Tok.muted)
                        }
                    }
                    .frame(width: 36, height: 36)
                    .frame(width: 44, height: 44)
                    .contentShape(Circle())
                }
                .buttonStyle(.plain)
                .disabled(!canSend)
                .accessibilityLabel(busyCLI && drivenID == nil ? "Queue" : "Send")
            }
            .opacity(reachable ? 1 : 0.6)
        }
        .padding(.horizontal, 10)
        .padding(.top, 8).padding(.bottom, 6)
        .background {
            Rectangle().fill(.bar).overlay(alignment: .top) { Rectangle().fill(Tok.line).frame(height: 0.5) }
                .ignoresSafeArea(edges: .bottom)
        }
        .sensoryFeedback(.impact(weight: .medium), trigger: sentCount)
        .sensoryFeedback(.selection, trigger: bashMode)
        .task(id: bashMode) { if bashMode { await loadHistory() } else if histBash { histIdx = -1 } }
        .task(id: focused) { if focused { await loadPrompts() } }
        .sheet(item: $slashPanel) { p in
            NavigationStack {
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        if p.menu { Text("A menu is open in the terminal. Answer it there.").font(.callout).foregroundStyle(.secondary) }
                        Text(p.text).font(.system(.footnote, design: .monospaced)).textSelection(.enabled)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
                }
                .navigationTitle(p.command)
                .navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { slashPanel = nil } } }
            }
            .presentationDetents([.medium, .large])
        }
        .sheet(isPresented: $showPrompts) {
            PromptHistorySheet(session: sessionID) { picked in
                text = picked
                histIdx = -1
                focused = true
            }
        }
        .onChange(of: text) { _, _ in saveDraft() }
        .onReceive(NotificationCenter.default.publisher(for: .amFillComposer)) { n in
            guard reachable, let code = n.userInfo?["code"] as? String else { return }
            text = insertFor(code, lang: n.userInfo?["lang"] as? String ?? "", bash: bashCapable, cur: text)
            focused = true
        }
        .onChange(of: tray.ids) { _, _ in saveDraft() }
        .task(id: target) {
            tray.threadID = drivenID
            #if DEBUG
            if DebugRoute.sample {
                let s = DebugRoute.screen ?? ""
                text = s.contains("bash") ? "!git status --short" : "Use the screenshot for the spacing."
                if s.contains("voice") { dictation.sample(s.contains("clean") ? .review : .listening); text = dictation.joined }
                if !s.contains("bash") && !s.contains("voice") { tray.addSamples() }
                return
            }
            #endif
            restoreDraft()
            if let sid = sessionID { await refreshLoop(sid) }
        }
    }

    /// This session's ! commands newest first, then other sessions' recent ones; kept 20 s.
    private func loadHistory() async {
        guard let sid = sessionID, Date().timeIntervalSince(histAt) > 20 else { return }
        async let mine = try? api.sessionCommands(sid)
        async let all = try? api.recentCommands()
        let byNew = { (x: [CmdItem]) in x.sorted { $0.at > $1.at }.map(\.cmd) }
        var seen = Set<String>()
        hist = (byNew(await mine ?? []) + byNew(await all ?? [])).filter { !$0.isEmpty && seen.insert($0).inserted }
        histAt = Date()
    }

    /// The list Up and Down walk: ! commands in bash mode, else earlier prompts.
    private var histList: [String] { (histIdx >= 0 ? histBash : bashMode) ? hist.map { "!" + $0 } : promptHist }

    private func loadPrompts() async {
        guard Date().timeIntervalSince(promptAt) > 20 else { return }
        async let mine = try? api.prompts(session: sessionID, limit: 100)
        async let all = try? api.prompts(limit: 300)
        var seen = Set<String>()
        let mineList = sessionID == nil ? [] : (await mine ?? [])
        promptHist = (mineList + (await all ?? [])).map(\.text).filter { seen.insert($0).inserted }
        promptAt = Date()
    }

    private func walkHistory(_ step: Int) {
        if histIdx == -1 { histBash = bashMode }
        let list = histList
        let next = histIdx + step
        guard next >= -1, next < list.count else { return }
        if histIdx == -1 { histTyped = text }
        histIdx = next
        text = next == -1 ? histTyped : list[next]
    }

    private func restoreDraft() {
        let key = draftKey
        guard restoredKey != key else { return }
        if let d = DraftStore.shared.get(key) {
            text = d.text
            tray.clear()
            tray.restore(d.attachments)
        } else {
            text = ""
            tray.clear()
        }
        restoredKey = key
    }

    private func saveDraft() {
        guard restoredKey == draftKey else { return }
        DraftStore.shared.set(draftKey, text: text, attachments: tray.uploadedList)
    }

    private var placeholder: String {
        if !reachable { return closed ? "Thread closed" : "Type in the terminal on the Mac" }
        if bashCapable && busyCLI == false { return "Message \(agentName(agent)), or ! for bash" }
        if drivenID != nil { return "Message \(agentName(agent))" }
        return busyCLI ? "Queue a message" : "Message \(agentName(agent))"
    }

    // Queued messages and running ! commands above the composer, oldest first.
    private func queueTitle(_ qi: [CLIQueueItem]) -> String {
        qi.allSatisfy { $0.kind == "bash" } ? "Terminal · \(qi.count)" : "\(cliName) queue · \(qi.count)"
    }

    private func queueLabel(_ it: CLIQueueItem, _ i: Int) -> some View {
        let bash = it.kind == "bash"
        let state = bash ? (it.status == "waiting" ? "WAITING" : "RUNNING") : "QUEUED"
        return HStack(spacing: 6) {
            Text(state).font(Typo.ui(11, .semibold)).tracking(0.5)
            if !bash { Text(i == 0 ? "next" : "#\(i + 1)").font(Typo.mono(11)) }
            Text(bash ? "in the terminal" : "in \(cliName)")
        }
        .font(Typo.xs)
        .foregroundStyle(Tok.muted)
    }

    /// One compact line per queued item, so the chat keeps its room; at most three show.
    private func queueBox(_ q: CLIQueueView, _ qi: [CLIQueueItem]) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Eyebrow(text: queueTitle(qi))
                Spacer()
                if qi.contains(where: { $0.kind != "bash" }) {
                    Button("Send now") { Task { await act("send-now", nil) } }
                        .buttonStyle(.am(.ghost))
                        .disabled(acting || !q.reachable)
                }
            }
            ForEach(Array(qi.prefix(3).enumerated()), id: \.element.id) { i, it in
              VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 8) {
                    queueLabel(it, qi[..<i].filter { $0.kind != "bash" }.count)
                        .fixedSize()
                    Text(it.text.isEmpty ? "(no text, for example an image)" : it.text)
                        .font(Typo.mono(13))
                        .foregroundStyle(Tok.fg)
                        .lineLimit(1).truncationMode(.tail)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    if it.kind == "bash" || q.canRemove {
                        // A sent ! command is only forgotten here; a queued message is removed from the CLI queue.
                        Button { Task { await act(it.kind == "bash" ? "forget" : "remove", it.id) } } label: {
                            Image(systemName: "xmark").font(.system(size: 11, weight: .semibold)).foregroundStyle(Tok.muted)
                                .frame(width: 44, height: 44).contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .disabled(acting)
                        .accessibilityLabel("Remove")
                    }
                }
                if let live = it.live, !live.isEmpty {
                    Text(live).font(Typo.mono(11.5)).foregroundStyle(Tok.fg2)
                        .lineLimit(8).frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                }
              }
                .padding(.horizontal, 10).padding(.vertical, 6)
                .background(Tok.panel3, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
                .overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).stroke(Tok.run.opacity(0.35), lineWidth: 1))
                .contextMenu {
                    Text(it.text)
                    if it.kind != "bash" {
                        if q.canRemove {
                            Button("Remove", role: .destructive) { Task { await act("remove", it.id) } }.disabled(acting)
                        } else {
                            Text(removeWhy(q, count: qi.count))
                        }
                    }
                }
            }
            if qi.count > 3 {
                Text("+\(qi.count - 3) more").font(Typo.xs).foregroundStyle(Tok.muted)
            }
            if let note = q.note, !note.isEmpty {
                Text(note).font(Typo.ui(12.5)).foregroundStyle(Tok.fg2)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 10).padding(.vertical, 7)
                    .overlay(RoundedRectangle(cornerRadius: 10).stroke(Tok.line2, style: StrokeStyle(lineWidth: 1, dash: [3, 3])))
            }
        }
        .padding(.horizontal, 4)
    }

    private func removeWhy(_ q: CLIQueueView, count: Int) -> String {
        if !q.reachable { return q.removeWhy ?? "agent-monitor cannot reach this terminal." }
        if count > 1 {
            return "Remove works only when exactly one message is queued: \(cliName) gives queued messages back only all at once."
        }
        return q.removeWhy ?? "Remove is not available now."
    }

    // MARK: actions

    private func send() async {
        guard !sending else { return }
        sending = true
        await dictation.settle(into: $text)
        sending = false
        let body = text.trimmingCharacters(in: .whitespacesAndNewlines)
        let ids = tray.ids
        let key = draftKey
        guard !body.isEmpty || !ids.isEmpty, !sending else { return }
        sending = true
        defer { sending = false }
        do {
            if let d = drivenID {
                try await api.drivenSend(d, text: body, attachments: ids)
                status = nil
            } else if let sid = sessionID {
                let sent = try await api.cliSendFull(sid, text: body, attachments: ids)
                status = sent.queued ? "\(cliName) is busy and queued the message." : nil
                if !body.isEmpty, !body.hasPrefix("/"), !body.hasPrefix("!") {
                    NotificationCenter.default.post(name: .chatSent, object: nil,
                                                    userInfo: ["sid": sid, "text": body, "queued": sent.queued || busyCLI])
                }
                if !sent.panel.isEmpty {
                    slashPanel = SlashPanel(command: String(body.split(separator: " ").first ?? ""), text: sent.panel, menu: sent.menu)
                }
                await loadQueue(sid)
            }
            // Keep text typed while the send was in flight.
            histIdx = -1
            histAt = .distantPast
            if text.trimmingCharacters(in: .whitespacesAndNewlines) == body {
                text = ""
                tray.clear()
                DraftStore.shared.clear(key)
            } else if restoredKey == key {
                tray.clear()
                saveDraft()
            }
            sentCount += 1
            error = nil
        } catch let e as APIError where !ids.isEmpty && (e.status == 400 || e.status == 415) {
            error = "This daemon did not take the attachments (\(e.message)). Update agent-monitor on the Mac, or send text only."
            status = nil
        } catch {
            // 409: a prompt is open in the terminal, or the terminal cannot be reached.
            self.error = error.localizedDescription
            status = nil
        }
    }

    private func pressEsc() async {
        guard let sessionID else { return }
        acting = true
        defer { acting = false }
        do {
            try await api.keys(sessionID, ["Escape"])
            error = nil
            status = "Esc sent. \(cliName) stops its turn."
        } catch {
            self.error = error.localizedDescription
        }
    }

    @discardableResult
    private func act(_ action: String, _ id: String?) async -> Bool {
        guard let sessionID else { return false }
        acting = true
        defer { acting = false }
        var ok = false
        do {
            try await api.cliQueueAction(sessionID, action: action, itemID: id)
            error = nil
            ok = true
            switch action {
            case "send-now": status = "\(cliName) stops the turn and sends the queue."
            case "forget", "clear-draft": status = "Cleared."
            default: status = "Removed from the queue."
            }
        } catch {
            self.error = error.localizedDescription
        }
        await loadQueue(sessionID)
        return ok
    }

    private func loadQueue(_ sid: String) async {
        queue = try? await api.cliQueue(sid)
    }

    /// Queue every 5 s, pane reachability every 15 s, while the chat is visible.
    private func refreshLoop(_ sid: String) async {
        var n = 0
        while !Task.isCancelled {
            if n % 3 == 0 { pane = try? await api.watchedPane(sid) }
            if n % 12 == 0, let u = try? await api.usageLimits() {
                credits = u.agents?.first { $0.agent == agentKey(agent) }?.creditsLine
            }
            await loadQueue(sid)
            n += 1
            // A running ! command shows its terminal output, so it refreshes faster.
            let live = queue?.items?.contains { $0.kind == "bash" && $0.status == "running" } ?? false
            try? await Task.sleep(nanoseconds: live ? 1_500_000_000 : 5_000_000_000)
        }
    }
}

// MARK: - Terminal question

// A question or plan that Claude Code shows in its pane with no held approval;
// answered by keys the daemon sends to the pane (POST /api/session/{id}/answer).
struct TermQuestionCard: View {
    var api: APIClient = .shared
    @Environment(AppStore.self) private var store
    let sessionID: String
    /// From the needs-you list: the card then shows that question and does not poll.
    var item: NeedsYouItem? = nil

    @State private var polled: TermQuestion?
    private var q: TermQuestion? { item?.question ?? polled }
    @State private var picked: [Int: [Int]] = [:]
    @State private var other: [Int: String] = [:]
    @State private var feedback = ""
    @State private var busy = false
    @State private var sentFor: String?
    @State private var error: String?
    @State private var sentAtLocal: Int64 = 0
    @State private var showScreen = false
    @State private var answered = 0
    /// The question the user chose to answer again after it was not taken.
    @State private var reopened: String?

    var body: some View {
        Group {
            // A held approval for this session already shows its own card.
            if let q, q.shown, item != nil || store.approvals(forSession: sessionID).isEmpty {
                card(q)
            }
        }
        .sheet(isPresented: $showScreen) { TerminalScreen(sessionID: sessionID) }
        .sensoryFeedback(.success, trigger: answered)
        .task(id: sessionID) {
            guard item == nil else { return }
            while !Task.isCancelled {
                polled = try? await api.termQuestion(sessionID)
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }

    private func card(_ q: TermQuestion) -> some View {
        let sent = reopened != (q.toolUseId ?? "?") && ((q.sentAt ?? 0) > 0 || (sentFor != nil && sentFor == q.toolUseId))
        let canSend = item?.answerable ?? (q.sendable && q.onScreen)
        let why = item?.why ?? q.why
        let off = busy || sent || !canSend
        return VStack(alignment: .leading, spacing: 10) {
            if let item {
                HStack(spacing: 6) {
                    AgentGlyph(agent: item.agent ?? "claude", size: 14)
                    Text(item.project).font(Typo.ui(14, .semibold)).foregroundStyle(Tok.fg).lineLimit(1)
                    Spacer()
                    Text(relativeTime(item.at)).font(Typo.xs).foregroundStyle(Tok.muted)
                }
            }
            HStack(spacing: 8) {
                Pill(text: "Needs you", tone: .need)
                Text(q.isPlan ? "Plan ready in the terminal" : "\(agentName(item?.agent ?? "claude")) asks in the terminal")
                    .font(Typo.ui(13, .medium)).foregroundStyle(Tok.fg)
            }
            if q.isPlan { planBody(q, off: off) } else { questionsBody(q, off: off) }
            if sent {
                Text("Sent to the terminal. This card closes when \(agentName(item?.agent ?? "claude")) takes the answer.")
                    .font(Typo.ui(12.5)).foregroundStyle(Tok.ok)
                SentLate(sentAt: q.sentAt ?? sentAtLocal, retry: { reopened = q.toolUseId ?? "?"; sentFor = nil }, look: { showScreen = true })
            } else if !canSend {
                Text(whyText(why))
                    .font(Typo.ui(12.5)).foregroundStyle(Tok.fg2)
            }
            if let error { Text(error).font(Typo.ui(12.5)).foregroundStyle(Tok.err) }
        }
        .askCard()
    }

    private func questionsBody(_ q: TermQuestion, off: Bool) -> some View {
        let qs = q.questions ?? []
        let step = min(q.step ?? 0, max(qs.count - 1, 0))
        let rest = qs.count - step
        let single = rest == 1 && !(qs.last?.multiSelect ?? false)
        return VStack(alignment: .leading, spacing: 12) {
            ForEach(Array(qs.enumerated()), id: \.offset) { i, x in
                let multi = x.multiSelect ?? false
                let done = i < step
                VStack(alignment: .leading, spacing: 6) {
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        if let h = x.header, !h.isEmpty { Pill(text: h) }
                        Text(x.question).font(.body.weight(.semibold)).foregroundStyle(done ? Tok.muted : Tok.fg)
                    }
                    if done {
                        Label("Answered in the terminal", systemImage: "checkmark.circle")
                            .font(Typo.sm).foregroundStyle(Tok.muted)
                    } else {
                    ForEach(Array(x.options.enumerated()), id: \.offset) { j, o in
                        let on = (picked[i] ?? []).contains(j)
                        Button {
                            var cur = picked[i] ?? []
                            if multi {
                                if on { cur.removeAll { $0 == j } } else { cur.append(j); cur.sort() }
                            } else {
                                cur = [j]
                                other[i] = ""
                            }
                            picked[i] = cur
                            if single { submit(q) }
                        } label: {
                            AskOption(label: o.label, detail: o.description, on: on, multi: multi)
                        }
                        .buttonStyle(.plain)
                        .disabled(off)
                    }
                    if multi {
                        Text("To type your own answer to this question, use the terminal.")
                            .font(Typo.sm).foregroundStyle(Tok.muted)
                    } else {
                        VoiceField(placeholder: "Other: type or say an answer", text: Binding(
                            get: { other[i] ?? DraftStore.shared.text(termKey(q, i)) },
                            set: {
                                other[i] = $0
                                DraftStore.shared.set(termKey(q, i), text: $0)
                                if !$0.isEmpty { picked[i] = [] }
                            }), disabled: off) { submit(q) }
                    }
                    }
                }
            }
            let typed = other.values.contains { !$0.trimmingCharacters(in: .whitespaces).isEmpty }
            if !single || typed {
                let done = answers(q).compactMap { $0 }.count
                Button { submit(q) } label: {
                    Text(rest > 1 ? "Send (\(done)/\(rest))" : "Send")
                }
                .buttonStyle(.am(.pri, big: true, full: true))
                .disabled(off || done < rest)
            }
        }
    }

    private func termKey(_ q: TermQuestion, _ i: Int) -> String { "term:\(sessionID):\(q.toolUseId ?? ""):\(i)" }

    /// Answers for the questions from the terminal's current step on; earlier ones were answered there.
    private func answers(_ q: TermQuestion) -> [TermAnswer?] {
        (q.questions ?? []).enumerated().dropFirst(q.step ?? 0).map { i, x in
            let t = (other[i] ?? DraftStore.shared.text(termKey(q, i))).trimmingCharacters(in: .whitespacesAndNewlines)
            let p = picked[i] ?? []
            if x.multiSelect ?? false { return p.isEmpty ? nil : TermAnswer(options: p) }
            if !t.isEmpty { return TermAnswer(text: t) }
            return p.first.map { TermAnswer(option: $0) }
        }
    }

    private func submit(_ q: TermQuestion) {
        let a = answers(q)
        guard !a.isEmpty, a.allSatisfy({ $0 != nil }) else { return }
        send(q, TermAnswerBody(answers: a.compactMap { $0 }))
    }

    private func planBody(_ q: TermQuestion, off: Bool) -> some View {
        let opts = q.planOptions ?? []
        let fb = opts.first { $0.feedback ?? false }
        return VStack(alignment: .leading, spacing: 8) {
            PlanBox(text: q.plan ?? "(empty plan)")
            if fb != nil {
                TextField("What should change in the plan?", text: $feedback, axis: .vertical)
                    .lineLimit(1...4)
                    .amField()
                    .disabled(off)
            }
            ForEach(opts, id: \.n) { o in
                let isFb = o.feedback ?? false
                Button {
                    send(q, TermAnswerBody(text: isFb ? feedback : nil, planOption: o.n))
                } label: { Text(o.label) }
                .buttonStyle(.am(o.n == 1 ? .allow : .normal, big: true, full: true))
                .disabled(off || (isFb && feedback.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty))
            }
        }
    }

    private func send(_ q: TermQuestion, _ body: TermAnswerBody) {
        guard !busy else { return }
        var b = body
        b.toolUseId = q.toolUseId
        busy = true
        Task {
            defer { busy = false }
            do {
                try await api.answerTerm(sessionID, b)
                DraftStore.shared.clear(prefix: "term:\(sessionID):\(q.toolUseId ?? ""):")
                sentFor = q.toolUseId
                sentAtLocal = Int64(Date().timeIntervalSince1970 * 1000)
                answered += 1
                reopened = nil
                error = nil
            } catch let e as APIError where e.status == 409 {
                error = "Answered elsewhere."
            } catch let e as APIError where e.status == 422 {
                error = "Not sent. " + whyText(e.message)
            } catch {
                self.error = error.localizedDescription
            }
            if item != nil { await store.loadNeedsYou() } else { polled = try? await api.termQuestion(sessionID) }
        }
    }
}

/// The daemon's reason as a sentence; without one, the terminal is the place to answer.
func whyText(_ why: String?) -> String {
    let w = (why ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    guard let first = w.first else { return "Answer it in the terminal." }
    return first.uppercased() + w.dropFirst() + (w.hasSuffix(".") ? "" : ".")
}

/// Built-in slash commands of the terminal agents, with a short label.
func agentSlashCommands(_ agent: String) -> [(String, String)] {
    switch agent {
    case "claude":
        return [("compact", "Compact the conversation"), ("clear", "Clear the conversation"), ("context", "Show context use"),
                ("cost", "Show the session cost"), ("usage", "Show plan usage"), ("review", "Review a pull request"),
                ("init", "Write a CLAUDE.md"), ("todos", "Show the todo list"), ("exit", "Exit the session")]
    case "codex":
        return [("compact", "Compact the conversation"), ("new", "Start a new chat"), ("diff", "Show the git diff"),
                ("init", "Write an AGENTS.md"), ("exit", "Exit the session")]
    default:
        return []
    }
}

/// The screen a Claude slash command drew in the terminal; it never reaches the transcript.
struct SlashPanel: Identifiable {
    let id = UUID()
    let command: String
    let text: String
    let menu: Bool
}

extension Notification.Name {
    /// A message the composer typed into a terminal; the chat shows it as pending until the transcript has it.
    static let chatSent = Notification.Name("am.chatSent")
}

/// After 20 s with the answer not taken: look at the terminal; after 90 s (the daemon's resend lock) answer again.
private struct SentLate: View {
    let sentAt: Int64
    let retry: () -> Void
    let look: () -> Void
    var body: some View {
        TimelineView(.periodic(from: .now, by: 5)) { tl in
            let age = tl.date.timeIntervalSince1970 * 1000 - Double(sentAt)
            if sentAt > 0, age > 20_000 {
                HStack(spacing: 8) {
                    Text("Not taken yet.").font(Typo.ui(12.5)).foregroundStyle(Tok.fg2)
                    Spacer()
                    Button("Terminal", action: look).buttonStyle(.am(.ghost))
                    if age > 90_000 { Button("Answer again", action: retry).buttonStyle(.am(.ghost)) }
                }
            }
        }
    }
}
