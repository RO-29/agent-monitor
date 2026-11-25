import Foundation

// Codable mirrors of the newer daemon payloads: approvals.go, notify.go, ops.go (ThreadV3),
// orch_store.go (OItem/ORun), watched_items.go, cli_queue.go and term_question.go.

/// Decodes an array element by element and skips the ones that do not decode.
struct Lossy<T: Decodable>: Decodable {
    var value: T?
    init(from decoder: Decoder) throws { value = try? T(from: decoder) }
}

// MARK: - Hook approvals

struct ApprovalOption: Hashable {
    var label: String
    var description: String?
}

struct ApprovalQuestion: Hashable {
    var question: String
    var header: String?
    var multiSelect: Bool
    var options: [ApprovalOption]
}

/// An ACP agent's permission option (allow_once, allow_always, reject_once, reject_always).
struct PermOption: Codable, Hashable, Identifiable {
    var optionId: String
    var name: String
    var kind: String
    var id: String { optionId }
    var allows: Bool { kind.hasPrefix("allow") }
}

func humanToolName(_ raw: String) -> String {
    guard raw.hasPrefix("mcp__") else { return raw }
    let parts = raw.dropFirst(5).components(separatedBy: "__").filter { !$0.isEmpty }
    guard let server = parts.first else { return raw }
    let tool = parts.dropFirst().joined(separator: " ").replacingOccurrences(of: "_", with: " ")
    return tool.isEmpty ? server : "\(server): \(tool)"
}

struct Approval: Codable, Identifiable, Hashable {
    var id: String
    var agent: String
    var sessionId: String
    var cwd: String
    var kind: String            // permission | question | plan
    var toolName: String
    var input: [String: JSONValue]?
    var summary: String
    var risk: String
    var riskReason: String
    var suggestions: [JSONValue]?
    var questions: [JSONValue]?
    var plan: String?
    var createdAt: Int64
    var holdUntil: Int64
    var state: String
    var decidedAt: Int64?
    var decidedBy: String?
    var message: String?
    var alwaysOk: Bool?
    var drivenId: String?
    var options: [PermOption]?
    var source: String?
    var sentAt: Int64?
    var choice: String?

    var isPending: Bool { state == "pending" }
    var project: String { (cwd as NSString).lastPathComponent }
    var canAlways: Bool { kind == "permission" && (alwaysOk ?? (risk != "high")) }
    /// One risk rule for every surface: allowing a high-risk permission asks to confirm first.
    var highRisk: Bool { kind == "permission" && risk == "high" }
    func needsConfirm(_ b: DecideBody) -> Bool { highRisk && b.behavior == "allow" }
    var terminalShown: Bool { source == "terminal-shown" }
    /// A card decision was typed into the terminal and waits for the terminal to confirm it.
    var sentToTerminal: Bool { terminalShown && (sentAt ?? 0) > 0 }

    /// Tool name for people: `mcp__ch-us-staging__run_query` reads "ch-us-staging: run query".
    var displayTool: String { humanToolName(toolName) }

    /// Second line of an approval card heading; the same verbs as the web card.
    var headline: String {
        let who = agentName(agent)
        switch kind {
        case "question": return "\(who) asks"
        case "plan": return "\(who) has a plan ready"
        default:
            if ["Bash", "shell", "exec_command", "local_shell", "unified_exec"].contains(toolName) { return "\(who) wants to run" }
            if ["Edit", "MultiEdit", "Write", "NotebookEdit"].contains(toolName) { return "\(who) wants to edit a file" }
            return "\(who) wants to use \(displayTool)"
        }
    }

    var command: String? {
        guard let v = input?["command"] ?? input?["cmd"] else { return nil }
        let s = v.display
        return s.isEmpty ? nil : s
    }

    var planText: String {
        if let p = plan, !p.isEmpty { return p }
        if case .string(let s)? = input?["plan"] { return s }
        return summary
    }

