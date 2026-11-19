import SwiftUI

/// Where a thread's messages go: a watched CLI session or a driven orchestrator thread.
enum ChatTarget: Hashable {
    case watched(String)
    case driven(String)
}

// A thread's chat. Watched sessions poll GET /api/session/{id}/items by cursor; driven threads follow
// /api/orch/stream after one GET /api/orch/threads/{id}. Pending approvals, a terminal question and the message box follow.
struct ThreadChatView: View {
    @Environment(AppStore.self) private var store
    let threadID: String
    private var fullscreen: Bool { store.chatFullscreen }

    @State private var items: [String: WItem] = [:]
    /// Turns of the CLI session a driven thread continued, before the thread existed.
    @State private var prior: [String: WItem] = [:]
    @State private var priorSID: String?
    @State private var cursor: Int64 = 0
    @State private var earlier: Int64?
    @State private var loaded = false
    @State private var feedError: String?
    @State private var earlierBusy = false
    @State private var expanded: Set<String> = []
    @State private var showDetails = false
    @State private var showLinked = false
    @State private var kids: [SessionChild] = []
    @State private var driven: ODriven?
    @State private var atBottom = true
    /// When the bottom marker last left the screen; a composer that grows pushes it out without a scroll.
    @State private var bottomLeftAt = Date.distantPast
    @State private var atTop = true
    @State private var selecting = false
    @State private var selected: Set<String> = []
    @State private var includeReplies = true
    @State private var showHistory = false
    @State private var confirmEnd = false
    private var sessionLive: Bool { [.running, .awaitingInput, .awaitingPermission, .idle].contains(session?.state ?? .unknown) }

    private var thread: V3Thread? { store.thread(threadID) }
    private var drivenID: String? {
        if let t = thread, t.isDriven { return (t.drivenId?.isEmpty == false ? t.drivenId : nil) ?? t.id }
        return threadID.hasPrefix("ot_") ? threadID : nil
    }
    /// Messages go to the newest session; an id with no thread row is taken as a session id.
    private var sessionID: String? { thread?.latestSession ?? (threadID.contains(":") ? threadID : nil) }
    private var target: ChatTarget? {
        if let d = drivenID { return .driven(d) }
        return sessionID.map { .watched($0) }
    }
    private var session: Session? { sessionID.flatMap { store.sessionsByID[$0] } }
    private var title: String { thread?.name ?? driven?.title ?? session?.headline ?? "Thread" }
    private var agent: String { thread?.agent ?? driven?.agent ?? session?.tool ?? "claude" }
    private var asks: [Approval] {
        if let t = thread { return store.approvals(forThread: t) }
        if let d = drivenID { return store.approvals.filter { $0.drivenId == d } }
        return sessionID.map { store.approvals(forSession: $0) } ?? []
    }
    private var running: Bool {
        if drivenID != nil { return driven?.status == "running" || driven?.status == "starting" }
        return session?.state == .running
    }
    private var sorted: [WItem] {
        (prior.isEmpty ? Array(items.values) : Array(items.values) + prior.values.filter { items[$0.id] == nil })
            .sorted { $0.startedAt == $1.startedAt ? $0.id < $1.id : $0.startedAt < $1.startedAt }
    }
    private var linked: LinkedAgents {
        .build(store: store, thread: thread, sessionID: drivenID == nil ? sessionID : nil, kids: kids,
               drivenChildren: driven?.children ?? [], drivenParent: driven?.parentId)
    }

    var body: some View {
        Group {
            if let target {
                chat(target)
            } else {
                ContentUnavailableView {
                    Label("No session to show", systemImage: "rectangle.on.rectangle.slash")
                } description: {
                    Text("This thread has no session the daemon can read.")
                }
            }
        }
        .background(Tok.panel)
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .navigationBarBackButtonHidden(selecting)
        .toolbar {
            if selecting {
                ToolbarItem(placement: .topBarLeading) {
                    Button("Cancel") { withAnimation(.snappy) { selecting = false; selected = [] } }
                }
                ToolbarItem(placement: .principal) {
                    Text(selected.isEmpty ? "Select Messages" : "\(selected.count) Selected").font(.headline)
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button(selected.count == userIDs.count ? "None" : "All") {
                        selected = selected.count == userIDs.count ? [] : Set(userIDs)
                    }
                }
            } else {
                ToolbarItem(placement: .principal) { titleView }
                ToolbarItem(placement: .topBarTrailing) {
                    Button { setFullscreen(true) } label: { Image(systemName: "arrow.up.left.and.arrow.down.right") }
                        .accessibilityLabel("Read full screen")
                }
                ToolbarItem(placement: .topBarTrailing) { threadMenu }
            }
        }
        .toolbarBackground(Tok.panel, for: .navigationBar)
        .toolbar(.hidden, for: .tabBar)
        .toolbar(fullscreen ? .hidden : .visible, for: .navigationBar)
        .statusBarHidden(fullscreen)
        .overlay(alignment: .top) { if fullscreen { exitStrip } }
        .sheet(isPresented: $showHistory) {
            if let sid = sessionID {
                CommandHistorySheet(sessionID: sid, run: canRunBash ? { cmd in await runBash(sid, cmd) } : nil)
                    .presentationDetents([.medium, .large])
                    .presentationDragIndicator(.visible)
                    .presentationBackground(Tok.bg)
            }
        }
        .sheet(isPresented: $showDetails) {
            ThreadDetailsSheet(thread: thread, session: session, sessionID: sessionID, edited: editedFiles,
                               linkedCount: linked.count) { openLinkedPage() }
                .presentationDetents([.medium, .large])
                .presentationDragIndicator(.visible)
                .presentationBackground(Tok.bg)
        }
        .sheet(isPresented: $showLinked) {
            LinkedAgentsSheet(thread: thread, sessionID: drivenID == nil ? sessionID : nil,
                              drivenChildren: driven?.children ?? [], drivenParent: driven?.parentId, kids: kids) { id in
                store.workspacePath.append(store.resolveThreadID(id))
            } onOpenPage: { openLinkedPage() }
            .presentationDetents([.medium, .large])
            .presentationDragIndicator(.visible)
            .presentationBackground(Tok.bg)
        }
        .task(id: sessionID) { await kidsLoop() }
        #if DEBUG
        .onAppear {
            let s = DebugRoute.screen ?? ""
            if DebugRoute.wantsDetails { showDetails = true }
            if s.hasPrefix("linked:") { showLinked = true }
            if s.hasPrefix("history:") { showHistory = true }
            if s.hasPrefix("fullscreen:") { Task { try? await Task.sleep(nanoseconds: 1_500_000_000); setFullscreen(true) } }
            if s.hasPrefix("select:") {
                selecting = true
                Task { try? await Task.sleep(nanoseconds: 800_000_000); selected = Set(userIDs.prefix(1)) }
            }
        }
        #endif
    }

