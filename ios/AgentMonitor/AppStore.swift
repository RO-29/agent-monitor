import Foundation
import Observation
import UIKit

enum AppTab: Hashable {
    case threads, approvals, alerts, settings
}

/// Screens pushed inside the Settings tab.
enum SettingsRoute: Hashable {
    case server, agents, talks, panes, alerts, storage, commands, machines
}

enum ConnectionState: Equatable {
    case connecting
    case connected
    case disconnected(String)

    var label: String {
        switch self {
        case .connecting: return "Connecting…"
        case .connected: return "Live"
        case .disconnected(let why): return why.isEmpty ? "Offline" : "Offline · \(why)"
        }
    }
}

/// Single source of truth for the whole app. Hydrated over REST on connect,
/// then kept live by WebSocket events.
@MainActor
@Observable
final class AppStore {
    var sessionsByID: [String: Session] = [:]
    var permissionsByID: [String: PermissionRequest] = [:]
    var talksByID: [String: Talk] = [:]
    var panesByID: [String: PaneRegistration] = [:]

    // Session linkage: computed by the daemon at /api/chains.
    var chains: [Chain] = []
    var chainOf: [String: String] = [:]              // sessionId → chain key
    var spawnParent: [String: String] = [:]          // childId → parentId
    var spawnChildren: [String: [SpawnChild]] = [:]   // parentId → children

    // Hook approvals (/api/approvals), pending only; kept live by approval events on /ws.
    var approvalsByID: [String: Approval] = [:] { didSet { recomputeNeedThreads() } }

    // The daemon's one "needs you" list; nil until loaded, and nil on an older daemon (404).
    var needsYouList: [NeedsYouItem]? { didSet { recomputeNeedThreads() } }
    var needsYouSupported: Bool?
    private var needsYouTotal = 0
    private var needsYouPoll: Task<Void, Never>?
    private var needsYouSoon: Task<Void, Never>?
    private var appActive = true
    /// Thread ids that hold a needs-you item, with their parents.
    private(set) var needThreads: Set<String> = []

    // Other machines (Settings, Machines): each one's needs-you list and last error, keyed by machine id.
    var machines: [Machine] = Machines.all
    var remoteNeeds: [String: [NeedsYouItem]] = [:]
    var remoteDown: [String: String] = [:]
    @ObservationIgnored var remoteSeen: Set<String> = []
    @ObservationIgnored var remotePoll: Task<Void, Never>?
    #if DEBUG
    /// Replaces the daemon list for simulator screenshots.
    var debugNeedsYou: [NeedsYouItem]? { didSet { recomputeNeedThreads() } }
    #endif

    // One-tap decisions from the home cards, keyed by approval id so they outlive a recycled row.
    var pendingDecisions: [String: PendingDecision] = [:]
    var decisionErrors: [String: String] = [:]
    var approvalSheet: String?
    /// The approval a tapped notification points at; Approvals scrolls to it and tints it.
    var focusApproval: String?
    /// Chat read full screen; kept until the app quits.
    var chatFullscreen = false
    /// A thread id from a notification tap that the thread list did not know yet; re-resolved after each load.
    private var pendingOpen: String?
    private var decisionTimers: [String: Task<Void, Never>] = [:]

    // Workspace threads (/api/v3/threads) and the session id -> thread id map.
    var threads: [V3Thread] = [] { didSet { recomputeNeedThreads() } }
    var threadOf: [String: String] = [:] { didSet { recomputeNeedThreads() } }
    var threadsError: String?
    /// True after the first thread list load that succeeded; before it, an empty list means "not loaded yet".
    var threadsLoaded = false

    // Notification centre (/api/notify), newest first.
    var alerts: [AlertItem] = []
    var alertsError: String?

    // Navigation state shared by tabs, so an alert can open a thread.
    var tab: AppTab = .threads
    var workspacePath: [String] = []
    var settingsPath: [SettingsRoute] = []

    // New thread sheet, reachable from every tab; `newThreadFolder` preselects a folder.
    var showNewThread = false
    var newThreadFolder: String?

    // Agents the daemon can start (GET /api/orch/agents); nil until loaded.
    var orchAgents: [OAgentInfo]?

