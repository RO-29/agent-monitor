import SwiftUI

enum Theme {
    static func color(for state: AgentState) -> Color {
        switch state {
        case .running: return .green
        case .awaitingPermission: return .orange
        case .awaitingInput: return .yellow
        case .idle: return .gray
        case .completed: return .blue
        case .abandoned: return .secondary
        case .unknown: return .secondary
        }
    }

    static func label(for state: AgentState) -> String {
        switch state {
        case .running: return "Running"
        case .awaitingPermission: return "Needs approval"
        case .awaitingInput: return "Needs input"
        case .idle: return "Idle"
        case .completed: return "Done"
        case .abandoned: return "Abandoned"
        case .unknown: return "Unknown"
        }
    }

    static func icon(for tool: String) -> String {
        switch tool {
        case "claude": return "sparkles"
        case "codex": return "chevron.left.forwardslash.chevron.right"
        case "opencode": return "curlybraces"
        case "cursor", "cursor-agent": return "cursorarrow.rays"
        default: return "terminal"
        }
    }
}

/// Compact relative time from a unix-millis timestamp.
func relativeTime(_ millis: Int64) -> String {
    guard millis > 0 else { return "—" }
    let secs = Int((Double(Date().timeIntervalSince1970) - Double(millis) / 1000.0))
    if secs < 5 { return "now" }
    if secs < 60 { return "\(secs)s" }
    let mins = secs / 60
    if mins < 60 { return "\(mins)m" }
    let hrs = mins / 60
    if hrs < 24 { return "\(hrs)h" }
    return "\(hrs / 24)d"
}

/// Human token count: 12345 -> "12.3k".
func compactCount(_ n: Int64) -> String {
    if n < 1000 { return "\(n)" }
    if n < 1_000_000 { return String(format: "%.1fk", Double(n) / 1000) }
    return String(format: "%.1fM", Double(n) / 1_000_000)
}
