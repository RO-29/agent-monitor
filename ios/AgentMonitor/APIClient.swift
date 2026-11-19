import Foundation

struct APIError: LocalizedError {
    let message: String
    var status: Int = 0
    var errorDescription: String? { message }
}

/// Thin async wrapper over the daemon's REST endpoints. Reads Config.baseURL on
/// every call so a Settings change takes effect without a restart.
struct APIClient {
    static let shared = APIClient()

    var session: URLSession {
        let cfg = URLSessionConfiguration.default
        cfg.timeoutIntervalForRequest = 15
        cfg.waitsForConnectivity = false
        return URLSession(configuration: cfg)
    }

    func url(_ path: String) throws -> URL {
        guard let base = Config.baseURL, let u = URL(string: base.absoluteString + path) else {
            throw APIError(message: "Bad server URL")
        }
        return u
    }

    // MARK: GET helpers

    /// Attaches the configured password as a Bearer token. Applied to every
    /// request so remote (tailnet/LAN) access to a password-protected daemon
    /// works; harmless when the daemon has no password set.
    static func authorize(_ req: inout URLRequest) {
        if let h = Config.authHeader {
            req.setValue(h, forHTTPHeaderField: "Authorization")
        }
    }

    func get<T: Decodable>(_ path: String, as: T.Type) async throws -> T {
        var req = URLRequest(url: try url(path))
        Self.authorize(&req)
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try JSONDecoder().decode(T.self, from: data)
    }

    @discardableResult
    func post(_ path: String, body: [String: Any]? = nil, timeout: TimeInterval? = nil) async throws -> Data {
        var req = URLRequest(url: try url(path))
        if let timeout { req.timeoutInterval = timeout }
        req.httpMethod = "POST"
        Self.authorize(&req)
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return data
    }

    static func check(_ resp: URLResponse, _ data: Data) throws {
        guard let http = resp as? HTTPURLResponse else { return }
        guard (200..<300).contains(http.statusCode) else {
            if let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let msg = obj["error"] as? String {
                throw APIError(message: msg, status: http.statusCode)
            }
            throw APIError(message: "HTTP \(http.statusCode)", status: http.statusCode)
        }
    }

    // MARK: Snapshots (hydrate on connect)

    // The daemon wraps list snapshots in an object keyed by kind (and omits the
    // key entirely when empty), so decode a wrapper with an optional array.
    private struct SessionsWrap: Decodable { var sessions: [Session]? }
    private struct PermsWrap: Decodable { var requests: [PermissionRequest]? }
    private struct TalksWrap: Decodable { var talks: [Talk]? }
    private struct PanesWrap: Decodable { var panes: [PaneRegistration]? }

    /// slim=1: ended or quiet sessions come without their event history (the full list is several MB).
    func sessions() async throws -> [Session] { try await get("/api/sessions?slim=1", as: SessionsWrap.self).sessions ?? [] }
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
    /// Answers the open terminal prompt; the daemon reads the screen and refuses a changed prompt.
    func answerPrompt(_ id: String, yes: Bool, key: String?) async throws {
        var body: [String: Any] = ["answer": yes ? "yes" : "no"]
        if let key { body["key"] = key }
        try await post("/api/session/prompt/\(enc(id))", body: body)
    }
    /// Clears a prompt card answered elsewhere; a prompt still on a watched screen comes back.
    func dismissPrompt(_ id: String) async throws {
        try await post("/api/session/prompt/\(enc(id))", body: ["answer": "dismiss"])
    }
    func keys(_ id: String, _ keys: [String]) async throws {
        try await post("/api/session/keys/\(enc(id))", body: ["keys": keys])
    }
    struct SkillItem: Decodable { let name: String; let description: String?; let insert: String? }
    struct SkillList: Decodable { let skills: [SkillItem] }
    /// Skills and custom commands the agent loads in a folder; insert is the text to type ("/name").
    func skills(agent: String, cwd: String) async throws -> [SkillItem] {
        let q = "agent=\(enc(agent))&cwd=\(enc(cwd))"
        return try await get("/api/orch/skills?\(q)", as: SkillList.self).skills
    }
    /// Stops the turn and exits the agent; false when it still runs after the daemon's wait.
    func endSession(_ id: String) async throws -> Bool {
        let d = try await post("/api/session/end/\(enc(id))", timeout: 30)
        return ((try? JSONSerialization.jsonObject(with: d)) as? [String: Any])?["ended"] as? Bool ?? false
    }
    /// Resumes an ended session in a new terminal; prompt is its first message.
    func resumeSession(_ id: String, prompt: String = "") async throws {
        _ = try await post("/api/session/resume/\(enc(id))", body: ["prompt": prompt])
    }
    func cancel(_ id: String) async throws {
        try await post("/api/session/cancel/\(enc(id))")
    }

    // MARK: Approve / reply a native TUI prompt over the pane bridge
    //
    // Interactive agents can't be driven without tmux, so answers to Claude's
    // terminal prompts are sent as the keystrokes a human would press in the
    // pane: option digits / Enter (Yes) / Escape (No) via keys(), and free
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

    // MARK: Focus: "open this session's terminal on the Mac"

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

    func place(_ sessionID: String) async throws -> SessionPlace {
        try await get("/api/session/\(enc(sessionID))/place", as: SessionPlace.self)
    }

    func enc(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? s
    }
}