    var parsedQuestions: [ApprovalQuestion] {
        (questions ?? []).compactMap { q in
            guard case .object(let o) = q, case .string(let text)? = o["question"] else { return nil }
            var header: String?
            if case .string(let h)? = o["header"] { header = h }
            var multi = false
            if case .bool(let b)? = o["multiSelect"] { multi = b }
            var opts: [ApprovalOption] = []
            if case .array(let arr)? = o["options"] {
                for x in arr {
                    guard case .object(let oo) = x, case .string(let label)? = oo["label"] else { continue }
                    var d: String?
                    if case .string(let s)? = oo["description"] { d = s }
                    opts.append(ApprovalOption(label: label, description: d))
                }
            }
            return ApprovalQuestion(question: text, header: header, multiSelect: multi, options: opts)
        }
    }

    var inputPairs: [(String, String)] {
        (input ?? [:]).sorted { $0.key < $1.key }.map { ($0.key, $0.value.display) }
    }
}

struct ApprovalsResponse: Decodable {
    var pending: [Approval]?

    private enum K: String, CodingKey { case pending }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: K.self)
        pending = try c.decodeIfPresent([Lossy<Approval>].self, forKey: .pending)?.compactMap(\.value)
    }
}

/// POST /api/approvals/{id}/decide body (approvalDecision in approvals.go).
struct DecideBody: Encodable {
    var behavior: String
    var message: String?
    var interrupt: Bool?
    var updatedInput: [String: JSONValue]?
    var always: Bool?
    var answers: [String: String]?
    var surface: String? = "ios"
    var optionId: String?

    static func allow() -> DecideBody { DecideBody(behavior: "allow") }
    static func deny(_ msg: String? = nil, interrupt: Bool = false) -> DecideBody {
        DecideBody(behavior: "deny", message: msg, interrupt: interrupt ? true : nil)
    }
}

// MARK: - Alerts (notification centre)

struct AlertItem: Codable, Identifiable, Hashable {
    var id: String
    var kind: String            // needs-you | finished | error | spend
    var title: String
    var body: String?
    var threadId: String?
    var url: String?
    var at: Int64
    var read: Bool?
    var count: Int?
    var quiet: Bool?
    var resolved: Bool?         // every approval it was about has ended
    var prompt: Bool?           // about a terminal prompt: the banner offers Yes and No

    var isRead: Bool { read ?? false }
    var isAlert: Bool { kind == "needs-you" || kind == "error" || kind == "spend" }
}

struct AlertsResponse: Decodable {
    var items: [AlertItem]?
    var unread: Int?

    private enum K: String, CodingKey { case items, unread }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: K.self)
        items = try c.decodeIfPresent([Lossy<AlertItem>].self, forKey: .items)?.compactMap(\.value)
        unread = try c.decodeIfPresent(Int.self, forKey: .unread)
    }
}

// MARK: - Threads (Workspace)

struct V3Thread: Decodable, Identifiable, Hashable {
    var id: String
    var title: String
    var cwd: String
    var ref: String?
    var tools: [String]?
    var sessions: [String]?
    var state: AgentState
    var startedAt: Int64
    var lastAt: Int64
    var turns: Int?
    var attention: Bool?
    var model: String?
    var parentThread: String?
    var childCount: Int?
    var childAttention: Int?
    var approvals: Int?
    var liveActivity: String?
    var display: String?
    var repo: String?
    var outcomeLine: String?
    var drivenId: String?
    var kind: String?
    var status: String?
    var group: String?   // a driven thread's server group (settle and snooze included)
    /// Set on a thread of another machine: its name, this Mac's grant there, and its id on that machine.
    var machine: String?
    var level: String?
    var peerID: String?
    var canAct: Bool { machine == nil || level == "control" || level == "full" }

