import Foundation
import Observation

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

    // Session linkage — computed by the daemon at /api/chains.
    var chains: [Chain] = []
    var chainOf: [String: String] = [:]              // sessionId → chain key
    var spawnParent: [String: String] = [:]          // childId → parentId
    var spawnChildren: [String: [SpawnChild]] = [:]   // parentId → children

    var connection: ConnectionState = .disconnected("")
    var lastError: String?

    private var live: LiveConnection?

    // Notification gating: don't alert for approvals that already existed when we
    // (re)connected — only genuinely new ones. `notifiedApprovals` also dedupes
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
        sessionsByID.values.filter { $0.state.needsAttention }.count
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

    var badgeCount: Int { attentionCount + permissionsByID.count + pendingTalks.count }

    /// Sessions parked waiting for a human — a native TUI permission prompt or a
    /// reply. These surface in the Approvals tab alongside MCP requests; answered
    /// over the tmux pane bridge. Most-recently-active first.
    var awaitingSessions: [Session] {
        sessionsByID.values
            .filter { $0.state == .awaitingPermission || $0.state == .awaitingInput }
            .sorted { $0.lastActivityAt > $1.lastActivityAt }
    }

    /// Total pending approvals across both channels — drives the tab badge.
    var approvalsCount: Int { permissionsByID.count + awaitingSessions.count }

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
    }

    func connect() {
        live?.stop()
        connection = .connecting
        let conn = LiveConnection(
            onEvent: { [weak self] ev in self?.apply(ev) },
            onState: { [weak self] st in self?.connection = st }
        )
        live = conn
        conn.start()
        Task { await hydrate() }
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
        // Treat everything pending right now as already-seen so we only alert on
        // approvals that arrive *after* this point (avoids a burst on launch /
        // reconnect), then arm notifications.
        notifiedApprovals.formUnion(permissionsByID.keys)
        notifiedApprovals.formUnion(awaitingSessions.map { $0.id })
        notifsReady = true
    }

    /// Fire a distinct-tune notification for a new approval ask, once per id.
    private func maybeNotifyApproval(id: String, title: String, body: String) {
        guard notifsReady, !notifiedApprovals.contains(id) else { return }
        notifiedApprovals.insert(id)
        Notifier.shared.approvalAsk(id: id, title: title, body: body)
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
        case .upsert(let s):
            let prev = sessionsByID[s.id]
            sessionsByID[s.id] = s
            let awaiting = s.state == .awaitingPermission || s.state == .awaitingInput
            if awaiting, prev?.state != s.state {
                maybeNotifyApproval(
                    id: s.id,
                    title: s.state == .awaitingPermission ? "Claude needs permission" : "Claude is asking",
                    body: s.headline)
            } else if !awaiting {
                clearApprovalNotification(s.id)
            }
        case .remove(let id):
            sessionsByID.removeValue(forKey: id)
            clearApprovalNotification(id)

        case .permAdd(let r):
            permissionsByID[r.id] = r
            let where_ = r.cwd.map { " · \(($0 as NSString).lastPathComponent)" } ?? ""
            maybeNotifyApproval(id: r.id, title: "Approval needed", body: "\(r.toolName)\(where_)")
        case .permRemove(let id):
            permissionsByID.removeValue(forKey: id)
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

    func dropPermission(_ id: String) { permissionsByID.removeValue(forKey: id) }
}
