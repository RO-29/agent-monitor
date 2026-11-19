import SwiftUI

// Home: what needs you first (answerable in one tap), then what is running, then recent threads.
struct WorkspaceView: View {
    @Environment(AppStore.self) private var store
    @State private var query = ""

    var body: some View {
        @Bindable var store = store
        NavigationStack(path: $store.workspacePath) {
            List {
                if let err = store.threadsError {
                    Section { InlineError(message: err).listRowSeparator(.hidden) }
                        .listRowBackground(Tok.bg)
                }
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
                        ForEach(settled.prefix(100)) { t in row(t, .settled) }
                    }
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
            .overlay {
                if store.threads.isEmpty && store.threadsError == nil && store.approvals.isEmpty {
                    ContentUnavailableView {
                        Label("No threads yet", systemImage: "bubble.left.and.text.bubble.right")
                    } description: {
                        Text("Agent sessions on the Mac appear here. Start one with the compose button.")
                    } actions: {
                        Button("New Thread") { store.newThread() }.buttonStyle(.borderedProminent)
                    }
                } else if !query.isEmpty && noMatches {
                    ContentUnavailableView.search(text: query)
                }
            }
            .searchable(text: $query, prompt: "Search threads")
            .navigationTitle("Threads")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { ConnectionChip(onlyWhenDown: true) }
                ToolbarItem(placement: .topBarTrailing) { ComposeButton() }
            }
            .navigationDestination(for: String.self) { id in
                if let tid = LinkedAgentsPage.threadID(fromRoute: id) {
                    LinkedAgentsPage(threadID: tid)
                } else {
                    ThreadChatView(threadID: id)
                }
            }
            .refreshable {
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
        if !cards.isEmpty || !waiting.isEmpty || other > 0 {
            FoldSection(ThreadGroup.need.label, key: "home.need", tint: Tok.need) {
                Text("\(store.approvalsCount)") // the needs-you count, as on the Approvals tab
            } content: {
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
        return rows.filter { $0.name.lowercased().contains(q) || $0.repoName.lowercased().contains(q) }
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
        return t.repoName
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