    private var userIDs: [String] { sorted.filter { $0.kind == "user" }.map(\.id) }
    private var canRunBash: Bool { drivenID == nil && ["claude", "codex"].contains(agentKey(agent)) }

    private func runBash(_ sid: String, _ cmd: String) async {
        do {
            _ = try await APIClient.shared.cliSend(sid, text: "!" + cmd, attachments: [])
            ToastCenter.shared.show("Sent to the terminal")
        } catch {
            ToastCenter.shared.show(error.localizedDescription, icon: "exclamationmark.triangle.fill")
        }
    }

    private func setFullscreen(_ on: Bool) {
        withAnimation(.snappy) { store.chatFullscreen = on }
    }

    /// In full screen: a tap on the top edge or the pill brings the bars back.
    private var exitStrip: some View {
        ZStack(alignment: .top) {
            Color.clear.frame(height: 28).contentShape(Rectangle())
                .onTapGesture { setFullscreen(false) }
                .ignoresSafeArea(edges: .top)
            Button { setFullscreen(false) } label: {
                Label("Show controls", systemImage: "chevron.down")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Tok.fg2)
                    .padding(.horizontal, 12).frame(height: 32)
                    .background(.regularMaterial, in: Capsule())
                    .overlay(Capsule().stroke(Tok.line, lineWidth: 0.5))
            }
            .buttonStyle(.plain)
            .padding(.top, 4)
            .accessibilityLabel("Exit full screen")
        }
        .frame(maxWidth: .infinity)
        .transition(.move(edge: .top).combined(with: .opacity))
    }

    /// The selected prompts, with the replies up to the next prompt when asked, as one Markdown document.
    private var exportMarkdown: String {
        var out = ["# \(title)", ""]
        var take = false
        for it in sorted {
            if it.kind == "user" {
                take = selected.contains(it.id)
                if take { out += ["## You", "", FileNote.split(it.text).text, ""] }
                continue
            }
            guard take, includeReplies else { continue }
            if it.kind == "agent", !it.text.isEmpty { out += ["## \(agentName(agent))", "", it.text, ""] }
            if it.kind == "command", let c = (it.command?.isEmpty == false ? it.command : it.text), !c.isEmpty {
                out += ["```", "$ " + c, "```", ""]
            }
        }
        return out.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines) + "\n"
    }

    private var selectBar: some View {
        VStack(spacing: 8) {
            Toggle("Include replies", isOn: $includeReplies).font(.subheadline).tint(Tok.accent)
            HStack(spacing: 8) {
                Button { copyToPasteboard(SlackFormat.toSlack(exportMarkdown), label: "Copied for Slack") } label: {
                    Label("Copy for Slack", systemImage: "number.square").frame(maxWidth: .infinity)
                }
                .buttonStyle(.am(.normal, big: true, full: true))
                Button { Share.markdownFile(exportMarkdown, name: title) } label: {
                    Label("Share .md", systemImage: "square.and.arrow.up").frame(maxWidth: .infinity)
                }
                .buttonStyle(.am(.pri, big: true, full: true))
            }
            .disabled(selected.isEmpty)
        }
        .padding(.horizontal, 16).padding(.top, 10).padding(.bottom, 8)
        .background {
            Rectangle().fill(.bar).overlay(alignment: .top) { Rectangle().fill(Tok.line).frame(height: 0.5) }
                .ignoresSafeArea(edges: .bottom)
        }
    }

    private func openLinkedPage() {
        store.workspacePath.append(LinkedAgentsPage.route(threadID))
    }

    /// Files the session edited, with summed line counts (filesOf in Side.tsx).
    private var editedFiles: [WFile] {
        var order: [String] = []
        var m: [String: WFile] = [:]
        for it in sorted where it.kind == "fileChange" && it.status != "declined" && it.status != "failed" {
            for f in it.files ?? [] {
                if var was = m[f.path] {
                    was.add += f.add
                    was.del += f.del
                    if was.kind != "add" { was.kind = f.kind }
                    m[f.path] = was
                } else {
                    order.append(f.path)
                    m[f.path] = f
                }
            }
        }
        return order.compactMap { m[$0] }
    }

    /// One trailing control for linked agents and details, so the title keeps its width; the badge counts linked agents.
    private var threadMenu: some View {
        let n = linked.count
        return Menu {
            Button { showLinked = true } label: {
                Label(n == 0 ? "Linked Agents" : "Linked Agents (\(n))", systemImage: "point.3.connected.trianglepath.dotted")
            }
            Button { openLinkedPage() } label: {
                Label("Open Linked Agents as Page", systemImage: "arrow.up.left.and.arrow.down.right")
            }
            Divider()
            Button { withAnimation(.snappy) { selecting = true; selected = [] } } label: {
                Label("Select Messages", systemImage: "checkmark.circle")
            }
            Button { setFullscreen(true) } label: { Label("Read Full Screen", systemImage: "arrow.up.left.and.arrow.down.right") }
            if sessionID != nil, canRunBash {
                Button { showHistory = true } label: { Label("Command History", systemImage: "clock.arrow.circlepath") }
            }
            if let sid = sessionID, canRunBash {
                if sessionLive {
                    Button(role: .destructive) { confirmEnd = true } label: { Label("End Session", systemImage: "stop.circle") }
                } else {
                    Button { Task { await resume(sid) } } label: { Label("Resume in Terminal", systemImage: "play.circle") }
                }
            }
            Divider()
            Button { showDetails = true } label: { Label("Thread Details", systemImage: "info.circle") }
        } label: {
            Image(systemName: "ellipsis.circle")
        }
        .confirmationDialog("End this session?", isPresented: $confirmEnd, titleVisibility: .visible) {
            Button("End Session", role: .destructive) { if let sid = sessionID { Task { await end(sid) } } }
        } message: {
            Text("This stops the current turn and exits the agent. You can resume it later.")
        }
        .accessibilityLabel("Thread actions")
        .accessibilityValue(n == 0 ? "No linked agents" : "\(n) linked agents")
    }

    private func end(_ sid: String) async {
        do {
            let ended = try await APIClient.shared.endSession(sid)
            ToastCenter.shared.show(ended ? "Session ended" : "Exit sent; check the terminal", icon: ended ? "stop.circle" : "exclamationmark.circle")
        } catch { ToastCenter.shared.show("Could not end: \(error.localizedDescription)", icon: "xmark.octagon") }
        await store.loadThreads()
    }

    private func resume(_ sid: String) async {
        do {
            try await APIClient.shared.resumeSession(sid)
            ToastCenter.shared.show("Resumed in a terminal", icon: "play.circle")
        } catch { ToastCenter.shared.show("Could not resume: \(error.localizedDescription)", icon: "xmark.octagon") }
        await store.loadThreads()
    }

    /// Two-line title: thread name, then a status dot with the agent and repo.
    private var titleView: some View {
        let status: (String, Color)? = !asks.isEmpty ? ("Needs you", Tok.need) : running ? ("Working", Tok.run) : nil
        return VStack(spacing: 1) {
            Text(title).font(.headline).foregroundStyle(Tok.fg).lineLimit(1).minimumScaleFactor(0.85)
            HStack(spacing: 5) {
                if let status {
                    StatusDot(color: status.1, size: 7)
                    Text(status.0).foregroundStyle(status.1)
                    Text("·")
                }
                AgentGlyph(agent: agent, size: 11)
                Text(thread?.repoName ?? session?.project ?? ((driven?.cwd ?? "") as NSString).lastPathComponent).lineLimit(1)
            }
            .font(.caption.weight(.medium))
            .foregroundStyle(Tok.muted)
        }
        .frame(maxWidth: 270)
        .accessibilityElement(children: .combine)
    }

    private func chat(_ target: ChatTarget) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 14) {
                    Color.clear.frame(height: 1).id("top")
                        .onAppear { atTop = true }
                        .onDisappear { atTop = false }
                    if earlier != nil, let sid = earlierSID(target) {
                        Button {
                            Task { await loadEarlier(sid) }
                        } label: {
                            HStack(spacing: 6) {
                                if earlierBusy { ProgressView().controlSize(.small) } else { Image(systemName: "arrow.up.circle") }
                                Text("Load earlier turns")
                            }
                            .font(.subheadline.weight(.medium))
                            .frame(minHeight: 44)
                        }
                        .buttonStyle(.borderless)
                        .tint(Tok.fg2)
                        .frame(maxWidth: .infinity)
                        .disabled(earlierBusy)
                    }
                    if !loaded {
                        HStack(spacing: 8) {
                            ProgressView()
                            Text(feedError ?? "Loading the transcript").font(.subheadline).foregroundStyle(Tok.muted)
                        }
                        .frame(maxWidth: .infinity).padding(.top, 60)
                    }
                    ForEach(sorted) { it in
                        ChatItemView(item: it, expanded: expanded.contains(it.id), agent: agent,
                                     selecting: selecting, selected: selected.contains(it.id)) {
                            withAnimation(.snappy(duration: 0.2)) {
                                if expanded.contains(it.id) { expanded.remove(it.id) } else { expanded.insert(it.id) }
                            }
                        } onSelect: {
                            if selected.contains(it.id) { selected.remove(it.id) } else { selected.insert(it.id) }
                        }
                        .id(it.id)
                        .padding(.top, it.kind == "user" && it.id != sorted.first?.id ? 12 : 0)
                    }
                    if running {
                        HStack(spacing: 10) { WorkingPulse(); Text("Working").font(.subheadline) }
                            .foregroundStyle(Tok.muted)
                            .padding(.leading, 4)
                            .accessibilityElement(children: .combine)
                    }
                    if let e = driven?.error, !e.isEmpty, driven?.status == "error" {
                        InlineError(message: e)
                    }
                    ForEach(asks) { a in HookApprovalCard(a: a, showHeader: false) }
                    // The card polls /question itself and shows only while a question is open in the terminal.
                    if case .watched(let sid) = target, asks.isEmpty, agent == "claude" {
                        TermQuestionCard(sessionID: sid)
                    }
                    if loaded, let feedError {
                        InlineError(message: feedError)
                    }
                    Color.clear.frame(height: 1).id("bottom")
                        .onAppear { atBottom = true }
                        .onDisappear { atBottom = false; bottomLeftAt = Date() }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 16)
            }
            .defaultScrollAnchor(.bottom)
            .scrollDismissesKeyboard(.interactively)
            .modifier(FullscreenGestures(fullscreen: Binding(get: { store.chatFullscreen }, set: { store.chatFullscreen = $0 })))
            .onChange(of: sorted.last?.id) { _, _ in
                // Follow new output only while the reader is at the bottom.
                if atBottom { withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
            }
            .onChange(of: asks.count) { _, _ in withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
            .environment(\.canFillComposer, true)
            .overlay(alignment: .topTrailing) {
                if !atTop && loaded {
                    Button {
                        withAnimation { proxy.scrollTo("top", anchor: .top) }
                    } label: {
                        Image(systemName: "arrow.up")
                            .font(.body.weight(.semibold))
                            .foregroundStyle(Tok.fg)
                            .frame(width: 40, height: 40)
                            .background(.regularMaterial, in: Circle())
                            .overlay(Circle().stroke(Tok.line, lineWidth: 0.5))
                            .shadow(color: .black.opacity(0.12), radius: 6, y: 2)
                    }
                    .padding(.trailing, 16).padding(.top, 10)
                    .transition(.scale.combined(with: .opacity))
                    .accessibilityLabel("Go to top")
                }
            }
            .animation(.snappy, value: atTop)
            .overlay(alignment: .bottomTrailing) {
                if !atBottom && loaded {
                    Button {
                        withAnimation { proxy.scrollTo("bottom", anchor: .bottom) }
                    } label: {
                        Image(systemName: "arrow.down")
                            .font(.body.weight(.semibold))
                            .foregroundStyle(Tok.fg)
                            .frame(width: 44, height: 44)
                            .background(.regularMaterial, in: Circle())
                            .overlay(Circle().stroke(Tok.line, lineWidth: 0.5))
                            .shadow(color: .black.opacity(0.12), radius: 6, y: 2)
                    }
                    .padding(.trailing, 16).padding(.bottom, 12)
                    .transition(.scale.combined(with: .opacity))
                    .accessibilityLabel("Scroll to the latest message")
                }
            }
            .animation(.snappy, value: atBottom)
            .safeAreaInset(edge: .bottom, spacing: 0) {
                if selecting {
                    selectBar
                } else if !fullscreen {
                    Composer(target: target, agent: agent,
                             busyCLI: running || session?.state == .awaitingPermission,
                             model: session?.model ?? thread?.model, cwd: session?.cwd ?? thread?.cwd ?? driven?.cwd ?? "",
                             nativeID: session?.sessionId ?? String((sessionID ?? "").split(separator: ":", maxSplits: 1).last ?? ""),
                             closed: driven?.status == "closed",
                             onHistory: canRunBash && sessionID != nil ? { showHistory = true } : nil,
                             draftID: threadID)
                    // The queue panel or an error makes the composer taller: keep a reader who was at the bottom there.
                    .background(GeometryReader { g in
                        Color.clear.onChange(of: g.size.height) { old, new in
                            guard new != old, atBottom || Date().timeIntervalSince(bottomLeftAt) < 0.5 else { return }
                            withAnimation { proxy.scrollTo("bottom", anchor: .bottom) }
                        }
                    })
                }
            }
            .task(id: target) {
                switch target {
                case .watched(let sid): await feedLoop(sid)
                case .driven(let id): await drivenLoop(id)
                }
            }
        }
    }

    // MARK: feed

    private func apply(_ r: WatchedItems, first: Bool) {
        if first || (r.reset ?? false) {
            items = [:]
            earlier = r.earlier
        }
        for it in r.items ?? [] { items[it.id] = it }
        cursor = r.cursor
        loaded = true
    }

    /// First read retries every 2 s (a new session writes its transcript late); then reads after the cursor.
    private func feedLoop(_ sid: String) async {
        items = [:]; prior = [:]; priorSID = nil; cursor = 0; earlier = nil; loaded = false; feedError = nil
        #if DEBUG
        if DebugRoute.sample { items = DebugRoute.sampleItems(); loaded = true; return }
        #endif
        while !Task.isCancelled && !loaded {
            do {
                apply(try await APIClient.shared.watchedItems(sid), first: true)
                feedError = nil
            } catch let e as APIError where e.status == 404 || e.status == 501 {
                feedError = e.message
            } catch {
                if Task.isCancelled { return }
                feedError = error.localizedDescription
            }
            if !loaded { try? await Task.sleep(nanoseconds: 2_000_000_000) }
        }
        while !Task.isCancelled {
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            if Task.isCancelled { return }
            do {
                apply(try await APIClient.shared.watchedItems(sid, after: cursor), first: false)
                feedError = nil
            } catch {
                if !Task.isCancelled { feedError = error.localizedDescription }
            }
        }
    }

    /// Loads the driven thread once, then follows /api/orch/stream; a daemon without the stream is polled.
    private func earlierSID(_ target: ChatTarget) -> String? {
        if case .watched(let sid) = target { return sid }
        return priorSID
    }

    /// Reads the newest page of the continued CLI session, keeping only turns from before the thread was made.
    private func loadPrior(_ t: ODriven) async {
        guard priorSID == nil, let native = t.nativeId, !native.isEmpty, ["claude", "codex"].contains(t.agent) else { return }
        let sid = "\(t.agent):\(native)"
        guard let r = try? await APIClient.shared.watchedItems(sid) else { return }
        let cut = t.createdAt ?? .max
        var next: [String: WItem] = [:]
        for it in r.items ?? [] where it.startedAt < cut { next["prior:" + it.id] = it.withID("prior:" + it.id) }
        // Keep the session even when this page holds only later turns: Load earlier reaches the older ones.
        guard !next.isEmpty || r.earlier != nil else { return }
        prior = next
        priorSID = sid
        earlier = r.earlier
    }

    private func drivenLoop(_ id: String) async {
        items = [:]; prior = [:]; priorSID = nil; earlier = nil; loaded = false; feedError = nil
        var hasStream = true
        var wait: UInt64 = 1
        while !Task.isCancelled {
            var seq: Int64?
            do {
                let d = try await APIClient.shared.drivenDetail(id)
                applyDetail(d)
                await loadPrior(d.thread)
                seq = d.seq
                feedError = nil
            } catch {
                if Task.isCancelled { return }
                feedError = error.localizedDescription
            }
            if hasStream, let after = seq {
                do {
                    for try await ev in APIClient.shared.orchStream(after: after) where ev.threadId == id {
                        applyEvent(ev)
                        wait = 1
                    }
                } catch let e as APIError where e.status == 404 || e.status == 405 || e.status == 501 {
                    hasStream = false
                } catch {
                    if Task.isCancelled { return }
                }
                if hasStream {
                    try? await Task.sleep(nanoseconds: wait * 1_000_000_000)
                    wait = min(wait * 2, 15)
                    continue
                }
            }
            let busy = driven?.status == "running" || driven?.status == "starting"
            wait = busy ? 2 : min(max(wait, 5) * 2, 30)
            try? await Task.sleep(nanoseconds: wait * 1_000_000_000)
        }
    }

    // Assigns only what changed, so an idle poll does not re-render the list.
    private func applyDetail(_ d: ODrivenDetail) {
        if driven != d.thread { driven = d.thread }
        var next: [String: WItem] = [:]
        for it in d.items { next[it.id] = it }
        if next != items { items = next }
        loaded = true
    }

    private func applyEvent(_ ev: OEvent) {
        guard let d = ev.data else { return }
        switch ev.type {
        case "thread.updated":
            if let t = d.thread, t != driven { driven = t }
        case "item.started", "item.completed":
            guard var it = d.item else { return }
            // A re-sent started item has no text; keep what the deltas built.
            if ev.type == "item.started", it.text.isEmpty, let was = items[it.id] { it.text = was.text }
            if items[it.id] != it { items[it.id] = it }
        case "item.delta":
            guard let iid = d.itemId, let text = d.text, var x = items[iid] else { return }
            if d.field == "output" { x.output = String(((x.output ?? "") + text).suffix(8192)) } else { x.text += text }
            items[iid] = x
        default:
            break
        }
    }

    private func loadEarlier(_ sid: String) async {
        guard let before = earlier, !earlierBusy else { return }
        earlierBusy = true
        defer { earlierBusy = false }
        do {
            let r = try await APIClient.shared.watchedItems(sid, before: before)
            if sid == priorSID {
                let cut = driven?.createdAt ?? .max
                for it in r.items ?? [] where it.startedAt < cut && prior["prior:" + it.id] == nil { prior["prior:" + it.id] = it.withID("prior:" + it.id) }
            } else {
                for it in r.items ?? [] where items[it.id] == nil { items[it.id] = it }
            }
            earlier = r.earlier
        } catch {
            feedError = error.localizedDescription
        }
    }

    /// Subagent count for the toolbar badge; refreshed every 20 s.
    private func kidsLoop() async {
        guard drivenID == nil, let sid = sessionID else { return }
        #if DEBUG
        if DebugRoute.sample { kids = DebugRoute.sampleKids(); return }
        #endif
        while !Task.isCancelled {
            if let r = try? await APIClient.shared.children(sid) { kids = r.children }
            try? await Task.sleep(nanoseconds: 20_000_000_000)
        }
    }
}

