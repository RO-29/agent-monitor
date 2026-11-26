import SwiftUI

// Home: what needs you first (answerable in one tap), then what is running, then recent threads.
struct WorkspaceView: View {
    @Environment(AppStore.self) private var store
    @State private var query = ""
    @State private var settledLimit = 100

    var body: some View {
        @Bindable var store = store
        NavigationStack(path: $store.workspacePath) {
            List {
                if let err = store.threadsError {
                    Section { InlineError(message: err).listRowSeparator(.hidden) }
                        .listRowBackground(Tok.bg)
                }
                if let at = store.needsYouAt { StaleNote(at: at) }
                if query.isEmpty { needsYou }
                let searching = !query.isEmpty
                let working = filtered(store.threads(in: .working))
                if !working.isEmpty {
                    FoldSection(ThreadGroup.working.label, key: "home.working", forceOpen: searching) {
                        Text("\(working.count)")
                    } content: {
                        ForEach(working) { t in row(t, .working) }
                    }
                }
                if searching {
                    let need = filtered(store.threads(in: .need))
                    if !need.isEmpty {
                        FoldSection(ThreadGroup.need.label, key: "home.need", tint: Tok.need, forceOpen: true) {
                            Text("\(need.count)")
                        } content: {
                            ForEach(need) { t in row(t, .need) }
                        }
                    }
                }
                let ready = filtered(store.threads(in: .ready))
                if !ready.isEmpty {
                    FoldSection(ThreadGroup.ready.label, key: "home.ready", forceOpen: searching) {
                        Text("\(ready.count)")
                    } content: {
                        ForEach(ready) { t in row(t, .ready) }
                    }
                }
                let settled = filtered(store.threads(in: .settled))
                if !settled.isEmpty {
                    FoldSection(ThreadGroup.settled.label, key: "home.settled", foldedByDefault: true, forceOpen: searching) {
                        Text("\(settled.count)")
                    } content: {
                        ForEach(settled.prefix(settledLimit)) { t in row(t, .settled) }
                        if settled.count > settledLimit {
                            Button("Show older (\(settled.count - settledLimit) more)") { settledLimit += 100 }
                                .font(.subheadline.weight(.medium)).frame(minHeight: 44)
                                .listRowBackground(Tok.bg)
                        }
                    }
                }
                // In the list, not an overlay, so it never covers the Usage section.
                if !searching && store.threads.isEmpty && store.threadsError == nil && store.approvals.isEmpty {
                    Section {
                        if store.threadsLoaded {
                            ContentUnavailableView {
                                Label("No threads yet", systemImage: "bubble.left.and.text.bubble.right")
                            } description: {
                                Text("Agent sessions on the Mac appear here. Start one with the compose button.")
                            } actions: {
                                Button("New Thread") { store.newThread() }.buttonStyle(.borderedProminent)
                            }
                        } else {
                            HStack(spacing: 8) {
                                ProgressView()
                                Text("Loading threads").font(.footnote).foregroundStyle(Tok.muted)
                            }
                            .frame(maxWidth: .infinity)
                            .padding(.vertical, 12)
                        }
                    }
                    .listRowBackground(Tok.bg)
                    .listRowSeparator(.hidden)
                }
                if searching {
                    MessageSearchRows(query: query)
                }
                if !searching {
                    FoldSection("Usage", key: "home.usage") {
                        UsageStrip()
                            .listRowBackground(Tok.bg)
                            .listRowSeparator(.hidden)
                    }
                }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
            .background(Tok.bg)

            .searchable(text: $query, prompt: "Search threads")
            .navigationTitle("Threads")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { ConnectionChip(onlyWhenDown: true) }
                ToolbarItem(placement: .topBarTrailing) { ComposeButton() }
                if store.manyMachines {
                    ToolbarItem(placement: .topBarTrailing) { MachineFilterMenu() }
                }
            }
            .navigationDestination(for: String.self) { id in
                if let tid = LinkedAgentsPage.threadID(fromRoute: id) {
                    LinkedAgentsPage(threadID: tid)
                } else {
                    ThreadChatView(threadID: id)
                }
            }
            .refreshable {
                await store.loadFleet()
                await store.loadThreads()
                await store.loadApprovals()
            }
            .sheet(isPresented: Binding(get: { store.approvalSheet != nil }, set: { if !$0 { store.approvalSheet = nil } })) {
                if let id = store.approvalSheet {
                    ApprovalSheet(id: id)
                        .presentationDetents([.medium, .large])
                        .presentationDragIndicator(.visible)
                        .presentationBackground(Tok.bg)
                }
            }
            .task {
                // Threads of the other machines: every 15 s while the list shows.
                while !Task.isCancelled {
                    await store.loadFleet()
                    try? await Task.sleep(nanoseconds: 15_000_000_000)
                }
            }
            .task {
                // Session events reload the list too; this catches what no event announces.
                while !Task.isCancelled {
                    await store.refreshThreadsIfStale(30)
                    try? await Task.sleep(nanoseconds: 30_000_000_000)
                }
            }
        }
    }

