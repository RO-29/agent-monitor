import SwiftUI

/// Everything linked to a thread: the parent, threads started from it, subagents and other agents.
struct LinkedAgents {
    struct Row: Identifiable, Hashable {
        enum Target: Hashable { case thread(String), transcript(SessionChild), none }
        var id: String
        var title: String
        var detail: String
        var agent: String
        var status: String
        var target: Target
    }
    var parent: Row?
    var threads: [Row] = []
    var subagents: [Row] = []
    var count: Int { (parent == nil ? 0 : 1) + threads.count + subagents.count }

    @MainActor
    static func build(store: AppStore, thread: V3Thread?, sessionID: String?, kids: [SessionChild], drivenChildren: [String], drivenParent: String?) -> LinkedAgents {
        var out = LinkedAgents()
        func threadRow(_ t: V3Thread) -> Row {
            Row(id: "t:" + t.id, title: t.name, detail: t.repoName, agent: t.agent, status: status(store, t), target: .thread(t.id))
        }
        let parentID = thread?.parentThread.flatMap { $0.isEmpty ? nil : $0 } ?? drivenParent
            ?? sessionID.flatMap { store.spawnParent[$0] }.map { store.resolveThreadID($0) }
        if let p = parentID {
            if let t = store.thread(store.resolveThreadID(p)) { out.parent = threadRow(t) } else {
                out.parent = Row(id: "t:" + p, title: "Parent thread", detail: p, agent: "", status: "", target: .thread(p))
            }
        }
        var seen = Set<String>()
        if let t = thread { seen.insert(t.id) }
        func addThread(_ id: String) {
            let rid = store.resolveThreadID(id)
            guard !seen.contains(rid) else { return }
            seen.insert(rid)
            if let t = store.thread(rid) { out.threads.append(threadRow(t)) }
        }
        if let t = thread { for c in store.childThreads(of: t.id) { addThread(c.id) } }
        for c in drivenChildren { addThread(c) }
        if let sid = sessionID {
            for s in store.spawnChildren[sid] ?? [] { addThread(s.id) }
        }
        for k in kids {
            if let sid = k.sessionId, !sid.isEmpty, k.kind != "subagent" {
                let rid = store.resolveThreadID(sid)
                guard !seen.contains(rid) else { continue }
                seen.insert(rid)
                out.threads.append(Row(id: "k:" + k.key, title: k.title, detail: label(k), agent: k.agent, status: k.status, target: .thread(rid)))
            } else {
                let can = (k.agentId ?? "").isEmpty == false && k.hasTranscript != false
                out.subagents.append(Row(id: "k:" + k.key, title: k.title, detail: label(k), agent: k.agent, status: k.status,
                                         target: can ? .transcript(k) : .none))
            }
        }
        return out
    }

    private static func label(_ k: SessionChild) -> String {
        [k.subagentType, k.background == true ? "background" : nil, k.model].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
    }

    @MainActor private static func status(_ store: AppStore, _ t: V3Thread) -> String {
        switch store.group(of: t) {
        case .need: return "waiting"
        case .working: return "running"
        default: return "completed"
        }
    }
}

/// Linked agents in a sheet over the chat; "Open as Page" moves it onto the thread's navigation stack.
struct LinkedAgentsSheet: View {
    @Environment(AppStore.self) private var store
    @Environment(\.dismiss) private var dismiss
    let thread: V3Thread?
    let sessionID: String?
    let drivenChildren: [String]
    let drivenParent: String?
    @State var kids: [SessionChild]
    let onOpen: (String) -> Void
    var onOpenPage: (() -> Void)? = nil

    @State private var loaded = false

    var body: some View {
        NavigationStack {
            LinkedAgentsList(linked: .build(store: store, thread: thread, sessionID: sessionID, kids: kids,
                                            drivenChildren: drivenChildren, drivenParent: drivenParent),
                             loaded: loaded, sessionID: sessionID) { id in
                dismiss()
                onOpen(id)
            }
            .navigationTitle("Linked Agents")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if let onOpenPage {
                    ToolbarItem(placement: .topBarLeading) {
                        Button { dismiss(); onOpenPage() } label: {
                            Label("Open as Page", systemImage: "arrow.up.left.and.arrow.down.right")
                        }
                    }
                }
                ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
            }
            .refreshable { await load() }
            .task { await load() }
        }
        .toastHost()
    }

    private func load() async {
        defer { loaded = true }
        if let k = await LinkedAgentsList.loadKids(sessionID) { kids = k }
    }
}

