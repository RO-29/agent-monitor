import Foundation

// Newer daemon routes: hook approvals, alerts, threads, watched items, the Claude Code queue
// and terminal questions. Every request goes through get/post, which add Config.authHeader.
extension APIClient {
    @discardableResult
    func postJSON<B: Encodable>(_ path: String, _ body: B) async throws -> Data {
        var req = URLRequest(url: try url(path))
        req.httpMethod = "POST"
        Self.authorize(&req)
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(body)
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return data
    }

    /// A GET request for a long-lived stream (SSE), with the same auth header.
    func streamRequest(_ path: String) throws -> URLRequest {
        var req = URLRequest(url: try url(path))
        Self.authorize(&req)
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        return req
    }

    // MARK: Hook approvals

    func approvals() async throws -> ApprovalsResponse {
        try await get("/api/approvals", as: ApprovalsResponse.self)
    }

    func decide(_ id: String, _ body: DecideBody) async throws -> Approval? {
        let data = try await postJSON("/api/approvals/\(enc(id))/decide", body)
        return try? JSONDecoder().decode(Approval.self, from: data)
    }

    // MARK: Alerts

    func alerts(limit: Int = 200) async throws -> AlertsResponse {
        try await get("/api/notify?limit=\(limit)", as: AlertsResponse.self)
    }

    func markAlertsRead(ids: [String]? = nil, all: Bool = false) async throws {
        var body: [String: Any] = [:]
        if let ids { body["ids"] = ids }
        if all { body["all"] = true }
        try await post("/api/notify/read", body: body)
    }

    // MARK: Threads


    func watchedItems(_ sessionID: String, after: Int64? = nil, before: Int64? = nil) async throws -> WatchedItems {
        var q = ""
        if let after { q = "?after=\(after)" } else if let before { q = "?before=\(before)" }
        return try await get("/api/session/\(enc(sessionID))/items\(q)", as: WatchedItems.self)
    }

    func watchedPane(_ sessionID: String) async throws -> WatchedPane {
        try await get("/api/session/\(enc(sessionID))/pane", as: WatchedPane.self)
    }

    // MARK: Claude Code queue

    /// Types the message into the CLI; answers whether the busy CLI queued it.
    func cliSend(_ sessionID: String, text: String) async throws -> Bool {
        struct R: Decodable { var queued: Bool? }
        let data = try await post("/api/session/\(enc(sessionID))/cli-queue/send", body: ["text": text])
        return (try? JSONDecoder().decode(R.self, from: data))?.queued ?? false
    }

    func cliQueue(_ sessionID: String) async throws -> CLIQueueView {
        try await get("/api/session/\(enc(sessionID))/cli-queue", as: CLIQueueView.self)
    }

    /// action is send-now or remove.
    func cliQueueAction(_ sessionID: String, action: String, itemID: String? = nil) async throws {
        var body: [String: Any] = [:]
        if let itemID { body["id"] = itemID }
        try await post("/api/session/\(enc(sessionID))/cli-queue/\(action)", body: body)
    }

    // MARK: Needs you

    func needsYou() async throws -> NeedsYouResponse {
        try await get("/api/needs-you", as: NeedsYouResponse.self)
    }

    // MARK: Terminal question

    func termQuestion(_ sessionID: String) async throws -> TermQuestion {
        try await get("/api/session/\(enc(sessionID))/question", as: TermQuestion.self)
    }

    func answerTerm(_ sessionID: String, _ body: TermAnswerBody) async throws {
        try await postJSON("/api/session/\(enc(sessionID))/answer", body)
    }

    // MARK: Subagents and usage

    func children(_ sessionID: String) async throws -> SessionChildren {
        try await get("/api/session/\(enc(sessionID))/children", as: SessionChildren.self)
    }

    func usageLimits() async throws -> UsageLimits {
        try await get("/api/usage/limits", as: UsageLimits.self)
    }

    func cwdGit(_ cwd: String) async throws -> OGit {
        var c = URLComponents()
        c.queryItems = [URLQueryItem(name: "cwd", value: cwd)]
        let q = (c.percentEncodedQuery ?? "").replacingOccurrences(of: "+", with: "%2B")
        return try await get("/api/orch/git?" + q, as: OGit.self)
    }

    // MARK: Pairing

    /// Trades a single-use pair code for the device token, which the daemon returns only as the am_device cookie.
    static func redeemPairCode(_ code: String, base: URL) async throws -> String {
        guard let u = URL(string: base.absoluteString + "/api/devices/session") else { throw APIError(message: "Bad server URL") }
        var req = URLRequest(url: u)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: ["code": code])
        let cfg = URLSessionConfiguration.ephemeral
        cfg.httpCookieStorage = nil
        cfg.httpShouldSetCookies = false
        cfg.timeoutIntervalForRequest = 15
        let (data, resp) = try await URLSession(configuration: cfg).data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw APIError(message: "No HTTP response") }
        switch http.statusCode {
        case 200..<300: break
        case 401: throw APIError(message: "The pair code is used, expired (5 minutes) or not valid, or the device was revoked. Add the device again on the Mac and paste the new link.", status: 401)
        case 404: throw APIError(message: "This daemon does not support device pairing yet.", status: 404)
        default: try check(resp, data)
        }
        var fields: [String: String] = [:]
        for (k, v) in http.allHeaderFields { if let k = k as? String, let v = v as? String { fields[k] = v } }
        let cookies = HTTPCookie.cookies(withResponseHeaderFields: fields, for: u)
        guard let tok = cookies.first(where: { $0.name == "am_device" })?.value, tok.hasPrefix("amd_") else {
            throw APIError(message: "The daemon accepted the code but sent no device token.")
        }
        return tok
    }
}