    var sessionIDs: [String] { sessions ?? [] }
    var latestSession: String? { sessions?.last }
    var isDriven: Bool { kind == "driven" }
    var agent: String { tools?.first ?? (latestSession?.components(separatedBy: ":").first ?? "claude") }
    var name: String {
        if let d = display, !d.isEmpty { return d }
        if !title.isEmpty { return title }
        return (cwd as NSString).lastPathComponent
    }
    var repoName: String {
        if let r = repo, !r.isEmpty { return r }
        return (cwd as NSString).lastPathComponent
    }
}

struct ThreadsResponse: Decodable {
    var threads: [V3Thread]?
    var threadOf: [String: String]?

    private enum K: String, CodingKey { case threads, threadOf }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: K.self)
        threads = try c.decodeIfPresent([Lossy<V3Thread>].self, forKey: .threads)?.compactMap(\.value)
        threadOf = try c.decodeIfPresent([String: String].self, forKey: .threadOf)
    }
}

enum ThreadGroup: String, CaseIterable, Identifiable {
    case need, working, ready, settled
    var id: String { rawValue }
    var label: String {
        switch self {
        case .need: return "Needs you"
        case .working: return "Working"
        case .ready: return "Ready"
        case .settled: return "Settled"
        }
    }
    var empty: String {
        switch self {
        case .need: return "Nothing waiting on you"
        case .working: return "Nothing running"
        case .ready: return "Nothing to review"
        case .settled: return "No settled threads"
        }
    }
}

// MARK: - Watched session items

struct WFile: Codable, Hashable {
    var path: String
    var kind: String
    var add: Int
    var del: Int
}

struct WItem: Codable, Identifiable, Hashable {
    var id: String
    var runId: String
    var kind: String            // user | agent | reasoning | command | fileChange | mcp | web | plan | error | other
    var status: String
    var text: String
    var command: String?
    var exitCode: Int?
    var output: String?
    var files: [WFile]?
    var by: String?
    var bg: Bool?
    var startedAt: Int64
    var endedAt: Int64?
    var `repeat`: Int?
    var attachments: [OAttachment]?
    /// A tool call that a Code Mode script made: the script item's id.
    var parentId: String?
    /// Set on the client: this scheduled turn stands for this many identical runs, the first at wakeSince.
    var wakeRuns: Int?
    var wakeSince: Int64?

    var failed: Bool { status == "failed" || (exitCode ?? 0) != 0 }
}

enum WakeFold {
    /// Back-to-back runs of one scheduled task with the same answer show as the newest run plus a count.
    static func fold(_ items: [WItem]) -> [WItem] {
        var runs: [[WItem]] = []
        for it in items {
            if let last = runs.last?.last, last.runId == it.runId { runs[runs.count - 1].append(it) } else { runs.append([it]) }
        }
        var out: [[WItem]] = []
        for r in runs {
            if let prev = out.last, let w = wake(r), let pw = wake(prev), w.text == pw.text,
               !r.contains(where: { $0.status == "started" }), let a = answer(r), a == answer(prev) {
                var cur = r
                if let i = cur.firstIndex(where: { $0.id == w.id }) {
                    cur[i].wakeRuns = (pw.wakeRuns ?? 1) + 1
                    cur[i].wakeSince = pw.wakeSince ?? pw.startedAt
                }
                out[out.count - 1] = cur
            } else {
                out.append(r)
            }
        }
        return out.flatMap { $0 }
    }

    /// "Scheduled task: X watch (long instructions…)" reads as "Scheduled task: X watch".
    static func title(_ t: String) -> String {
        guard let head = t.range(of: ": ") else { return t }
        let body = t[head.upperBound...]
        var short = Substring(body)
        for sep in [" (", ". "] {
            if let r = short.range(of: sep), short.distance(from: short.startIndex, to: r.lowerBound) >= 8 { short = short[..<r.lowerBound] }
        }
        return String(t[..<head.upperBound]) + (short.count > 80 ? String(short.prefix(80)) + "…" : String(short))
    }

