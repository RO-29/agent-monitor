import Foundation

// Orchestrator routes: agents, new threads (driven or in a terminal), driven chat, uploads and subagent transcripts.
extension APIClient {
    func orchAgents() async throws -> [OAgentInfo] {
        var req = URLRequest(url: try url("/api/orch/agents"))
        authorize(&req)
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        struct Wrap: Decodable { var agents: [Lossy<OAgentInfo>]? }
        if let arr = try? JSONDecoder().decode([Lossy<OAgentInfo>].self, from: data) { return arr.compactMap(\.value) }
        return (try JSONDecoder().decode(Wrap.self, from: data).agents ?? []).compactMap(\.value)
    }

    /// POST /api/orch/threads; returns the new driven thread id.
    func createThread(_ body: [String: Any]) async throws -> String {
        struct R: Decodable { struct T: Decodable { var id: String }; var thread: T }
        let data = try await post("/api/orch/threads", body: body)
        return try JSONDecoder().decode(R.self, from: data).thread.id
    }

    func startTerminal(_ body: [String: Any]) async throws -> OTerminalStart {
        let data = try await post("/api/orch/terminal", body: body)
        return try JSONDecoder().decode(OTerminalStart.self, from: data)
    }

    func hostInfo(_ hostID: String) async throws -> OHostInfo {
        try await get("/api/hosts/\(enc(hostID))", as: OHostInfo.self)
    }

    /// Picks an option of the prompt the host shows, for example "trust" or "cancel".
    func answerHost(_ hostID: String, option: String) async throws {
        _ = try await post("/api/hosts/\(enc(hostID))/answer", body: ["option": option])
    }

    func drivenDetail(_ id: String) async throws -> ODrivenDetail {
        try await get("/api/orch/threads/\(enc(id))", as: ODrivenDetail.self)
    }