/// Linked agents as a pushed page on the Threads stack, reached by a `LinkedAgentsPage.route(_:)` path value.
struct LinkedAgentsPage: View {
    @Environment(AppStore.self) private var store
    let threadID: String

    @State private var kids: [SessionChild] = []
    @State private var driven: ODriven?
    @State private var loaded = false

    static let prefix = "linked-agents|"
    static func route(_ threadID: String) -> String { prefix + threadID }
    static func threadID(fromRoute r: String) -> String? { r.hasPrefix(prefix) ? String(r.dropFirst(prefix.count)) : nil }

    private var thread: V3Thread? { store.thread(threadID) }
    private var drivenID: String? {
        if let t = thread, t.isDriven { return (t.drivenId?.isEmpty == false ? t.drivenId : nil) ?? t.id }
        return threadID.hasPrefix("ot_") ? threadID : nil
    }
    private var sessionID: String? {
        guard drivenID == nil else { return nil }
        return thread?.latestSession ?? (threadID.contains(":") ? threadID : nil)
    }

    var body: some View {
        LinkedAgentsList(linked: .build(store: store, thread: thread, sessionID: sessionID, kids: kids,
                                        drivenChildren: driven?.children ?? [], drivenParent: driven?.parentId),
                         loaded: loaded, sessionID: sessionID) { id in
            store.workspacePath.append(store.resolveThreadID(id))
        }
        .navigationTitle("Linked Agents")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.hidden, for: .tabBar)
        .refreshable { await load() }
        .task { await load() }
    }

    private func load() async {
        defer { loaded = true }
        if let d = drivenID {
            if let r = try? await APIClient.shared.drivenDetail(d) { driven = r.thread }
        } else if let k = await LinkedAgentsList.loadKids(sessionID) {
            kids = k
        }
    }
}

/// The shared list: a filter, active rows first (running, then waiting), finished rows folded under "N completed".
struct LinkedAgentsList: View {
    let linked: LinkedAgents
    let loaded: Bool
    let sessionID: String?
    let onThread: (String) -> Void

    @State private var query = ""

    private static let done: Set<String> = ["completed", "killed", "stopped"]

    private func matches(_ r: LinkedAgents.Row) -> Bool {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        guard !q.isEmpty else { return true }
        return [r.title, r.detail, agentName(r.agent), statusLabel(r.status)].contains { $0.lowercased().contains(q) }
    }

    private func rank(_ s: String) -> Int {
        switch s {
        case "running", "starting": return 0
        case "waiting": return 1
        case "failed": return 2
        default: return 3
        }
    }

    var body: some View {
        let parent = linked.parent.flatMap { matches($0) ? $0 : nil }
        let all = (linked.threads.map { ($0, true) } + linked.subagents.map { ($0, false) }).filter { matches($0.0) }
        let failed = all.filter { $0.0.status == "failed" }
        let active = all.filter { !Self.done.contains($0.0.status) && $0.0.status != "failed" }
            .enumerated().sorted { (rank($0.element.0.status), $0.offset) < (rank($1.element.0.status), $1.offset) }.map(\.element)
        let finished = all.filter { Self.done.contains($0.0.status) }
        let searching = !query.trimmingCharacters(in: .whitespaces).isEmpty
        List {
            if let p = parent {
                Section("Started from") { row(p, isThread: true) }.listRowBackground(Tok.panel)
            }
            if !failed.isEmpty {
                Section {
                    ForEach(failed, id: \.0.id) { r in row(r.0, isThread: r.1) }
                } header: {
                    Text("Needs attention · \(failed.count)").foregroundStyle(Tok.err)
                }
                .listRowBackground(Tok.panel)
            }
            if !active.isEmpty {
                let running = active.filter { rank($0.0.status) == 0 }.count
                Section {
                    ForEach(active, id: \.0.id) { r in row(r.0, isThread: r.1) }
                } header: {
                    Text(running == active.count ? "Running · \(running)" : "Active · \(active.count)")
                }
                .listRowBackground(Tok.panel)
            }
            if !finished.isEmpty {
                FoldSection("\(finished.count) completed", key: "linked.completed", foldedByDefault: !active.isEmpty || !failed.isEmpty || parent != nil,
                            forceOpen: searching) {
                    ForEach(finished, id: \.0.id) { r in row(r.0, isThread: r.1) }
                }
                .listRowBackground(Tok.panel)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .always), prompt: "Filter linked agents")
        .overlay {
            if linked.count == 0 {
                if loaded {
                    ContentUnavailableView {
                        Label("No linked agents", systemImage: "point.3.connected.trianglepath.dotted")
                    } description: {
                        Text("Subagents, threads started from this one and its parent appear here.")
                    }
                } else {
                    ProgressView()
                }
            } else if searching && parent == nil && all.isEmpty {
                ContentUnavailableView.search(text: query)
            }
        }
    }