    var connection: ConnectionState = .disconnected("")
    var lastError: String?

    private var live: LiveConnection?
    private var alertStream: Task<Void, Never>?
    private var lastAlertID: Int64 = 0
    private var threadsReload: Task<Void, Never>?
    private var lastThreadsLoad = Date.distantPast
    private let streamSession: URLSession = {
        let cfg = URLSessionConfiguration.default
        cfg.timeoutIntervalForRequest = 60   // the daemon sends a heartbeat every 15 s
        cfg.timeoutIntervalForResource = 24 * 3600
        return URLSession(configuration: cfg)
    }()

    // Notification gating: don't alert for approvals that already existed when we
    // (re)connected: only genuinely new ones. `notifiedApprovals` also dedupes
    // perm-add replays on reconnect and repeated awaiting upserts.
    private var notifsReady = false
    private var notifiedApprovals: Set<String> = []

    // MARK: Derived views

    var sessions: [Session] {
        sessionsByID.values.sorted { a, b in
            // Attention first, then most-recently-active.
            if a.state.needsAttention != b.state.needsAttention {
                return a.state.needsAttention
            }
            return a.lastActivityAt > b.lastActivityAt
        }
    }

    var attentionCount: Int {
        guard usesNeedsYou else { return sessionsByID.values.filter { $0.state.needsAttention }.count }
        return Set(needsYouItems.filter { $0.kind == "prompt" || $0.kind == "question" }.compactMap(\.sessionId)).count
    }

    var permissions: [PermissionRequest] {
        permissionsByID.values.sorted { $0.createdAt > $1.createdAt }
    }

    var pendingTalks: [Talk] {
        talksByID.values.filter { $0.isPending }.sorted { $0.createdAt > $1.createdAt }
    }

    var talks: [Talk] {
        talksByID.values.sorted { $0.createdAt > $1.createdAt }
    }

    var panes: [PaneRegistration] {
        panesByID.values.sorted { $0.lastSeenAt > $1.lastSeenAt }
    }


    /// Sessions parked waiting for a human: a native TUI permission prompt or a
    /// reply. These surface in the Approvals tab alongside MCP requests; answered
    /// over the tmux pane bridge. Most-recently-active first.
    var awaitingSessions: [Session] {
        let held = Set(approvalsByID.values.map { $0.sessionId })
        return sessionsByID.values
            .filter { ($0.state == .awaitingPermission || $0.state == .awaitingInput) && !held.contains($0.id) }
            .sorted { $0.lastActivityAt > $1.lastActivityAt }
    }

    /// The Approvals badge: the daemon's `count`, or the older sources on a daemon without /api/needs-you.
    var approvalsCount: Int {
        #if DEBUG
        if let d = debugNeedsYou { return d.count }
        #endif
        if needsYouList != nil { return needsYouTotal }
        return approvals.count + permissionsByID.count + awaitingSessions.count
    }

    var usesNeedsYou: Bool {
        #if DEBUG
        if debugNeedsYou != nil { return true }
        #endif
        return needsYouList != nil
    }

    /// What the Approvals tab draws, oldest first.
    var needsYouItems: [NeedsYouItem] {
        #if DEBUG
        if let d = debugNeedsYou { return d }
        #endif
        return needsYouList ?? legacyNeedsYou
    }

    /// The older daemon's sources in the same shape: hook approvals, MCP requests, waiting sessions.
    private var legacyNeedsYou: [NeedsYouItem] {
        var out = approvals.map {
            NeedsYouItem(kind: "approval", key: $0.id, sessionId: $0.sessionId, agent: $0.agent, cwd: $0.cwd,
                         at: $0.createdAt, answerable: true, approval: $0, legacy: true)
        }
        out += permissions.map {
            NeedsYouItem(kind: "mcp", key: "perm:" + $0.id, sessionId: $0.sessionId, cwd: $0.cwd,
                         at: $0.createdAt, answerable: true, mcp: $0, legacy: true)
        }
        out += awaitingSessions.map {
            NeedsYouItem(kind: "prompt", key: "tp:" + $0.id, sessionId: $0.id, agent: $0.tool, cwd: $0.cwd, title: $0.headline,
                         at: $0.lastActivityAt, answerable: hasPane($0.id), message: $0.permissionMessage ?? $0.lastMessage, legacy: true)
        }
        return out.sorted { $0.at < $1.at }
    }

