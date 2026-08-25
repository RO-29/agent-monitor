import SwiftUI

// Swift port of the web dashboard's trace-view model (web/index.html
// buildStream). Turns a SessionDetail into: grouped turns (prose + nested tool
// waterfall), a flat atomic list, facet counts, and ribbon events — with the
// "→ Bash" dispatch stubs suppressed so the stream reads like a transcript.

enum EventType: String { case assistant, tool, user, error }

struct RibbonEvent { var ts: Int64; var type: EventType }

struct RibbonBin: Identifiable {
    let id: Int
    var assistant = 0, tool = 0, user = 0, error = 0
    var total: Int { assistant + tool + user + error }
}

struct TraceTool: Identifiable, Hashable {
    var id: String
    var name: String
    var ts: Int64
    var args: String
    var result: String
    var isSubagent: Bool
    var isError: Bool
    var isSub: Bool          // rendered as a subagent row (richer prompt/result)
    var running: Bool { !isSub && result.isEmpty }
}

struct TraceTurn: Identifiable, Hashable {
    var id: String
    var ts: Int64
    var role: String          // "user" | "assistant"
    var text: String
    var model: String?
    var tools: [TraceTool]
    var hasErr: Bool
}

struct ToolCount: Identifiable, Hashable { var id: String { name }; var name: String; var count: Int }

struct TraceCounts { var all = 0, prose = 0, tools = 0, errors = 0, user = 0 }

struct TraceStream {
    var turns: [TraceTurn] = []
    var flatTools: [TraceTool] = []
    var events: [RibbonEvent] = []
    var counts = TraceCounts()
    var toolCounts: [ToolCount] = []
    var keptMsgs: [DetailMsg] = []
    var tsMin: Int64 = 0
    var tsMax: Int64 = 0
}