    @ViewBuilder private func row(_ r: LinkedAgents.Row, isThread: Bool) -> some View {
        let kind = isThread ? "Thread" : nil
        let content = HStack(alignment: .center, spacing: 12) {
            ZStack {
                Circle().fill(Tok.panel3)
                if r.agent.isEmpty { Image(systemName: "bubble.left").font(.footnote).foregroundStyle(Tok.fg2) } else { AgentGlyph(agent: r.agent, size: 16) }
            }
            .frame(width: 34, height: 34)
            .overlay(alignment: .bottomTrailing) { StatusDot(color: color(r.status), size: 9).overlay(Circle().stroke(Tok.panel, lineWidth: 2)) }
            VStack(alignment: .leading, spacing: 2) {
                Text(r.title).font(.body).foregroundStyle(Tok.fg).lineLimit(2)
                Text([statusLabel(r.status), kind ?? "", r.detail].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.footnote).foregroundStyle(Tok.muted).lineLimit(1)
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 4)
        .accessibilityElement(children: .combine)
        switch r.target {
        case .thread(let id):
            Button { onThread(id) } label: {
                HStack { content; Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(Tok.muted.opacity(0.6)) }
            }
            .buttonStyle(.plain)
        case .transcript(let k):
            if let sid = sessionID {
                NavigationLink { SubagentTranscriptView(sessionID: sid, child: k) } label: { content }
            } else {
                content
            }
        case .none:
            content
        }
    }

    private func statusLabel(_ s: String) -> String {
        switch s {
        case "running": return "Running"
        case "waiting": return "Needs you"
        case "failed": return "Failed"
        case "killed", "stopped": return "Stopped"
        case "completed": return "Done"
        default: return s.capitalized
        }
    }

    private func color(_ status: String) -> Color {
        switch status {
        case "running": return Tok.run
        case "waiting": return Tok.need
        case "failed", "killed": return Tok.err
        case "completed": return Tok.ok
        default: return Tok.idle
        }
    }

    static func loadKids(_ sessionID: String?) async -> [SessionChild]? {
        #if DEBUG
        if DebugRoute.sample { return DebugRoute.sampleKids() }
        #endif
        guard let sid = sessionID, let r = try? await APIClient.shared.children(sid) else { return nil }
        return r.children
    }
}

/// A Claude subagent's own transcript (GET /api/session/{id}/children/{agentId}/items?after=).
struct SubagentTranscriptView: View {
    let sessionID: String
    let child: SessionChild
    @State private var items: [String: WItem] = [:]
    @State private var cursor: Int64?
    @State private var error: String?
    @State private var loaded = false
    @State private var expanded: Set<String> = []

    private var sorted: [WItem] {
        items.values.sorted { $0.startedAt == $1.startedAt ? $0.id < $1.id : $0.startedAt < $1.startedAt }
    }

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 14) {
                if let p = child.prompt, !p.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Task").font(.footnote.weight(.semibold)).foregroundStyle(Tok.muted)
                        Text(p).font(.subheadline).foregroundStyle(Tok.fg2).lineLimit(8).textSelection(.enabled)
                    }
                    .padding(12)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                }
                if !loaded {
                    ProgressView().frame(maxWidth: .infinity).padding(.top, 40)
                }
                ForEach(sorted) { it in
                    ChatItemView(item: it, expanded: expanded.contains(it.id)) {
                        if expanded.contains(it.id) { expanded.remove(it.id) } else { expanded.insert(it.id) }
                    }
                }
                if let error { InlineError(message: error) }
            }
            .padding(16)
        }
        .background(Tok.panel)
        .navigationTitle(child.subagentType ?? "Subagent")
        .navigationBarTitleDisplayMode(.inline)
        .task { await loop() }
    }

    private func loop() async {
        guard let aid = child.agentId else { return }
        while !Task.isCancelled {
            do {
                let r = try await APIClient.shared.subagentItems(sessionID, agentID: aid, after: cursor)
                if r.reset ?? false { items = [:] }
                for it in r.items ?? [] { items[it.id] = it }
                cursor = r.cursor
                error = nil
            } catch {
                if !Task.isCancelled { self.error = error.localizedDescription }
            }
            loaded = true
            if child.status != "running" && error == nil { return }
            try? await Task.sleep(nanoseconds: 2_000_000_000)
        }
    }
}