    /// Server-sent events after `after`, for every driven thread; ends when the connection drops.
    func orchStream(after: Int64) -> AsyncThrowingStream<OEvent, Error> {
        AsyncThrowingStream { cont in
            let task = Task {
                do {
                    var req = URLRequest(url: try url("/api/orch/stream?after=\(after)"))
                    req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    authorize(&req)
                    let cfg = URLSessionConfiguration.default
                    // The daemon sends a heartbeat every 15 s, so 45 s of silence means a dead link.
                    cfg.timeoutIntervalForRequest = 45
                    cfg.timeoutIntervalForResource = 24 * 3600
                    let s = URLSession(configuration: cfg)
                    defer { s.invalidateAndCancel() }
                    let (bytes, resp) = try await s.bytes(for: req)
                    if let http = resp as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
                        throw APIError(message: "HTTP \(http.statusCode)", status: http.statusCode)
                    }
                    for try await line in bytes.lines {
                        guard line.hasPrefix("data:") else { continue }
                        let json = line.dropFirst(5).drop { $0 == " " }
                        if let ev = try? JSONDecoder().decode(OEvent.self, from: Data(json.utf8)) { cont.yield(ev) }
                    }
                    cont.finish()
                } catch {
                    cont.finish(throwing: error)
                }
            }
            cont.onTermination = { _ in task.cancel() }
        }
    }

    func drivenSend(_ id: String, text: String, attachments: [String]) async throws {
        var body: [String: Any] = ["text": text]
        if !attachments.isEmpty { body["attachments"] = attachments }
        try await post("/api/orch/threads/\(enc(id))/messages", body: body)
    }

    /// Settles a driven thread: it leaves "needs you" until it changes again.
    func drivenSettle(_ id: String) async throws {
        try await post("/api/orch/threads/\(enc(id))/settle", body: ["settled": true])
    }

    /// Types the message into the CLI with optional upload ids; answers whether the busy CLI queued it.
    func cliSend(_ sessionID: String, text: String, attachments: [String]) async throws -> Bool {
        struct R: Decodable { var queued: Bool? }
        var body: [String: Any] = ["text": text]
        if !attachments.isEmpty { body["attachments"] = attachments }
        let data = try await post("/api/session/\(enc(sessionID))/cli-queue/send", body: body)
        return (try? JSONDecoder().decode(R.self, from: data))?.queued ?? false
    }

    func subagentItems(_ sessionID: String, agentID: String, after: Int64?) async throws -> WatchedItems {
        let q = after.map { "?after=\($0)" } ?? ""
        return try await get("/api/session/\(enc(sessionID))/children/\(enc(agentID))/items\(q)", as: WatchedItems.self)
    }

    /// all=1 keeps child threads, which the daemon drops otherwise; the home list filters them out.
    func threadsCompact() async throws -> ThreadsResponse {
        try await get("/api/v3/threads?compact=1&all=1", as: ThreadsResponse.self)
    }

    /// Multipart POST /api/orch/uploads with progress; threadId only for a driven thread (ot_...).
    func upload(data: Data, name: String, mime: String, threadID: String?, progress: @escaping @Sendable (Double) -> Void) async throws -> OAttachment {
        var req = URLRequest(url: try url("/api/orch/uploads"))
        req.httpMethod = "POST"
        req.timeoutInterval = 120
        authorize(&req)
        let boundary = "am-\(UUID().uuidString)"
        req.setValue("multipart/form-data; boundary=\(boundary)", forHTTPHeaderField: "Content-Type")
        var body = Data()
        func line(_ s: String) { body.append(Data((s + "\r\n").utf8)) }
        if let t = threadID, t.hasPrefix("ot_") {
            line("--\(boundary)")
            line("Content-Disposition: form-data; name=\"threadId\"")
            line("")
            line(t)
        }
        let safe = name.replacingOccurrences(of: "\"", with: "'").replacingOccurrences(of: "\r", with: "").replacingOccurrences(of: "\n", with: "")
        line("--\(boundary)")
        line("Content-Disposition: form-data; name=\"file\"; filename=\"\(safe)\"")
        line("Content-Type: \(mime)")
        line("")
        body.append(data)
        line("")
        line("--\(boundary)--")
        let (resp, http) = try await session.upload(for: req, from: body, delegate: UploadProgress(progress))
        do {
            try Self.check(http, resp)
        } catch let e as APIError {
            throw Self.uploadError(e, mime: mime)
        }
        struct Wrap: Decodable { var upload: OAttachment? }
        if let w = try? JSONDecoder().decode(Wrap.self, from: resp), let u = w.upload { return u }
        return try JSONDecoder().decode(OAttachment.self, from: resp)
    }

    /// Older daemons take only images up to 10 MB; say so plainly.
    static func uploadError(_ e: APIError, mime: String) -> APIError {
        switch e.status {
        case 404, 405:
            return APIError(message: "This daemon cannot take attachments. Update agent-monitor on the Mac.", status: e.status)
        case 415:
            return APIError(message: "This daemon accepts only PNG, JPEG, WebP and GIF images. Update agent-monitor on the Mac to send PDFs and text files.", status: e.status)
        case 400 where !mime.hasPrefix("image/"):
            return APIError(message: "This daemon accepts only PNG, JPEG, WebP and GIF images. Update agent-monitor on the Mac to send PDFs and text files.", status: e.status)
        case 413:
            return APIError(message: "The file is too large for this daemon. \(e.message)", status: e.status)
        default:
            return e
        }
    }
}

private final class UploadProgress: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    let onProgress: @Sendable (Double) -> Void
    init(_ f: @escaping @Sendable (Double) -> Void) { onProgress = f }
    func urlSession(_ session: URLSession, task: URLSessionTask, didSendBodyData bytesSent: Int64, totalBytesSent: Int64, totalBytesExpectedToSend: Int64) {
        guard totalBytesExpectedToSend > 0 else { return }
        onProgress(Double(totalBytesSent) / Double(totalBytesExpectedToSend))
    }
}