    /// Hook approvals as one-tap cards, then threads waiting in a terminal; hidden when nothing waits.
    @ViewBuilder private var needsYou: some View {
        let cards = store.approvals
        let waiting = store.threads(in: .need).filter { store.approvals(forThread: $0).isEmpty }
        let other = store.permissions.count
        let remote = store.remoteForHome.flatMap { m in (store.remoteNeeds[m.id] ?? []).map { (m, $0) } }
        if !cards.isEmpty || !waiting.isEmpty || other > 0 || !remote.isEmpty {
            FoldSection(ThreadGroup.need.label, key: "home.need", tint: Tok.need) {
                // The Approvals tab count, but never fewer than the cards shown (the server list can lag).
                Text("\(max(store.approvalsCount, cards.count + waiting.count + other) + remote.count)")
            } content: {
                ForEach(remote, id: \.1.id) { m, it in
                    RemoteNeedCard(machine: m, item: it)
                        .listRowBackground(Tok.bg)
                        .listRowSeparator(.hidden)
                        .listRowInsets(EdgeInsets(top: 6, leading: 16, bottom: 6, trailing: 16))
                }
                ForEach(cards) { a in
                    QuickApprovalCard(a: a)
                        .listRowBackground(Tok.bg)
                        .listRowSeparator(.hidden)
                        .listRowInsets(EdgeInsets(top: 6, leading: 16, bottom: 6, trailing: 16))
                }
                ForEach(waiting) { t in row(t, .need) }
                if other > 0 {
                    Button { store.tab = .approvals } label: {
                        Label("\(other) MCP request\(other == 1 ? "" : "s") in Approvals", systemImage: "checkmark.shield")
                            .font(.subheadline.weight(.medium)).foregroundStyle(Tok.need)
                            .frame(minHeight: 44)
                    }
                    .listRowBackground(Tok.bg)
                }
            }
        }
    }

    private var noMatches: Bool {
        ThreadGroup.allCases.allSatisfy { filtered(store.threads(in: $0)).isEmpty }
    }

    private func row(_ t: V3Thread, _ g: ThreadGroup) -> some View {
        let resume = resumeCommand(tool: t.agent, sessionID: nativeID(t), cwd: t.cwd)
        return ThreadRow(t: t, group: g)
            .background { NavigationLink(value: t.id) { EmptyView() }.opacity(0) }
            .listRowBackground(Tok.bg)
            .listRowSeparatorTint(Tok.line)
            .listRowInsets(EdgeInsets(top: 0, leading: 12, bottom: 0, trailing: 16))
            .swipeActions(edge: .trailing) {
                if let resume {
                    Button { copyToPasteboard(resume, label: "Resume command copied") } label: {
                        Label("Resume", systemImage: "terminal")
                    }
                    .tint(Tok.accent)
                }
            }
            .swipeActions(edge: .leading) {
                Button { store.newThread(in: t.cwd) } label: { Label("New Here", systemImage: "square.and.pencil") }
                    .tint(Tok.ok)
            }
            .contextMenu {
                Button { store.workspacePath = [t.id] } label: { Label("Open", systemImage: "bubble.left") }
                Button { store.newThread(in: t.cwd) } label: { Label("New Thread in \(t.repoName)", systemImage: "square.and.pencil") }
                if let resume {
                    Button { copyToPasteboard(resume, label: "Resume command copied") } label: { Label("Copy Resume Command", systemImage: "terminal") }
                }
                if !t.cwd.isEmpty {
                    Button { copyToPasteboard(t.cwd, label: "Path copied") } label: { Label("Copy Folder Path", systemImage: "folder") }
                }
            } preview: {
                ThreadRow(t: t, group: g).environment(store).padding(8).frame(width: 340).background(Tok.bg)
            }
    }