// MARK: - One chat item

/// Pulsing run dot (`.d-run`).
struct WorkingPulse: View {
    @State private var on = false
    var body: some View {
        StatusDot(color: Tok.run, size: 7)
            .background(Circle().fill(Tok.runSoft).scaleEffect(on ? 2.2 : 1.2))
            .onAppear { withAnimation(.easeInOut(duration: 0.8).repeatForever()) { on = true } }
    }
}

struct ChatItemView: View {
    let item: WItem
    let expanded: Bool
    var agent = "claude"
    var selecting = false
    var selected = false
    let toggle: () -> Void
    var onSelect: () -> Void = {}

    private var running: Bool { ["started", "running", "inProgress"].contains(item.status) }
    private var dur: Int64 { item.endedAt.map { max(0, $0 - item.startedAt) } ?? 0 }

    var body: some View {
        switch item.kind {
        case "user":
            let note = FileNote.split(item.text)
            let att = (item.attachments ?? []) + note.files
            let shown = note.text
            HStack(alignment: .center, spacing: 10) {
            if selecting {
                Image(systemName: selected ? "checkmark.circle.fill" : "circle")
                    .font(.title2)
                    .foregroundStyle(selected ? Tok.accent : Tok.line2)
                    .frame(width: 32)
                    .transition(.move(edge: .leading).combined(with: .opacity))
            }
            VStack(alignment: .trailing, spacing: 4) {
                if !att.isEmpty {
                    ForEach(att.filter { $0.mime.hasPrefix("image/") && $0.mime != "image/heic" }) { a in
                        UploadThumb(att: a)
                    }
                    ForEach(att.filter { !$0.mime.hasPrefix("image/") || $0.mime == "image/heic" }) { a in
                        Label(a.name ?? "Attachment", systemImage: a.mime.hasPrefix("image/") ? "photo" : "doc")
                            .font(.footnote.weight(.medium)).foregroundStyle(Tok.fg2).lineLimit(1)
                            .padding(.horizontal, 10).padding(.vertical, 6)
                            .background(Tok.panel2, in: Capsule())
                            .overlay(Capsule().stroke(Tok.line, lineWidth: 0.5))
                    }
                }
                if !shown.isEmpty {
                    Text(shown)
                        .font(.body)
                        .foregroundStyle(Tok.fg)
                        .lineSpacing(2)
                        .textSelection(.enabled)
                        .padding(.horizontal, 14).padding(.vertical, 10)
                        .background(Tok.panel3, in: UnevenRoundedRectangle(topLeadingRadius: 14, bottomLeadingRadius: 14, bottomTrailingRadius: 4, topTrailingRadius: 14))
                        .overlay(UnevenRoundedRectangle(topLeadingRadius: 14, bottomLeadingRadius: 14, bottomTrailingRadius: 4, topTrailingRadius: 14).stroke(Tok.line, lineWidth: 0.5))
                        .contextMenu { CopyMenuItems(markdown: shown) }
                    if !selecting {
                        HStack(spacing: 4) {
                            Text(clock(item.startedAt)).font(.caption2.monospacedDigit()).foregroundStyle(Tok.muted)
                            CopyButton(markdown: shown)
                        }
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .trailing)
            .padding(.leading, selecting ? 0 : 40)
            }
            .contentShape(Rectangle())
            .onTapGesture { if selecting { onSelect() } }
            .accessibilityAddTraits(selecting && selected ? .isSelected : [])
        case "agent":
            VStack(alignment: .leading, spacing: 2) {
                FoldedProse(text: item.text, size: 17)
                    .contextMenu { CopyMenuItems(markdown: item.text) }
                if !selecting && !item.text.isEmpty {
                    HStack(spacing: 2) {
                        CopyButton(markdown: item.text)
                        Menu {
                            CopyMenuItems(markdown: item.text)
                        } label: {
                            Image(systemName: "ellipsis").font(.footnote.weight(.medium)).foregroundStyle(Tok.muted)
                                .frame(width: 32, height: 32).contentShape(Rectangle())
                        }
                        .accessibilityLabel("More copy options")
                    }
                    .padding(.leading, -8)
                }
            }
            .padding(.vertical, 2)
            .opacity(selecting ? 0.55 : 1)
        case "error":
            Text((item.text.isEmpty ? "Error" : item.text) + ((item.`repeat` ?? 0) > 1 ? "  ×\(item.`repeat`!) · last \(relativeTime(item.endedAt ?? item.startedAt))" : ""))
                .font(.subheadline).foregroundStyle(Tok.err)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 12).padding(.vertical, 9)
                .background(Tok.errSoft, in: RoundedRectangle(cornerRadius: 10))
                .overlay(RoundedRectangle(cornerRadius: 10).stroke(Tok.err.opacity(0.4), lineWidth: 1))
        case "reasoning":
            thinking
        case "plan":
            if let todos = parseTodos(item.text) { todoBox(todos) } else { toolLine }
        default:
            toolLine
        }
    }

    // Glyph, colour, label and right-hand meta, as DrivenChat.tsx renders a `.witem` row.
    private var row: (icon: String, color: Color, label: String, detail: String, meta: String) {
        let first = item.text.trimmingCharacters(in: .whitespacesAndNewlines).components(separatedBy: "\n").first ?? ""
        let name = item.command ?? ""
        let failIcon = ("✗", Tok.err)
        switch item.kind {
        case "command":
            let (i, c) = running ? ("●", Tok.run) : item.failed ? failIcon : ("✓", Tok.muted)
            let cmd = (name.isEmpty ? item.text : name).components(separatedBy: "\n").first ?? ""
            let meta = running ? "running" : item.failed ? "exit \(item.exitCode.map(String.init) ?? "?")" : dur > 0 ? fmtDuration(dur) : ""
            return (i, c, "", cmd, meta)
        case "fileChange":
            let fs = item.files ?? []
            let add = fs.reduce(0) { $0 + $1.add }, del = fs.reduce(0) { $0 + $1.del }
            let what = fs.count == 1 ? (fs[0].path as NSString).lastPathComponent : "\(fs.count) files"
            return ("±", Tok.need, item.status == "declined" ? "Edit declined" : "Edited", what, "+\(add) -\(del)")
        case "mcp":
            let parts = name.components(separatedBy: "__")
            let label = parts.count >= 3 && parts[0] == "mcp" ? "\(parts[1]) · \(parts[2...].joined(separator: "__"))" : (name.isEmpty ? "MCP" : name)
            return item.failed ? (failIcon.0, failIcon.1, label, first, "") : ("◆", Tok.accent, label, first, "")
        case "web":
            let fetch = name.lowercased().contains("fetch") || first.hasPrefix("http")
            return (fetch ? "↗" : "⌕", item.failed ? Tok.err : Tok.run, fetch ? "Fetch" : "Web search", first, "")
        default:
            if running { return ("●", Tok.run, name.isEmpty ? "Step" : name, first, "running") }
            if item.failed { return ("✗", Tok.err, name.isEmpty ? "Step" : name, first, item.status == "declined" ? "declined" : "") }
            switch name {
            case "Task", "Agent": return ("↳", Tok.claude, "Subagent", first, "")
            case "Read": return ("○", Tok.muted, "Read", first, "")
            case "Grep", "Glob": return ("⌕", Tok.muted, "Search", first, "")
            default: return ("·", Tok.muted, name.isEmpty ? "Step" : name, first == name ? "" : first, "")
            }
        }
    }

    private var detail: String {
        var parts: [String] = []
        if item.kind == "command", let c = item.command, c.contains("\n") || c.count > 60 { parts.append("$ " + c) }
        if item.kind != "command", item.kind != "fileChange", !item.text.isEmpty { parts.append(item.text) }
        if let fs = item.files, !fs.isEmpty {
            parts.append(fs.map { "\($0.kind == "add" ? "new " : $0.kind == "delete" ? "deleted " : "")\($0.path)  +\($0.add) -\($0.del)" }.joined(separator: "\n"))
        }
        if let o = item.output?.trimmingCharacters(in: .whitespacesAndNewlines), !o.isEmpty { parts.append(o) }
        return parts.joined(separator: "\n\n")
    }

    /// A shell command this row ran: Bash or a ! command.
    private var copyableCommand: String? {
        guard item.kind == "command" else { return nil }
        let c = (item.command?.isEmpty == false ? item.command! : item.text).trimmingCharacters(in: .whitespacesAndNewlines)
        return c.isEmpty ? nil : c
    }

    private var isSubagent: Bool { item.kind != "command" && ["Task", "Agent"].contains(item.command ?? "") }

    @ViewBuilder private var copyMenu: some View {
        if let c = copyableCommand {
            Button { copyToPasteboard(cleanCommand(c), label: "Command copied") } label: { Label("Copy Command", systemImage: "terminal") }
        }
        if let o = item.output?.trimmingCharacters(in: .whitespacesAndNewlines), !o.isEmpty {
            Button { copyToPasteboard(o) } label: { Label("Copy Output", systemImage: "doc.on.doc") }
            Button { copyToPasteboard(isSubagent ? SlackFormat.toSlack(o) : "```\n\(o)\n```", label: "Copied for Slack") } label: {
                Label("Copy Output for Slack", systemImage: "number.square")
            }
            if let c = copyableCommand {
                Button { copyToPasteboard("```\n$ \(cleanCommand(c))\n\(o)\n```", label: "Copied as Markdown") } label: {
                    Label("Copy Command and Output", systemImage: "text.badge.plus")
                }
            }
            Button { Share.present([o]) } label: { Label("Share Output…", systemImage: "square.and.arrow.up") }
        }
        if item.kind != "command", !item.text.isEmpty {
            Button { copyToPasteboard(item.text) } label: { Label("Copy", systemImage: "doc.on.doc") }
        }
        if let fs = item.files, !fs.isEmpty {
            Button { copyToPasteboard(fs.map(\.path).joined(separator: "\n")) } label: { Label("Copy Paths", systemImage: "folder") }
        }
    }

    private var toolLine: some View {
        let r = row
        return VStack(alignment: .leading, spacing: 2) {
            Button(action: toggle) {
                HStack(spacing: 8) {
                    Text(r.icon).font(.footnote.weight(.bold)).foregroundStyle(r.color).frame(width: 16)
                    HStack(spacing: 5) {
                        if !r.label.isEmpty { Text(r.label).font(.footnote.weight(.semibold)) }
                        if !r.detail.isEmpty {
                            Text(r.detail).font(item.kind == "command" || item.kind == "fileChange" ? .footnote.monospaced() : .footnote)
                        }
                    }
                    .lineLimit(1)
                    .truncationMode(.tail)
                    Spacer(minLength: 4)
                    if !r.meta.isEmpty {
                        Text(r.meta).font(.caption.monospaced()).foregroundStyle(item.failed ? Tok.err : Tok.muted)
                    }
                    Image(systemName: "chevron.right")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(Tok.muted.opacity(0.7))
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                }
                .foregroundStyle(item.failed ? Tok.err : Tok.fg2)
                .padding(.horizontal, 8)
                .frame(minHeight: 34)
                .contentShape(Rectangle())
            }
            .buttonStyle(RowPress())
            .background(isSubagent ? Tok.subSoft : .clear, in: RoundedRectangle(cornerRadius: Radius.row))
            .contextMenu { copyMenu }
            if expanded, isSubagent, let o = item.output, !o.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    if !item.text.isEmpty {
                        Text(item.text).font(.footnote).foregroundStyle(Tok.fg2).lineLimit(6)
                    }
                    FoldedProse(text: o, size: 15)
                    HStack { Spacer(); CopyButton(markdown: o) }
                }
                .padding(12)
                .background(Tok.panel2, in: RoundedRectangle(cornerRadius: Radius.box))
                .overlay(alignment: .leading) { Rectangle().fill(Tok.sub).frame(width: 3).clipShape(RoundedRectangle(cornerRadius: 2)) }
                .overlay(RoundedRectangle(cornerRadius: Radius.box).stroke(Tok.line, lineWidth: 1))
                .contextMenu { CopyMenuItems(markdown: o) }
                .padding(.leading, 24)
            } else if expanded {
                ScrollView {
                    Text(detail.isEmpty ? "No output" : detail)
                        .font(.caption.monospaced())
                        .foregroundStyle(Tok.fg)
                        .lineSpacing(2)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 10).padding(.vertical, 8)
                }
                .frame(maxHeight: 320)
                .background(Tok.panel2, in: RoundedRectangle(cornerRadius: Radius.row))
                .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
                .overlay(alignment: .topTrailing) {
                    Menu { copyMenu } label: {
                        Image(systemName: "doc.on.doc")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Tok.fg2)
                            .frame(width: 36, height: 36)
                            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8))
                    } primaryAction: {
                        if let c = copyableCommand { copyToPasteboard(cleanCommand(c), label: "Command copied") }
                        else { copyToPasteboard(detail) }
                    }
                    .padding(4)
                    .accessibilityLabel(copyableCommand == nil ? "Copy" : "Copy command")
                    .accessibilityHint("Long-press for output and Slack")
                }
                .padding(.leading, 32)
                .padding(.bottom, 4)
            }
        }
    }

    // `.wthink`: a quiet left rule; the claude-coloured star; the text opens below.
    private var thinking: some View {
        let first = item.text.trimmingCharacters(in: .whitespacesAndNewlines).components(separatedBy: "\n").first ?? ""
        return VStack(alignment: .leading, spacing: 2) {
            Button(action: toggle) {
                HStack(spacing: 8) {
                    Text("✻").font(.footnote).foregroundStyle(running ? Tok.run : Tok.claude).frame(width: 16)
                    (Text(running ? "Thinking " : "Thought ").foregroundColor(Tok.muted)
                        + Text(expanded ? "" : first).foregroundColor(Tok.fg2))
                        .font(.footnote).italic().lineLimit(1)
                    Spacer(minLength: 4)
                    Image(systemName: "chevron.right")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(Tok.muted.opacity(0.7))
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                }
                .padding(.horizontal, 8)
                .frame(minHeight: 34)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            if expanded {
                Text(item.text.isEmpty ? "No text" : item.text)
                    .font(.subheadline).foregroundStyle(Tok.fg2).lineSpacing(3)
                    .textSelection(.enabled)
                    .padding(.leading, 30).padding(.trailing, 12).padding(.bottom, 8)
            }
        }
        .overlay(alignment: .leading) { Rectangle().fill(Tok.line2).frame(width: 2) }
    }

    // `.wtodo`: the TodoWrite checklist.
    private func todoBox(_ todos: [Todo]) -> some View {
        let done = todos.filter { $0.status == "completed" }.count
        return VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                Text("☰").foregroundStyle(Tok.muted)
                Text("Plan").foregroundStyle(Tok.fg2)
                Spacer()
                Text("\(done)/\(todos.count) done").font(Typo.mono(11.5)).foregroundStyle(Tok.muted)
            }
            .font(Typo.ui(12.5, .semibold))
            ForEach(Array(todos.enumerated()), id: \.offset) { _, t in
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    ZStack {
                        RoundedRectangle(cornerRadius: 4)
                            .fill(t.status == "completed" ? Tok.ok : .clear)
                        RoundedRectangle(cornerRadius: 4)
                            .stroke(t.status == "completed" ? Tok.ok : t.status == "in_progress" ? Tok.run : Tok.line2, lineWidth: 1.5)
                        if t.status == "completed" {
                            Text("✓").font(Typo.ui(9, .bold)).foregroundStyle(Tok.panel)
                        } else if t.status == "in_progress" {
                            Circle().fill(Tok.run).frame(width: 5, height: 5)
                        }
                    }
                    .frame(width: 14, height: 14)
                    .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 3 }
                    Text(t.content)
                        .font(Typo.ui(13, t.status == "in_progress" ? .semibold : .regular))
                        .foregroundStyle(t.status == "completed" ? Tok.muted : Tok.fg)
                        .strikethrough(t.status == "completed", color: Tok.muted)
                }
            }
        }
        .padding(.horizontal, 10).padding(.top, 6).padding(.bottom, 8)
        .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
    }
}