    private static func wake(_ r: [WItem]) -> WItem? { r.first { $0.kind == "user" && $0.by == "schedule" } }
    private static func answer(_ r: [WItem]) -> String? {
        let t = r.last { $0.kind == "agent" }?.text.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return t.isEmpty ? nil : t
    }
}

struct WRun: Codable, Identifiable, Hashable {
    var id: String
    var index: Int
    var prompt: String
    var status: String
    var startedAt: Int64
    var endedAt: Int64?
}

struct WatchedItems: Decodable {
    var id: String
    var agent: String?
    var items: [WItem]? { lossyItems?.compactMap(\.value) }
    private var lossyItems: [Lossy<WItem>]?
    var runs: [WRun]?
    var cursor: Int64
    var more: Bool?
    var earlier: Int64?
    var reset: Bool?

    private enum CodingKeys: String, CodingKey {
        case id, agent, runs, cursor, more, earlier, reset
        case lossyItems = "items"
    }
}

struct WatchedPane: Decodable {
    var registered: Bool?
    var sendable: Bool?
    var live: Bool?
    var agentId: String?
    var paneId: String?
}

// MARK: - Claude Code queue

struct CLIQueueItem: Codable, Identifiable, Hashable {
    var id: String
    var text: String
    var at: Int64
    var kind: String
    /// For a ! command agent-monitor sent: "waiting" or "running" until its result is in the transcript.
    var status: String?
    /// A running ! command's output as the terminal shows it now.
    var live: String?
}

struct CLIQueueView: Decodable {
    var agent: String?
    var supported: Bool
    var items: [CLIQueueItem]?
    var hidden: Int?
    var reachable: Bool
    var note: String?
    var canRemove: Bool
    var removeWhy: String?
    /// Text typed in the terminal's input box and not sent yet.
    var draft: String?
    /// The agent terminal's footer (model, context, mode).
    var statusLine: [String]?
}

// MARK: - Terminal question (a question or plan shown in the Claude Code pane)

struct TermOption: Codable, Hashable {
    var label: String
    var description: String?
}

struct TermQuestionItem: Codable, Hashable {
    var question: String
    var header: String?
    var multiSelect: Bool?
    var options: [TermOption]
}

struct TermPlanOption: Codable, Hashable {
    var n: Int
    var label: String
    var feedback: Bool?
}

struct TermQuestion: Decodable {
    var pending: Bool
    var tool: String?
    var toolUseId: String?
    var questions: [TermQuestionItem]?
    var plan: String?
    var planOptions: [TermPlanOption]?
    var held: Bool?
    var sendable: Bool
    var onScreen: Bool
    var why: String?
    var sentAt: Int64?
    /// The question the terminal shows now; earlier ones were answered there.
    var step: Int?

    var isPlan: Bool { tool == "ExitPlanMode" }
    var shown: Bool { pending && !(held ?? false) && (!(questions ?? []).isEmpty || isPlan) }
}

struct TermAnswer: Encodable {
    var option: Int?
    var options: [Int]?
    var text: String?
}

struct TermAnswerBody: Encodable {
    var toolUseId: String?
    var answers: [TermAnswer]?
    var text: String?
    var planOption: Int?
}

// MARK: - Subagents (children.go)

struct SessionChild: Decodable, Identifiable, Hashable {
    var key: String
    var kind: String            // subagent | agent | talk
    var agent: String
    var sessionId: String?
    var name: String?
    var subagentType: String?
    var description: String?
    var command: String?
    var model: String?
    var status: String          // running | completed | failed | killed | stopped
    var background: Bool?
    var startedAt: Int64
    var endedAt: Int64?
    var agentId: String?
    var prompt: String?
    var hasTranscript: Bool?
    var id: String { key }

    var title: String {
        for s in [description, name, command] { if let s, !s.isEmpty { return s } }
        return subagentType ?? kind
    }
}