    private func nativeID(_ t: V3Thread) -> String {
        String((t.latestSession ?? "").split(separator: ":", maxSplits: 1).last ?? "")
    }

    private func filtered(_ rows: [V3Thread]) -> [V3Thread] {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        guard !q.isEmpty else { return rows }
        return rows.filter { t in
            [t.name, t.repoName, t.ref ?? "", t.agent, t.machine ?? ""].contains { $0.lowercased().contains(q) }
        }
    }
}

/// Picks which machine's threads the list shows; only there with two or more machines.
struct MachineFilterMenu: View {
    @Environment(AppStore.self) private var store
    var body: some View {
        @Bindable var store = store
        Menu {
            Picker("Machine", selection: $store.machineFilter) {
                Text("All Machines").tag("")
                ForEach(store.fleetMachines) { m in
                    Text(m.isSelf ? "Main Mac" : m.online ? m.name : "\(m.name) (offline)").tag(m.name)
                }
            }
        } label: {
            Image(systemName: "desktopcomputer")
                .symbolVariant(store.machineFilter.isEmpty ? .none : .fill)
        }
        .accessibilityLabel(store.machineFilter.isEmpty ? "All machines" : "Only \(store.machineFilter)")
    }
}

/// The compose button every tab shows in its navigation bar.
struct ComposeButton: View {
    @Environment(AppStore.self) private var store
    var body: some View {
        Button { store.newThread() } label: {
            Image(systemName: "square.and.pencil").fontWeight(.semibold)
        }
        .accessibilityLabel("New thread")
    }
}

/// Error line for list sections.
struct InlineError: View {
    @Environment(AppStore.self) private var store: AppStore?
    let message: String
    private var unauthorized: Bool { message.lowercased() == "unauthorized" || message.contains("HTTP 401") }
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label(unauthorized ? "The daemon needs this phone's token." : message, systemImage: "exclamationmark.triangle.fill")
                .font(.footnote)
                .foregroundStyle(Tok.err)
            if unauthorized, let store {
                Button("Pair in Settings") { store.tab = .settings }
                    .font(.footnote.weight(.semibold))
                    .buttonStyle(.borderless)
                    .padding(.leading, 26)
            }
        }
        .padding(.vertical, 4)
    }
}

struct ThreadRow: View {
    @Environment(AppStore.self) private var store
    let t: V3Thread
    let group: ThreadGroup

    private var dot: Color? {
        switch group {
        case .need: return Tok.need
        case .working: return Tok.run
        default: return t.isDriven && t.status == "error" ? Tok.err : nil
        }
    }