enum Trace {
    // Conservative error detection on a tool result (mirror of web ERR_RE).
    static func isError(_ result: String) -> Bool {
        guard !result.isEmpty else { return false }
        let head = String(result.prefix(400)).lowercased()
        if head.range(of: #"exit code [1-9]"#, options: .regularExpression) != nil { return true }
        let needles = ["error:", "error ", "\nerror", "traceback (most recent", "fatal:",
                       "command failed", "no such file", "permission denied", "npm err!",
                       "cannot find", "is not recognized", "segmentation fault", "panic:"]
        return needles.contains { head.contains($0) }
    }

    static func isStub(role: String, text: String) -> Bool {
        guard role == "assistant" else { return false }
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if t.isEmpty || t == "(no text)" { return true }
        return t.range(of: #"^→\s*[\w.\-]+$"#, options: .regularExpression) != nil
    }

    static func build(_ detail: SessionDetail?) -> TraceStream {
        guard let detail else { return TraceStream() }
        let msgs = detail.messages ?? []
        let tools = detail.toolCalls ?? []
        let subs = detail.subagents ?? []

        let kept = msgs.filter { $0.role == "user" || !isStub(role: $0.role, text: $0.text) }
                       .sorted { $0.ts < $1.ts }
        var turns = kept.map {
            TraceTurn(id: "msg:\($0.ts):\($0.role)", ts: $0.ts, role: $0.role,
                      text: $0.text, model: $0.model, tools: [], hasErr: false)
        }
        let asstIdx = turns.indices.filter { turns[$0].role == "assistant" }
        let subTs = Set(subs.map { $0.ts })
        var orphans: [TraceTool] = []

        var ai = 0
        for t in tools.sorted(by: { $0.ts < $1.ts }) {
            if t.isSubagent && subTs.contains(t.ts) { continue }
            let tt = TraceTool(id: t.id, name: t.name, ts: t.ts, args: t.args, result: t.result,
                               isSubagent: t.isSubagent, isError: isError(t.result), isSub: false)
            while ai + 1 < asstIdx.count && turns[asstIdx[ai + 1]].ts <= t.ts { ai += 1 }
            if !asstIdx.isEmpty && turns[asstIdx[ai]].ts <= t.ts {
                turns[asstIdx[ai]].tools.append(tt)
                if tt.isError { turns[asstIdx[ai]].hasErr = true }
            } else { orphans.append(tt) }
        }
        var si = 0
        for s in subs.sorted(by: { $0.ts < $1.ts }) {
            let row = TraceTool(id: s.id, name: s.subagentType.isEmpty ? "sub" : s.subagentType,
                                ts: s.ts, args: s.prompt, result: s.result,
                                isSubagent: true, isError: false, isSub: true)
            while si + 1 < asstIdx.count && turns[asstIdx[si + 1]].ts <= s.ts { si += 1 }
            if !asstIdx.isEmpty && turns[asstIdx[si]].ts <= s.ts { turns[asstIdx[si]].tools.append(row) }
            else { orphans.append(row) }
        }
        if !orphans.isEmpty {
            let o = orphans.sorted { $0.ts < $1.ts }
            turns.insert(TraceTurn(id: "orphans", ts: o.first!.ts, role: "assistant",
                                   text: "", model: nil, tools: o, hasErr: o.contains { $0.isError }), at: 0)
        }
        for i in turns.indices { turns[i].tools.sort { $0.ts < $1.ts } }

        let userCount = kept.filter { $0.role == "user" }.count
        let proseCount = kept.filter { $0.role == "assistant" }.count
        var flat: [TraceTool] = []
        for t in turns { flat += t.tools }
        let errCount = flat.filter { $0.isError }.count
        var tc: [String: Int] = [:]
        for t in flat { tc[t.name, default: 0] += 1 }
        let toolCounts = tc.sorted { $0.value > $1.value }.map { ToolCount(name: $0.key, count: $0.value) }

        var events: [RibbonEvent] = []
        for m in kept { events.append(.init(ts: m.ts, type: m.role == "user" ? .user : .assistant)) }
        for t in flat { events.append(.init(ts: t.ts, type: t.isError ? .error : .tool)) }
        let tsList = events.map { $0.ts }

        return TraceStream(
            turns: turns, flatTools: flat, events: events,
            counts: TraceCounts(all: userCount + proseCount + flat.count, prose: proseCount,
                                tools: flat.count, errors: errCount, user: userCount),
            toolCounts: toolCounts, keptMsgs: kept,
            tsMin: tsList.min() ?? 0, tsMax: tsList.max() ?? 0)
    }

    // Bin ribbon events into `n` stacked buckets over [tsMin, tsMax].
    static func bins(_ stream: TraceStream, n: Int) -> [RibbonBin] {
        var out = (0..<n).map { RibbonBin(id: $0) }
        let span = max(1, stream.tsMax - stream.tsMin)
        for e in stream.events {
            var i = Int(Double(e.ts - stream.tsMin) / Double(span) * Double(n))
            i = min(n - 1, max(0, i))
            switch e.type {
            case .assistant: out[i].assistant += 1
            case .tool: out[i].tool += 1
            case .user: out[i].user += 1
            case .error: out[i].error += 1
            }
        }
        return out
    }
}

// MARK: - formatting

func fmtBytes(_ n: Int) -> String {
    if n < 1000 { return "\(n)b" }
    if n < 1_000_000 { return String(format: n < 10000 ? "%.1fk" : "%.0fk", Double(n) / 1000) }
    return String(format: "%.1fM", Double(n) / 1_000_000)
}

func fmtDurationMs(_ ms: Int64) -> String {
    guard ms > 1000 else { return "—" }
    let s = ms / 1000
    if s < 60 { return "\(s)s" }
    if s < 3600 { return "\(s / 60)m" }
    if s < 86400 { return "\(s / 3600)h \((s % 3600) / 60)m" }
    return "\(s / 86400)d"
}

// Per-tool accent, matching the web palette.
func toolColor(_ name: String) -> Color {
    switch name {
    case "Bash": return .cyan
    case "Read": return .blue
    case "Edit", "Write", "MultiEdit", "NotebookEdit": return .orange
    case "Grep", "Glob", "ToolSearch": return .green
    case "Task", "Agent", "SendMessage", "AskUserQuestion": return .purple
    case "WebFetch", "WebSearch": return .yellow
    default: return .cyan
    }
}

// One-line preview of a tool call's arguments, tuned per tool (mirror of web).
func toolPreview(name: String, args: String) -> String {
    guard let data = args.data(using: .utf8),
          let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
        return args
    }
    func s(_ k: String) -> String? { obj[k] as? String }
    switch name {
    case "Bash": if let c = s("command") { return c }
    case "Edit", "Write", "MultiEdit", "NotebookEdit": if let f = s("file_path") { return f }
    case "Read": if let f = s("file_path") { return f + ((obj["offset"] as? Int).map { " @\($0)" } ?? "") }
    case "Grep": if let p = s("pattern") { return "\"\(p)\"" + (s("path").map { " in \($0)" } ?? "") }
    case "Glob": if let p = s("pattern") { return p }
    case "ToolSearch", "WebSearch": if let q = s("query") { return q }
    case "WebFetch": if let u = s("url") { return u }
    case "Task": if let d = s("description") { return "[\(s("subagent_type") ?? "general")] \(d)" }
    case "TaskCreate": if let s = s("subject") { return s }
    case "TaskUpdate": if let t = obj["taskId"] { return "task #\(t) → \(s("status") ?? "update")" }
    default: break
    }
    if obj.isEmpty { return "(no input)" }
    let flat = args.replacingOccurrences(of: "\n", with: " ")
    return flat.count > 140 ? String(flat.prefix(140)) + "…" : flat
}

func firstLine(_ s: String) -> String {
    s.split(separator: "\n", omittingEmptySubsequences: false).first.map(String.init)?
        .trimmingCharacters(in: .whitespaces) ?? s
}