struct SessionChildren: Decodable {
    var id: String
    var count: Int?
    var children: [SessionChild] { lossy?.compactMap(\.value) ?? [] }
    private var lossy: [Lossy<SessionChild>]?
    private enum CodingKeys: String, CodingKey { case id, count, lossy = "children" }
}

// MARK: - Plan usage windows (usage_limits.go)

struct UsageWindow: Decodable, Hashable {
    var key: String
    var label: String
    var usedPercent: Double?
    var resetsAt: Int64?
    var expired: Bool?
}

/// Tokens counted from this Mac's transcripts for one window; not the plan limit.
struct UsageLocal: Decodable, Hashable {
    var key: String
    var label: String?
    var from: Int64?
    var aligned: Bool?
    var tokens: TokenUsage
    var costUsd: Double?
    var costEstimated: Bool?
    var messages: Int?
}

/// Tokens one model used in one window, from this Mac's transcripts.
struct ModelUsage: Decodable, Hashable {
    var model: String
    var window: String
    var inTokens: Int64?
    var outTokens: Int64?
    var cacheRead: Int64?
    var cacheWrite: Int64?
    var costUsd: Double?
    var messages: Int?
}

struct AgentUsage: Decodable, Hashable {
    var agent: String
    var plan: String?
    var note: String?
    var installed: Bool?
    var windows: [UsageWindow] = []
    var local: [UsageLocal]?
    var models: [ModelUsage]?
    var credits: JSONValue?
    /// A reached cap, shown as a red line.
    var alert: String?
    /// The plan reading is older than one hour.
    var stale: Bool?
    /// When the plan reading was taken, ms.
    var measuredAt: Int64?
    /// The subscription (hashed key, masked email, organisation) and, in fleet readings, the machine.
    var account: UsageAccount?
    var machine: String?
    var hasReading: Bool { windows.contains { $0.usedPercent != nil } }
    var hasLocal: Bool { (local ?? []).contains { $0.tokens.total + $0.tokens.cacheRead + $0.tokens.cacheCreate > 0 } }
    var hasModels: Bool { !(models ?? []).isEmpty }

    init(agent: String, plan: String? = nil, note: String? = nil, installed: Bool? = nil, windows: [UsageWindow] = [],
         local: [UsageLocal]? = nil, models: [ModelUsage]? = nil, credits: JSONValue? = nil) {
        self.agent = agent; self.plan = plan; self.note = note; self.installed = installed
        self.windows = windows; self.local = local; self.models = models; self.credits = credits
    }

    private enum K: String, CodingKey { case agent, plan, note, installed, windows, local, models, credits, alert, stale, measuredAt, account, machine }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: K.self)
        agent = try c.decode(String.self, forKey: .agent)
        plan = try? c.decodeIfPresent(String.self, forKey: .plan)
        note = try? c.decodeIfPresent(String.self, forKey: .note)
        installed = try? c.decodeIfPresent(Bool.self, forKey: .installed)
        windows = (try? c.decodeIfPresent([Lossy<UsageWindow>].self, forKey: .windows))?.compactMap(\.value) ?? []
        local = (try? c.decodeIfPresent([Lossy<UsageLocal>].self, forKey: .local))?.compactMap(\.value)
        models = (try? c.decodeIfPresent([Lossy<ModelUsage>].self, forKey: .models))?.compactMap(\.value)
        credits = try? c.decodeIfPresent(JSONValue.self, forKey: .credits)
        alert = try? c.decodeIfPresent(String.self, forKey: .alert)
        stale = try? c.decodeIfPresent(Bool.self, forKey: .stale)
        measuredAt = try? c.decodeIfPresent(Int64.self, forKey: .measuredAt)
        account = try? c.decodeIfPresent(UsageAccount.self, forKey: .account)
        machine = try? c.decodeIfPresent(String.self, forKey: .machine)
    }
}

