import Foundation

// Codable mirrors of the Go structs in types.go / perm.go / talk.go /
// pane_registry.go. JSON keys match the `json:"..."` tags exactly.

enum AgentState: String, Codable, CaseIterable {
    case running
    case awaitingPermission = "awaiting-permission"
    case awaitingInput = "awaiting-input"
    case idle
    case completed
    case abandoned
    case unknown

    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = AgentState(rawValue: raw) ?? .unknown
    }

    /// Sessions that need a human — surfaced at the top of the list.
    var needsAttention: Bool { self == .awaitingPermission || self == .awaitingInput }
}

struct TokenUsage: Codable, Hashable {
    var input: Int64 = 0
    var output: Int64 = 0
    var cacheRead: Int64 = 0
    var cacheCreate: Int64 = 0
    var webSearch: Int64? = nil
    var webFetch: Int64? = nil

    var total: Int64 { input + output }
}

struct RecentEvent: Codable, Hashable, Identifiable {
    var ts: Int64
    var kind: String
    var text: String
    var id: String { "\(ts)-\(kind)-\(text.hashValue)" }
}

struct Session: Codable, Identifiable, Hashable {
    var id: String
    var tool: String
    var sessionId: String
    var cwd: String
    var state: AgentState
    var pid: Int?
    var startedAt: Int64
    var lastActivityAt: Int64
    var lastMessage: String?
    var permissionMessage: String?
    var title: String?
    var firstMessage: String?
    var model: String?
    var branch: String?
    var mode: String?
    var messageCount: Int
    var tokens: TokenUsage
    var toolUsage: [String: Int]?
    var subagentCount: Int?
    var filesTouched: Int?
    var bgTasksCount: Int?
    var transcriptPath: String?
    var recentEvents: [RecentEvent]?

    /// Last path component of cwd, for compact display.
    var project: String {
        let trimmed = cwd.hasSuffix("/") ? String(cwd.dropLast()) : cwd
        return (trimmed as NSString).lastPathComponent
    }

    var headline: String {
        if let t = title, !t.isEmpty { return t }
        if let f = firstMessage, !f.isEmpty { return f }
        return project
    }

    /// A copy-paste command that resumes this session in its project dir.
    /// Claude's session id *is* its resume id (`claude --resume <id>`). Each
    /// exited session resumes independently — there's no merge across sessions.
    /// nil for tools without a stable resume-by-id CLI.
    var resumeCommand: String? {
        guard !sessionId.isEmpty else { return nil }
        let resume: String
        switch tool {
        case "claude": resume = "claude --resume \(sessionId)"
        case "codex":  resume = "codex resume \(sessionId)"
        default: return nil
        }
        guard !cwd.isEmpty else { return resume }
        let dir = cwd.contains(" ") ? "\"\(cwd)\"" : cwd
        return "cd \(dir) && \(resume)"
    }
}

// MARK: - Session detail (on-demand transcript)

struct DetailMsg: Codable, Hashable, Identifiable {
    var ts: Int64
    var role: String
    var text: String
    var toolCount: Int
    var model: String?
    var id: String { "\(ts)-\(role)-\(text.prefix(24))" }
}

struct DetailTool: Codable, Hashable, Identifiable {
    var id: String
    var name: String
    var ts: Int64
    var args: String
    var result: String
    var isSubagent: Bool
}

struct DetailSub: Codable, Hashable, Identifiable {
    var id: String
    var subagentType: String
    var description: String
    var prompt: String
    var result: String
    var ts: Int64
}

struct DetailBgTask: Codable, Hashable, Identifiable {
    var ts: Int64
    var id: String
    var status: String
    var summary: String
}

struct SessionDetail: Codable {
    var session: Session?
    var messages: [DetailMsg]?
    var toolCalls: [DetailTool]?
    var subagents: [DetailSub]?
    var files: [String]?
    var bgTasks: [DetailBgTask]?
}

// MARK: - Session linkage (chains + spawn edges)

/// A group of related sessions (a /clear continuation, a shared handoff plan, or
/// same-pane runs) surfaced as one "chain". Mirrors the Go Chain struct.
struct Chain: Codable, Hashable, Identifiable {
    var id: String
    var cwd: String
    var ref: String
    var sessions: [String]   // member ids, oldest → newest
}

/// A child agent an Agent/Task/Workflow tool call launched. Mirrors SpawnChild.
struct SpawnChild: Codable, Hashable, Identifiable {
    var id: String
    var prompt: String
    var name: String
}

// MARK: - Permissions

struct PermissionRequest: Codable, Identifiable, Hashable {
    var id: String
    var toolName: String
    var input: [String: JSONValue]?
    var toolUseId: String?
    var cwd: String?
    var sessionId: String?
    var createdAt: Int64

    /// Ordered, readable input pairs for display.
    var inputPairs: [(String, String)] {
        (input ?? [:]).sorted { $0.key < $1.key }.map { ($0.key, $0.value.display) }
    }
}

// MARK: - Talks (inter-agent messages)

struct Talk: Codable, Identifiable, Hashable {
    var id: String
    var fromAgent: String
    var fromLabel: String
    var toAgent: String
    var toLabel: String?
    var message: String
    var status: String
    var reason: String?
    var reply: String?
    var createdAt: Int64
    var resolvedAt: Int64?
    var repliedAt: Int64?

    var isPending: Bool { status == "pending" || status == "pending_reply" }
}

// MARK: - Pane registry

struct PaneRegistration: Codable, Identifiable, Hashable {
    var agentId: String
    var tool: String
    var sessionId: String
    var paneId: String
    var tmuxSocket: String?
    var cwd: String
    var pid: Int?
    var alias: String?
    var registeredAt: Int64
    var lastSeenAt: Int64
    var source: String

    var id: String { agentId }
    var displayName: String { (alias?.isEmpty == false ? alias! : agentId) }
}