    /// The thread an item belongs to.
    func threadID(of it: NeedsYouItem) -> String? {
        if it.kind == "approval", let a = approvalsByID[it.key] ?? it.approval { return threadID(of: a) }
        if let tid = it.threadId, !tid.isEmpty { return resolveThreadID(tid) }
        guard let sid = it.sessionId, !sid.isEmpty else { return nil }
        return threadOf[sid] ?? sid
    }

    func needsYouItem(forThread t: V3Thread) -> NeedsYouItem? {
        let sids = Set(t.sessionIDs)
        return needsYouItems.first { threadID(of: $0) == t.id || sids.contains($0.sessionId ?? "") }
    }

    private func recomputeNeedThreads() {
        guard usesNeedsYou else { if !needThreads.isEmpty { needThreads = [] }; return }
        var ids = Set(needsYouItems.compactMap { threadID(of: $0) })
        let sids = Set(needsYouItems.compactMap(\.sessionId))
        for t in threads where !sids.isDisjoint(with: t.sessionIDs) { ids.insert(t.id) }
        for t in threads where ids.contains(t.id) { if let p = t.parentThread, !p.isEmpty { ids.insert(p) } }
        if ids != needThreads { needThreads = ids }
    }

    // MARK: Hook approvals, threads, alerts (derived)

    var approvals: [Approval] {
        approvalsByID.values.filter { $0.isPending }.sorted { $0.createdAt < $1.createdAt }
    }

    func thread(_ id: String) -> V3Thread? { threads.first { $0.id == id } }

    /// Thread id an approval belongs to: a driven thread, else the thread of its session.
    func threadID(of a: Approval) -> String {
        if let d = a.drivenId, !d.isEmpty, let t = threads.first(where: { $0.drivenId == d || $0.id == d }) { return t.id }
        return threadOf[a.sessionId] ?? a.sessionId
    }

    func approvals(forThread t: V3Thread) -> [Approval] {
        let sids = Set(t.sessionIDs)
        return approvals.filter { threadID(of: $0) == t.id || sids.contains($0.sessionId) }
    }

    func approvals(forSession sid: String) -> [Approval] {
        approvals.filter { $0.sessionId == sid }
    }

    /// The live session state wins over the thread's indexed state.
    func liveState(_ t: V3Thread) -> AgentState {
        if let sid = t.latestSession, let s = sessionsByID[sid] { return s.state }
        return t.state
    }

    func group(of t: V3Thread) -> ThreadGroup {
        let today = Calendar.current.startOfDay(for: Date()).timeIntervalSince1970 * 1000
        let recent = Double(t.lastAt) >= today
        if t.isDriven, let g = t.group.flatMap(ThreadGroup.init(rawValue:)) {
            // The daemon's group knows settle and snooze; derive one only for an older daemon.
            if g != .need, !approvals(forThread: t).isEmpty { return .need }
            return g
        }
        if t.isDriven {
            // With the daemon list, a failed thread needs you only until it is dismissed (settled).
            let failed = usesNeedsYou ? needThreads.contains(t.id) : t.status == "error"
            if !approvals(forThread: t).isEmpty || t.status == "waiting" || failed { return .need }
            if t.status == "running" || t.status == "starting" { return .working }
            return recent && t.status != "closed" ? .ready : .settled
        }
        let st = liveState(t)
        if usesNeedsYou {
            // Only a real request counts; a session that ended its turn is ready, not waiting.
            if needThreads.contains(t.id) || !approvals(forThread: t).isEmpty { return .need }
            if st == .running { return .working }
            return recent ? .ready : .settled
        }
        // Live approvals from /ws win over the list's pending count, which lags by up to 30 s.
        if !approvals(forThread: t).isEmpty || t.attention == true
            || (t.childAttention ?? 0) > 0 || st.needsAttention { return .need }
        if st == .running { return .working }
        return recent ? .ready : .settled
    }

    func threads(in g: ThreadGroup) -> [V3Thread] {
        threads.filter { $0.parentThread == nil || $0.parentThread == "" }
            .filter { group(of: $0) == g }
            .sorted { $0.lastAt > $1.lastAt }
    }