struct Todo { var content: String; var status: String }

/// "14:05" for a unix-millis time today, else "Mar 3".
func clock(_ ms: Int64) -> String {
    guard ms > 0 else { return "" }
    let d = Date(timeIntervalSince1970: Double(ms) / 1000)
    return Calendar.current.isDateInToday(d) ? d.formatted(date: .omitted, time: .shortened) : d.formatted(.dateTime.month(.abbreviated).day())
}

/// A TodoWrite payload ({todos:[...]} or an array) or a markdown checklist; nil when the text is prose.
func parseTodos(_ text: String) -> [Todo]? {
    let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !t.isEmpty else { return nil }
    if t.hasPrefix("{") || t.hasPrefix("["), let data = t.data(using: .utf8), let j = try? JSONSerialization.jsonObject(with: data) {
        let arr = (j as? [Any]) ?? ((j as? [String: Any])?["todos"] as? [Any]) ?? ((j as? [String: Any])?["plan"] as? [Any])
        if let arr, !arr.isEmpty {
            return arr.map { x in
                if let s = x as? String { return Todo(content: s, status: "pending") }
                let o = x as? [String: Any] ?? [:]
                let st = o["status"] as? String ?? ""
                let status = st == "completed" ? "completed" : (st == "in_progress" || st == "inProgress") ? "in_progress" : "pending"
                let c = (o["status"] as? String == "in_progress" ? o["activeForm"] as? String : nil)
                    ?? o["content"] as? String ?? o["step"] as? String ?? o["text"] as? String ?? ""
                return Todo(content: c, status: status)
            }
        }
    }
    let lines = t.components(separatedBy: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    let re = try? NSRegularExpression(pattern: #"^(?:[-*]|\d+[.)])\s+(?:\[([ xX~>])\]\s*)?(.+)$"#)
    guard lines.count >= 2, let re else { return nil }
    var out: [Todo] = []
    for l in lines {
        let ns = l as NSString
        guard let m = re.firstMatch(in: l, range: NSRange(location: 0, length: ns.length)) else { return nil }
        let box = m.range(at: 1).location == NSNotFound ? " " : ns.substring(with: m.range(at: 1)).lowercased()
        out.append(Todo(content: ns.substring(with: m.range(at: 2)),
                        status: box == "x" ? "completed" : (box == "~" || box == ">") ? "in_progress" : "pending"))
    }
    return out
}

/// Two-finger tap toggles full screen; in full screen a long scroll up brings the bars back (iOS 18 and later).
struct FullscreenGestures: ViewModifier {
    @Binding var fullscreen: Bool
    @State private var anchor: CGFloat?
    @State private var dragging = false

    func body(content: Content) -> some View {
        if #available(iOS 18.0, *) {
            content
                .gesture(TwoFingerTap { withAnimation(.snappy) { fullscreen.toggle() } })
                .onScrollPhaseChange { _, phase in
                    dragging = phase == .interacting || phase == .decelerating
                    if !dragging { anchor = nil }
                }
                .onScrollGeometryChange(for: CGFloat.self) { $0.contentOffset.y } action: { old, new in
                    // Only a scroll the reader drives counts; layout shifts from hiding the bars do not.
                    guard fullscreen, dragging else { anchor = nil; return }
                    if new > old { anchor = new; return }
                    if let a = anchor, a - new > 160 { withAnimation(.snappy) { fullscreen = false }; anchor = nil }
                }
        } else {
            content
        }
    }
}

@available(iOS 18.0, *)
struct TwoFingerTap: UIGestureRecognizerRepresentable {
    let action: () -> Void
    func makeUIGestureRecognizer(context: Context) -> UITapGestureRecognizer {
        let g = UITapGestureRecognizer()
        g.numberOfTouchesRequired = 2
        g.cancelsTouchesInView = false
        return g
    }
    func handleUIGestureRecognizerAction(_ recognizer: UITapGestureRecognizer, context: Context) {
        if recognizer.state == .ended { action() }
    }
}