struct UsageAccount: Decodable, Hashable {
    var key: String
    var label: String?
    var org: String?
}

/// One subscription across machines: its newest reading and the machines that use it.
struct UsageGroup: Identifiable {
    var id: String
    var a: AgentUsage
    var machines: [String]

    func who(many: Bool) -> String {
        let acct = [a.account?.label, a.account?.org].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
        let whereText = many && !machines.isEmpty ? "on " + machines.joined(separator: ", ") : ""
        return [acct, whereText].filter { !$0.isEmpty }.joined(separator: " · ")
    }

    /// Groups by agent and account; a reading with no account groups by agent and machine.
    static func group(_ list: [AgentUsage]) -> [UsageGroup] {
        let installed = Set(list.filter { $0.installed != false }.map(\.agent))
        var out: [UsageGroup] = []
        for a in list {
            if a.installed == false, installed.contains(a.agent) { continue }
            let key = a.agent + "|" + (a.account.map { "acct:" + $0.key } ?? (a.installed == false ? "off" : "m:" + (a.machine ?? "")))
            func fresh(_ u: AgentUsage) -> Int64 { u.hasReading || u.credits != nil ? max(u.measuredAt ?? 1, 1) : 0 }
            if let i = out.firstIndex(where: { $0.id == key }) {
                if let m = a.machine, !out[i].machines.contains(m) { out[i].machines.append(m) }
                if fresh(a) > fresh(out[i].a) {
                    var n = a
                    n.local = out[i].a.local
                    n.models = out[i].a.models
                    out[i].a = n
                }
            } else {
                out.append(UsageGroup(id: key, a: a, machines: a.machine.map { [$0] } ?? []))
            }
        }
        return out
    }
}

struct FleetUsage: Decodable {
    var machines: [FleetMachine]?
    var agents: [AgentUsage]?
}

struct UsageLimits: Decodable {
    var agents: [AgentUsage]?
}

/// GET /api/orch/git?cwd= for a watched session's folder (read only).
struct OGitFile: Decodable, Hashable {
    var path: String
    var status: String
    var add: Int?
    var del: Int?
}

struct OGit: Decodable, Hashable {
    var isRepo: Bool?
    var branch: String?
    var head: String?
    var upstream: String?
    var ahead: Int?
    var behind: Int?
    var dirty: Int?
    var defaultBranch: String?
    var files: [OGitFile]?
}

// MARK: - Orchestrator (orch_api.go, orch_uploads.go, host_registry.go)

/// An uploaded file as the daemon returns it from POST /api/orch/uploads.
struct OAttachment: Codable, Hashable, Identifiable {
    var id: String
    var mime: String
    var name: String?
    var size: Int64?
    var url: String
}

struct OAgentInfo: Decodable, Hashable, Identifiable {
    var id: String
    var name: String
    var kind: String            // claude | codex | acp
    var installed: Bool
    var version: String?
    var canTerminal: Bool { kind == "claude" || kind == "codex" }
}

/// The driven thread fields the phone reads.
struct ODriven: Decodable, Hashable {
    var id: String
    var agent: String
    var title: String?
    var cwd: String?
    var status: String
    var parentId: String?
    var children: [String]?
    var error: String?
    /// The CLI session this thread continued ("Continue here"); its earlier turns show above the driven ones.
    var nativeId: String?
    var createdAt: Int64?
}

struct ODrivenDetail: Decodable {
    var thread: ODriven
    var items: [WItem] { lossy?.compactMap(\.value) ?? [] }
    /// The event seq at the snapshot; the stream resumes after it. Older daemons omit it.
    var seq: Int64?
    private var lossy: [Lossy<WItem>]?
    private enum CodingKeys: String, CodingKey { case thread, seq, lossy = "items" }
}

/// One event from GET /api/orch/stream (orch_hub.go): thread.updated, item.started, item.completed, item.delta and others.
struct OEvent: Decodable {
    var seq: Int64
    var threadId: String
    var type: String
    var data: Payload?