    var needsYouThreadCount: Int { threads(in: .need).count }

    var alertsUnread: Int { alerts.filter { !$0.isRead }.count }

    /// Maps an alert's threadId (a thread, a driven id, or a session id) to a thread id to open.
    func resolveThreadID(_ id: String) -> String {
        if thread(id) != nil { return id }
        if let t = threadOf[id] { return t }
        if let t = threads.first(where: { $0.drivenId == id }) { return t.id }
        return id
    }

    func openThread(_ id: String) {
        showNewThread = false
        chatFullscreen = false
        approvalSheet = nil
        tab = .threads
        let rid = resolveThreadID(id)
        pendingOpen = thread(rid) == nil ? id : nil
        workspacePath = [rid]
    }

    private var presenceTask: Task<Void, Never>?

    /// Tells the daemon the app is open, so Bark pushes wait while the app alerts by itself; it lapses after 90 s.
    func setForeground(_ active: Bool) {
        presenceTask?.cancel()
        appActive = active
        setRemoteForeground(active)
        if active {
            Task { await loadNeedsYou() }
            presenceTask = Task {
                while !Task.isCancelled {
                    _ = try? await APIClient.shared.post("/api/push/presence", body: ["active": true])
                    try? await Task.sleep(for: .seconds(30))
                }
            }
        } else {
            Task { _ = try? await APIClient.shared.post("/api/push/presence", body: ["active": false]) }
        }
    }

    /// Routes a tapped notification: a thread chat, or Approvals with that request in view.
    func open(_ route: NotifyRoute) {
        switch route {
        case .thread(let id): openThread(id)
        case .approvals(let id):
            showNewThread = false
            approvalSheet = nil
            focusApproval = id
            tab = .approvals
        case .alerts:
            showNewThread = false
            approvalSheet = nil
            tab = .alerts
        case .web(let u):
            UIApplication.shared.open(u)
        }
    }

    /// Does this session have a registered tmux pane we can drive? (agentId == session id)
    func hasPane(_ sessionID: String) -> Bool { panesByID[sessionID] != nil }

    /// The chain a session belongs to, or nil if it isn't linked to any other.
    func chain(for id: String) -> Chain? {
        guard let key = chainOf[id] else { return nil }
        return chains.first { $0.id == key }
    }

    // MARK: Lifecycle

    func start() {
        connect()
        startNeedsYouPoll()
    }

    // MARK: Needs you

    /// Polls /api/needs-you every 3 s while the app is active and connected; stops on a 404.
    private func startNeedsYouPoll() {
        needsYouPoll?.cancel()
        needsYouPoll = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                if self.appActive, self.connection == .connected, self.needsYouSupported != false { await self.loadNeedsYou() }
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }

    /// Load order: a response older than one already applied, or one that started before a local removal, is dropped.
    @ObservationIgnored private var needsYouSeq = 0
    @ObservationIgnored private var needsYouApplied = 0
    @ObservationIgnored private var needsYouRemovals = 0

    func loadNeedsYou() async {
        needsYouSeq += 1
        let mine = needsYouSeq, removals = needsYouRemovals
        do {
            let r = try await APIClient.shared.needsYou()
            guard mine > needsYouApplied, removals == needsYouRemovals else { return }
            needsYouApplied = mine
            let items = r.items ?? []
            needsYouSupported = true
            notifyNeeds(items)
            needsYouList = items
            needsYouTotal = r.count ?? items.count
        } catch let e as APIError where e.status == 404 {
            guard mine > needsYouApplied else { return }
            needsYouSupported = false
            needsYouList = nil
        } catch {
            // Keep the last list through a network blip.
        }
    }