    /// One quiet line: what it waits for or does now, else the repo.
    private var subtitle: String {
        if group == .need, t.machine != nil {
            return t.state == .awaitingInput ? "Waiting for a reply" : "Needs you"
        }
        if group == .need {
            if let a = store.approvals(forThread: t).first { return a.kind == "permission" ? "Wants to run \(a.displayTool)" : a.summary }
            if t.isDriven && t.status == "error" { return "Stopped with an error" }
            if store.usesNeedsYou {
                let it = store.needsYouItem(forThread: t)
                switch it?.kind {
                case "mcp": return "Asks permission for a tool"
                case "question": return it?.question?.isPlan == true ? "Has a plan to review" : "Asks a question"
                case "prompt": return "Asks in the terminal"
                default: return "Needs you"
                }
            }
            let st = store.liveState(t)
            return st == .awaitingInput ? "Waiting for your reply" : "Waiting in the terminal"
        }
        if group == .working, let l = t.liveActivity, !l.isEmpty { return l }
        let place = t.isDriven ? "Thread" : (t.latestSession.flatMap { store.sessionsByID[$0] }?.placeText ?? "")
        return place.isEmpty ? t.repoName : "\(place) · \(t.repoName)"
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            ZStack {
                if let dot {
                    if group == .working { WorkingPulse() } else { StatusDot(color: dot, size: 9) }
                }
            }
            .frame(width: 12)
            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(t.name)
                        .font(.body.weight(group == .settled ? .regular : .medium))
                        .foregroundStyle(group == .settled ? Tok.fg2 : Tok.fg)
                        .lineLimit(2)
                    Spacer(minLength: 4)
                    Text(relativeTime(t.lastAt)).font(.subheadline).monospacedDigit().foregroundStyle(Tok.muted)
                }
                HStack(spacing: 6) {
                    AgentGlyph(agent: t.agent, size: 13)
                    if let m = t.machine {
                        Text(m)
                            .font(.caption2.weight(.semibold))
                            .padding(.horizontal, 5).padding(.vertical, 1)
                            .foregroundStyle(Tok.accent)
                            .background(Tok.accentSoft, in: RoundedRectangle(cornerRadius: 4))
                            .accessibilityLabel("on \(m)")
                    }
                    if group == .working || group == .need, subtitle != t.repoName {
                        Text(t.repoName).foregroundStyle(Tok.muted)
                        Text("·").foregroundStyle(Tok.muted)
                    }
                    Text(subtitle)
                        .foregroundStyle(group == .need ? Tok.need : group == .working ? Tok.fg2 : Tok.muted)
                        .lineLimit(1)
                }
                .font(.subheadline)
                .lineLimit(1)
            }
        }
        .padding(.vertical, 11)
        .frame(minHeight: 56)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityValue(group.label)
    }
}

/// Home search, below the thread matches: prompts that hold the words, each opening its thread.
private struct MessageSearchRows: View {
    @Environment(AppStore.self) private var store
    let query: String
    @State private var hits: [PromptRun] = []
    @State private var state = "idle"

    var body: some View {
        Section {
            if state == "idle" {
                Button { Task { await run() } } label: {
                    Label("Search messages for \u{201C}\(query)\u{201D}", systemImage: "text.magnifyingglass")
                        .frame(minHeight: 44)
                }
            } else if state == "busy" {
                HStack(spacing: 8) { ProgressView(); Text("Searching messages").foregroundStyle(Tok.muted) }.frame(minHeight: 44)
            } else if hits.isEmpty {
                Text(state == "error" ? "Could not search messages." : "No message holds \u{201C}\(query)\u{201D}.")
                    .font(.subheadline).foregroundStyle(Tok.muted).frame(minHeight: 44)
            } else {
                ForEach(hits) { h in
                    Button { if let s = h.sessionId { store.openThread(s) } } label: {
                        VStack(alignment: .leading, spacing: 3) {
                            Text(h.text).font(.subheadline).foregroundStyle(Tok.fg).lineLimit(3)
                            Text(((h.cwd ?? "") as NSString).lastPathComponent + (h.at.map { " · " + relativeTime($0) } ?? ""))
                                .font(.caption).foregroundStyle(Tok.muted)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .disabled(h.sessionId == nil)
                }
            }
        } header: {
            Text("Messages")
        }
        .listRowBackground(Tok.bg)
        .onChange(of: query) { _, _ in state = "idle"; hits = [] }
    }

    private func run() async {
        state = "busy"
        let q = query
        do {
            let r = try await APIClient.shared.prompts(query: q, limit: 40)
            // The user typed on while this ran: these hits are for older words.
            guard q == query else { return }
            hits = r
            state = "done"
        } catch {
            if q == query { state = "error" }
        }
    }
}

/// Says how old the list is when refreshes stop, so old rows never look live.
private struct StaleNote: View {
    let at: Date
    @Environment(\.scenePhase) private var phase
    var body: some View {
        TimelineView(.periodic(from: .now, by: 5)) { ctx in
            let age = ctx.date.timeIntervalSince(at)
            if phase == .active && age > 15 {
                Label("Last updated \(at.formatted(.relative(presentation: .named)))", systemImage: "clock.arrow.circlepath")
                    .font(.footnote).foregroundStyle(Tok.muted)
                    .listRowBackground(Tok.bg).listRowSeparator(.hidden)
            }
        }
    }
}
