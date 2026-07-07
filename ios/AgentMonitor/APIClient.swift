import Foundation

struct APIError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

/// Thin async wrapper over the daemon's REST endpoints. Reads Config.baseURL on
/// every call so a Settings change takes effect without a restart.
struct APIClient {
    static let shared = APIClient()

    private var session: URLSession {
        let cfg = URLSessionConfiguration.default
        cfg.timeoutIntervalForRequest = 15
        cfg.waitsForConnectivity = false
        return URLSession(configuration: cfg)
    }

    private func url(_ path: String) throws -> URL {
        guard let base = Config.baseURL, let u = URL(string: base.absoluteString + path) else {
            throw APIError(message: "Bad server URL")
        }
        return u
    }

    // MARK: GET helpers

    private func get<T: Decodable>(_ path: String, as: T.Type) async throws -> T {
        let (data, resp) = try await session.data(from: try url(path))
        try Self.check(resp, data)
        return try JSONDecoder().decode(T.self, from: data)
    }

    @discardableResult
    private func post(_ path: String, body: [String: Any]? = nil) async throws -> Data {
        var req = URLRequest(url: try url(path))
        req.httpMethod = "POST"
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return data
    }

    private static func check(_ resp: URLResponse, _ data: Data) throws {
        guard let http = resp as? HTTPURLResponse else { return }
        guard (200..<300).contains(http.statusCode) else {
            if let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let msg = obj["error"] as? String {
                throw APIError(message: msg)
            }
            throw APIError(message: "HTTP \(http.statusCode)")
        }
    }

    // MARK: Snapshots (hydrate on connect)

    // The daemon wraps list snapshots in an object keyed by kind (and omits the
    // key entirely when empty), so decode a wrapper with an optional array.
    private struct SessionsWrap: Decodable { var sessions: [Session]? }
    private struct PermsWrap: Decodable { var requests: [PermissionRequest]? }
    private struct TalksWrap: Decodable { var talks: [Talk]? }
    private struct PanesWrap: Decodable { var panes: [PaneRegistration]? }

    func sessions() async throws -> [Session] { try await get("/api/sessions", as: SessionsWrap.self).sessions ?? [] }
    func permissions() async throws -> [PermissionRequest] { try await get("/api/permissions", as: PermsWrap.self).requests ?? [] }
    func talks() async throws -> [Talk] { try await get("/api/talks", as: TalksWrap.self).talks ?? [] }
    func panes() async throws -> [PaneRegistration] { try await get("/api/panes", as: PanesWrap.self).panes ?? [] }
    func detail(_ id: String) async throws -> SessionDetail {
        // The daemon serves the transcript at /api/session/<id>/full.
        try await get("/api/session/\(enc(id))/full", as: SessionDetail.self)
    }

    // MARK: Session linkage (chains + spawn edges)

    struct ChainsResponse: Decodable {
        var chains: [Chain]?
        var chainOf: [String: String]?          // sessionId → chain key
        var spawnParent: [String: String]?      // childId → parentId
        var spawnChildren: [String: [SpawnChild]]?  // parentId → children
    }
    func chains() async throws -> ChainsResponse {
        try await get("/api/chains", as: ChainsResponse.self)
    }
    func health() async throws -> Bool {
        _ = try await get("/api/health", as: JSONValue.self)
        return true
    }

    // MARK: Permission responses

    func respondPermission(_ id: String, allow: Bool, reason: String? = nil) async throws {
        var body: [String: Any] = ["behavior": allow ? "allow" : "deny"]
        if let reason, !reason.isEmpty { body["reason"] = reason }
        try await post("/api/permission/\(enc(id))/respond", body: body)
    }

    // MARK: Pane bridge

    func send(_ id: String, text: String, enter: Bool) async throws {
        try await post("/api/session/send/\(enc(id))", body: ["text": text, "enter": enter])
    }
    func keys(_ id: String, _ keys: [String]) async throws {
        try await post("/api/session/keys/\(enc(id))", body: ["keys": keys])
    }
    func cancel(_ id: String) async throws {
        try await post("/api/session/cancel/\(enc(id))")
    }

    // MARK: Approve / reply a native TUI prompt over the pane bridge
    //
    // Interactive agents can't be driven without tmux, so answers to Claude's
    // terminal prompts are sent as the keystrokes a human would press in the
    // pane — option digits / Enter (Yes) / Escape (No) via keys(), and free
    // text via replyViaPane (answers an awaiting-input prompt).
    func replyViaPane(_ id: String, text: String) async throws {
        try await send(id, text: text, enter: true)
    }

    struct PaneView: Decodable {
        var ok: Bool
        var paneId: String?
        var content: String?
        var command: String?
        var error: String?
    }
    // plain=1 → the daemon strips ANSI escapes (iOS shows raw text, not colour).
    func paneView(_ id: String, lines: Int = 40) async throws -> PaneView {
        try await get("/api/session/pane-view/\(enc(id))?lines=\(lines)&plain=1", as: PaneView.self)
    }

    // MARK: Focus — "open this session's terminal on the Mac"

    struct FocusResult: Decodable {
        var ok: Bool
        var terminal: String?
        var sessionName: String?
        var paneId: String?
        var reason: String?
    }
    /// Asks the daemon to select the session's tmux pane and raise its hosting
    /// terminal app on the Mac. Works over the tailnet from the phone.
    func focus(_ id: String) async throws -> FocusResult {
        let data = try await post("/api/session/focus/\(enc(id))")
        return try JSONDecoder().decode(FocusResult.self, from: data)
    }

    // MARK: Panes registry

    func renamePane(_ agentId: String, alias: String) async throws {
        try await post("/api/pane/name/\(enc(agentId))", body: ["alias": alias])
    }

    // MARK: Talks

    func requestTalk(to recipient: String, message: String) async throws {
        try await post("/api/talk/request", body: ["toAgent": recipient, "message": message])
    }
    func respondTalk(_ id: String, allow: Bool, reason: String? = nil) async throws {
        var body: [String: Any] = ["behavior": allow ? "allow" : "deny"]
        if let reason, !reason.isEmpty { body["reason"] = reason }
        try await post("/api/talk/\(enc(id))/respond", body: body)
    }

    private func enc(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? s
    }
}