    /// Reloads the list shortly after a live event, so bursts make one request.
    private func soonNeedsYou() {
        guard needsYouSupported == true, needsYouSoon == nil else { return }
        needsYouSoon = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 400_000_000)
            guard let self, !Task.isCancelled else { return }
            self.needsYouSoon = nil
            await self.loadNeedsYou()
        }
    }

    /// Removes an answered item at once; the next load confirms it.
    private func pruneNeedsYou(_ match: (NeedsYouItem) -> Bool) {
        guard let list = needsYouList else { return }
        needsYouRemovals += 1 // a load already on the way still holds the removed item
        let next = list.filter { !match($0) }
        if next.count != list.count {
            needsYouTotal = max(0, needsYouTotal - (list.count - next.count))
            needsYouList = next
        }
    }

    /// One alert per terminal question or prompt; the alert clears when the item leaves the list.
    private func notifyNeeds(_ items: [NeedsYouItem]) {
        let kinds: Set<String> = ["prompt", "question"]
        let now = Set(items.filter { kinds.contains($0.kind) }.compactMap(\.sessionId))
        let before = Set((needsYouList ?? []).filter { kinds.contains($0.kind) }.compactMap(\.sessionId))
        for sid in before.subtracting(now) { clearApprovalNotification(sid) }
        for it in items where kinds.contains(it.kind) {
            guard let sid = it.sessionId else { continue }
            let who = agentName(it.agent ?? "")
            maybeNotifyApproval(id: sid, title: it.kind == "question" ? "\(who) is asking" : "\(who) needs permission",
                                body: it.title ?? it.project, session: true, question: it.kind == "question")
        }
    }

    func connect() {
        live?.stop()
        if needsYouSupported == false { needsYouSupported = nil }
        connection = .connecting
        let conn = LiveConnection(
            onEvent: { [weak self] ev in self?.apply(ev) },
            onState: { [weak self] st in self?.connection = st }
        )
        live = conn
        conn.start()
        Task {
            await hydrate()
            startAlertStream()
        }
    }

    /// Pull full snapshots via REST so initial state is complete regardless of
    /// what the socket replays.
    func hydrate() async {
        do {
            async let s = APIClient.shared.sessions()
            async let p = APIClient.shared.permissions()
            async let t = APIClient.shared.talks()
            async let pn = APIClient.shared.panes()
            let (sessions, perms, talks, panes) = try await (s, p, t, pn)
            for x in sessions { sessionsByID[x.id] = x }
            permissionsByID = Dictionary(uniqueKeysWithValues: perms.map { ($0.id, $0) })
            talksByID = Dictionary(uniqueKeysWithValues: talks.map { ($0.id, $0) })
            panesByID = Dictionary(uniqueKeysWithValues: panes.map { ($0.id, $0) })
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
        await fetchChains()
        await loadApprovals()
        await loadNeedsYou()
        await loadThreads()
        await loadAlerts()
        // Treat everything pending right now as already-seen so we only alert on
        // approvals that arrive *after* this point (avoids a burst on launch /
        // reconnect), then arm notifications.
        notifiedApprovals.formUnion(permissionsByID.keys)
        notifiedApprovals.formUnion(awaitingSessions.map { $0.id })
        notifiedApprovals.formUnion(needsYouItems.compactMap(\.sessionId))
        notifsReady = true
    }

    func loadApprovals() async {
        do {
            let r = try await APIClient.shared.approvals()
            var next = Dictionary((r.pending ?? []).map { ($0.id, $0) }, uniquingKeysWith: { _, b in b })
            #if DEBUG
            for (k, v) in approvalsByID where k.hasPrefix("sample-") { next[k] = v }
            #endif
            approvalsByID = next
        } catch {
            // An older daemon has no /api/approvals; the other tabs still work.
        }
    }

    func loadThreads() async {
        lastThreadsLoad = Date()
        do {
            let r = try await APIClient.shared.threadsCompact()
            threads = r.threads ?? []
            threadOf = r.threadOf ?? [:]
            #if DEBUG
            if DebugRoute.sample { threads.insert(DebugRoute.sampleThread, at: 0) }
            #endif
            threadsError = nil
            threadsLoaded = true
            if let p = pendingOpen, workspacePath.first.map({ $0 == p || $0 == resolveThreadID(p) }) == true {
                let rid = resolveThreadID(p)
                if thread(rid) != nil {
                    pendingOpen = nil
                    if workspacePath.first != rid { workspacePath[0] = rid }
                }
            }
        } catch {
            threadsError = error.localizedDescription
            #if DEBUG
            if DebugRoute.sample, thread(DebugRoute.sampleThread.id) == nil { threads.insert(DebugRoute.sampleThread, at: 0); threadsError = nil }
            #endif
        }
    }

    /// The list is large (megabytes on a busy Mac), so events reload it at most every 15 s.
    private func soonThreads() {
        guard threadsReload == nil else { return }
        let wait = max(2.5, 15 - Date().timeIntervalSince(lastThreadsLoad))
        threadsReload = Task { [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(wait * 1_000_000_000))
            guard let self, !Task.isCancelled else { return }
            self.threadsReload = nil
            await self.loadThreads()
        }
    }

    /// Reloads the thread list when it is older than `maxAge` seconds.
    func refreshThreadsIfStale(_ maxAge: TimeInterval = 15) async {
        if Date().timeIntervalSince(lastThreadsLoad) >= maxAge { await loadThreads() }
    }

    func applyDecision(_ a: Approval) {
        if a.isPending { approvalsByID[a.id] = a } else { dropApproval(a.id) }
    }

    func dropApproval(_ id: String) {
        approvalsByID.removeValue(forKey: id)
        pruneNeedsYou { $0.kind == "approval" && $0.key == id }
    }

    /// Sends a decision and applies the daemon's answer; the POST may finish after the app leaves the screen.
    func decide(_ a: Approval, _ body: DecideBody) async throws {
        let bg = BackgroundTask("decide \(a.id)")
        defer { bg.end() }
        let r = try await APIClient.shared.decide(a.id, body)
        if let r { applyDecision(r) } else { dropApproval(a.id) }
    }

    static let undoWindow: TimeInterval = 3

    /// Holds a decision for the undo window, then posts it.
    func queueDecision(_ a: Approval, _ body: DecideBody, label: String) {
        guard pendingDecisions[a.id] == nil else { return }
        decisionErrors[a.id] = nil
        pendingDecisions[a.id] = PendingDecision(approval: a, body: body, label: label, start: Date())
        decisionTimers[a.id] = Task { [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(Self.undoWindow * 1_000_000_000))
            guard !Task.isCancelled else { return }
            self?.flushDecision(a.id)
        }
    }

    func undoDecision(_ id: String) {
        guard pendingDecisions[id]?.phase == .waiting else { return }
        decisionTimers.removeValue(forKey: id)?.cancel()
        pendingDecisions.removeValue(forKey: id)
    }

    /// Posts a held decision now: when its time runs out, its card leaves the screen, or the app leaves the foreground.
    func flushDecision(_ id: String) {
        guard var p = pendingDecisions[id], p.phase == .waiting else { return }
        decisionTimers.removeValue(forKey: id)?.cancel()
        p.phase = .sending
        pendingDecisions[id] = p
        #if DEBUG
        if id.hasPrefix("sample-") {
            Task {
                try? await Task.sleep(nanoseconds: 600_000_000)
                finishDecision(id, ok: true, behavior: p.body.behavior)
            }
            return
        }
        #endif
        Task {
            do {
                try await decide(p.approval, p.body)
                finishDecision(id, ok: true, behavior: p.body.behavior)
            } catch let e as APIError where e.status == 409 {
                decisionErrors[id] = "Answered elsewhere."
                finishDecision(id, ok: false, behavior: p.body.behavior)
                await loadApprovals()
            } catch {
                decisionErrors[id] = error.localizedDescription
                finishDecision(id, ok: false, behavior: p.body.behavior)
            }
        }
    }

    func flushPendingDecisions() {
        for id in pendingDecisions.keys { flushDecision(id) }
    }

    private func finishDecision(_ id: String, ok: Bool, behavior: String) {
        let haptic = UINotificationFeedbackGenerator()
        if ok {
            pendingDecisions[id]?.phase = .sent
            haptic.notificationOccurred(behavior == "allow" ? .success : .warning)
            Task {
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                if pendingDecisions[id]?.phase == .sent { pendingDecisions.removeValue(forKey: id) }
            }
        } else {
            pendingDecisions.removeValue(forKey: id)
            haptic.notificationOccurred(.error)
        }
    }

    /// Folders of recent threads, newest first (NewThread.tsx useRecentFolders).
    func recentFolders(_ n: Int = 12) -> [String] {
        var seen: [String: Int64] = [:]
        for t in threads where !t.cwd.isEmpty { seen[t.cwd] = max(seen[t.cwd] ?? 0, t.lastAt) }
        return seen.sorted { $0.value > $1.value }.prefix(n).map(\.key)
    }

    func newThread(in folder: String? = nil) {
        newThreadFolder = folder
        showNewThread = true
    }

    func loadOrchAgents() async {
        if let a = try? await APIClient.shared.orchAgents(), !a.isEmpty { orchAgents = a }
    }

    /// Child threads of a thread row (parentThread links).
    func childThreads(of id: String) -> [V3Thread] {
        threads.filter { $0.parentThread == id }.sorted { $0.lastAt > $1.lastAt }
    }

    // MARK: Alerts

    func loadAlerts() async {
        do {
            let r = try await APIClient.shared.alerts()
            let list = (r.items ?? []).sorted { $0.at > $1.at }
            alerts = list
            lastAlertID = max(lastAlertID, list.compactMap { Int64($0.id) }.max() ?? 0)
            alertsError = nil
        } catch {
            alertsError = error.localizedDescription
        }
    }

    func markAlertsRead(_ ids: [String]) async {
        let unread = ids.filter { id in alerts.contains { $0.id == id && !$0.isRead } }
        guard !unread.isEmpty else { return }
        for i in alerts.indices where unread.contains(alerts[i].id) { alerts[i].read = true }
        try? await APIClient.shared.markAlertsRead(ids: unread)
    }

    func markAllAlertsRead() async {
        for i in alerts.indices { alerts[i].read = true }
        try? await APIClient.shared.markAlertsRead(all: true)
    }

    private func receiveAlert(_ a: AlertItem) {
        if let n = Int64(a.id) { lastAlertID = max(lastAlertID, n) }
        if let i = alerts.firstIndex(where: { $0.id == a.id }) {
            alerts[i] = a
            if a.resolved ?? false { Notifier.shared.clearAlert(id: a.id) }
            return
        }
        alerts.insert(a, at: 0)
        if alerts.count > 300 { alerts.removeLast(alerts.count - 300) }
        // Alert only for fresh needs-you items; a reconnect replays older ones.
        let fresh = Int64(Date().timeIntervalSince1970 * 1000) - a.at < 120_000
        if !(a.quiet ?? false), !(a.resolved ?? false), fresh {
            var kind = NotifyKind.from(alertKind: a.kind)
            if kind == .needsYou, let t = a.threadId, approvals.contains(where: { threadID(of: $0) == resolveThreadID(t) && $0.kind == "question" }) {
                kind = .question
            }
            Notifier.shared.alert(id: a.id, kind: kind, title: a.title, body: a.body ?? "", threadId: a.threadId, url: a.url)
        }
    }

    /// Follows GET /api/notify/stream (SSE) and reconnects with ?after= the last seen id.
    func startAlertStream() {
        alertStream?.cancel()
        alertStream = Task { [weak self] in
            var attempt = 0
            while !Task.isCancelled {
                guard let self else { return }
                do {
                    try await self.readAlertStream()
                    attempt = 0
                } catch let e as APIError where e.status == 404 {
                    return  // an older daemon without the notification centre
                } catch {
                    attempt += 1
                }
                if Task.isCancelled { return }
                let delay = min(Double(max(attempt, 1)) * 2, 30)
                try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            }
        }
    }

    private func readAlertStream() async throws {
        let req = try APIClient.shared.streamRequest("/api/notify/stream?after=\(lastAlertID)")
        let (bytes, resp) = try await streamSession.bytes(for: req)
        if let http = resp as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
            throw APIError(message: "HTTP \(http.statusCode)", status: http.statusCode)
        }
        // Each event is one "data:" line of JSON, so the blank separator lines are not needed.
        for try await line in bytes.lines {
            if Task.isCancelled { return }
            guard line.hasPrefix("data:") else { continue }
            let json = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
            guard let d = json.data(using: .utf8),
                  let a = try? JSONDecoder().decode(AlertItem.self, from: d) else { continue }
            receiveAlert(a)
        }
    }

    /// Fire a distinct-tune notification for a new approval ask, once per id.
    private func maybeNotifyApproval(id: String, title: String, body: String, session: Bool = false, question: Bool = false) {
        guard notifsReady, !notifiedApprovals.contains(id) else { return }
        notifiedApprovals.insert(id)
        if session {
            Notifier.shared.sessionAsk(sessionID: id, question: question, title: title, body: body)
        } else {
            Notifier.shared.approvalAsk(id: id, title: title, body: body)
        }
    }

    private func clearApprovalNotification(_ id: String) {
        notifiedApprovals.remove(id)
        Notifier.shared.clear(id: id)
    }

    /// Refresh the linkage graph. Chains aren't pushed over the socket (they're
    /// derived from the whole session set), so recompute on hydrate and on demand.
    func fetchChains() async {
        guard let r = try? await APIClient.shared.chains() else { return }
        chains = r.chains ?? []
        chainOf = r.chainOf ?? [:]
        spawnParent = r.spawnParent ?? [:]
        spawnChildren = r.spawnChildren ?? [:]
    }

    // MARK: Event application

    private func apply(_ ev: WSEvent) {
        switch ev {
        case .snapshot(let list):
            for s in list { sessionsByID[s.id] = s }
            Task { await loadApprovals() }
            soonThreads()
            soonNeedsYou()
        case .upsert(let s):
            let prev = sessionsByID[s.id]
            sessionsByID[s.id] = s
            let awaiting = s.state == .awaitingPermission || s.state == .awaitingInput
            soonThreads()
            if prev?.state != s.state { soonNeedsYou() }
            // With /api/needs-you the list raises the alerts; a held hook approval raises its own.
            let held = approvalsByID.values.contains { $0.sessionId == s.id }
            if needsYouSupported == true {
                if !awaiting { clearApprovalNotification(s.id) }
            } else if awaiting, prev?.state != s.state, !held {
                maybeNotifyApproval(
                    id: s.id,
                    title: s.state == .awaitingPermission ? "\(agentName(s.tool)) needs permission" : "\(agentName(s.tool)) is asking",
                    body: s.headline, session: true, question: s.state == .awaitingInput)
            } else if !awaiting {
                clearApprovalNotification(s.id)
            }
        case .remove(let id):
            sessionsByID.removeValue(forKey: id)
            clearApprovalNotification(id)
            pruneNeedsYou { $0.sessionId == id && $0.kind != "approval" }
            soonThreads()

        case .approval(let a):
            applyDecision(a)
            soonNeedsYou()
        case .approvalDone(let a):
            dropApproval(a.id)
            soonNeedsYou()

        case .permAdd(let r):
            permissionsByID[r.id] = r
            let where_ = r.cwd.map { " · \(($0 as NSString).lastPathComponent)" } ?? ""
            maybeNotifyApproval(id: r.id, title: "Approval needed", body: "\(humanToolName(r.toolName))\(where_)")
            soonNeedsYou()
        case .permRemove(let id):
            permissionsByID.removeValue(forKey: id)
            pruneNeedsYou { $0.key == "perm:" + id }
            clearApprovalNotification(id)

        case .paneRegister(let r), .paneName(let r):
            panesByID[r.agentId] = r
        case .paneForget(let id):
            panesByID.removeValue(forKey: id)

        case .talkRequest(let t), .talkResolved(let t):
            talksByID[t.id] = t

        case .unknown:
            break
        }
    }

    // MARK: Optimistic local removals (server also broadcasts, this hides lag)

    func dropPermission(_ id: String) {
        permissionsByID.removeValue(forKey: id)
        pruneNeedsYou { $0.key == "perm:" + id }
    }
}

struct PendingDecision {
    enum Phase { case waiting, sending, sent }
    var approval: Approval
    var body: DecideBody
    var label: String
    var start: Date
    var phase: Phase = .waiting
}

/// UIKit background time so a POST started just before the app is suspended still completes.
@MainActor
final class BackgroundTask {
    private var id: UIBackgroundTaskIdentifier = .invalid

    init(_ name: String) {
        id = UIApplication.shared.beginBackgroundTask(withName: name) { [weak self] in self?.end() }
    }

    func end() {
        guard id != .invalid else { return }
        UIApplication.shared.endBackgroundTask(id)
        id = .invalid
    }
}