    struct Payload: Decodable {
        var thread: ODriven?
        var item: WItem?
        var itemId: String?
        var text: String?
        var field: String?
        private enum K: String, CodingKey { case thread, item, itemId, text, field }
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: K.self)
            thread = try? c.decodeIfPresent(ODriven.self, forKey: .thread)
            item = try? c.decodeIfPresent(WItem.self, forKey: .item)
            itemId = try? c.decodeIfPresent(String.self, forKey: .itemId)
            text = try? c.decodeIfPresent(String.self, forKey: .text)
            field = try? c.decodeIfPresent(String.self, forKey: .field)
        }
    }
}

struct OTerminalStart: Decodable {
    var session: String
    var hostId: String
    var attach: String
}

struct OHostInfo: Decodable {
    var hostId: String
    var agentId: String?
    /// A first-run question on the host screen, such as the folder trust dialog; set only before the session links.
    var prompt: OHostPrompt?
    var screenTail: [String]?
}

struct OHostPrompt: Decodable, Equatable {
    struct Option: Decodable, Equatable { var id: String; var label: String }
    var kind: String
    var agent: String
    var folder: String?
    var title: String?
    var text: String
    var options: [Option]
}

extension WItem {
    func withID(_ id: String) -> WItem {
        var x = self
        x.id = id
        return x
    }
}

// MARK: - Needs you (GET /api/needs-you): the one list every surface draws

struct NeedsYouItem: Decodable, Identifiable {
    var kind: String            // approval | mcp | question | prompt | error
    var key: String
    var sessionId: String?
    var threadId: String?       // a driven thread, for "error" items
    var agent: String?
    var cwd: String?
    var title: String?
    var at: Int64
    var answerable: Bool
    var why: String?
    var approval: Approval?
    var mcp: PermissionRequest?
    var question: TermQuestion?
    var message: String?
    var promptKey: String?
    /// Built on the phone from the older endpoints when the daemon has no /api/needs-you.
    var legacy: Bool?

    var id: String { key }
    var project: String { ((cwd ?? "") as NSString).lastPathComponent }
    /// The web panel's title rule: the session title, then the project, then "<Agent> session".
    var displayTitle: String {
        if let t = title, !t.isEmpty { return t }
        return project.isEmpty ? "\(agentName(agent ?? "")) session" : project
    }
}

struct NeedsYouResponse: Decodable {
    var items: [NeedsYouItem]?
    var count: Int?

    private enum K: String, CodingKey { case items, count }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: K.self)
        items = try c.decodeIfPresent([Lossy<NeedsYouItem>].self, forKey: .items)?.compactMap(\.value)
        count = try c.decodeIfPresent(Int.self, forKey: .count)
    }
}

extension AgentUsage {
    /// One line for the thread status area, e.g. "Credits 12.40 of 50.00 USD (25%)"; nil with no reading.
    var creditsLine: String? {
        guard case .object(let o)? = credits else { return nil }
        if case .number(let sp)? = o["spent"], case .number(let li)? = o["limit"] {
            var cur = ""
            if case .string(let c)? = o["currency"], !c.isEmpty { cur = " " + c }
            var pct = li > 0 ? sp / li * 100 : 0
            if case .number(let p)? = o["percent"] { pct = p }
            let capped: Bool = { if case .bool(true)? = o["capReached"] { return true }; return false }()
            return String(format: "Credits %.2f of %.2f%@ (%d%%)", sp, li, cur, Int(pct)) + (capped ? ", cap reached" : "")
        }
        if case .bool(true)? = o["unlimited"] { return "Credits: unlimited" }
        switch o["balance"] {
        case .number(let n)?: return "Credits: " + String(format: n == n.rounded() ? "%.0f" : "%.2f", n)
        case .string(let b)? where !b.isEmpty: return "Credits: \(b)"
        default: return nil
        }
    }
}
