import ActivityKit
import Foundation
import Security
import WidgetKit

/// The Live Activity's data; AgentMonitorWidgets has the same type, and ActivityKit matches it by name.
struct AgentsActivity: ActivityAttributes {
    struct ContentState: Codable, Hashable {
        var working: Int
        var waiting: Int
        var oldestName: String
        var oldestSince: Date?
    }
    var machine: String
}

/// Counts for the home screen widget and the Live Activity, written while the app runs.
enum AgentsGlance {
    static let liveKey = "liveActivityOn"
    private static var last: AgentsActivity.ContentState?
    private static var lastWrite = Date.distantPast

    static func publish(_ s: AgentsActivity.ContentState) {
        // Write on a change, and once a minute so the widget can tell an old count from a fresh one.
        if s != last || Date().timeIntervalSince(lastWrite) > 60, let data = try? JSONEncoder().encode(GlanceSnapshot(state: s, at: Date())) {
            GlanceStore.write(data)
            if s != last { WidgetCenter.shared.reloadAllTimelines() }
            last = s
            lastWrite = Date()
        }
        Task { await live(s) }
    }

    /// One Live Activity while any agent works or waits; it ends when all are idle.
    @MainActor static func live(_ s: AgentsActivity.ContentState) async {
        let on = UserDefaults.standard.object(forKey: liveKey) as? Bool ?? true
        let current = Activity<AgentsActivity>.activities
        let content = ActivityContent(state: s, staleDate: Date().addingTimeInterval(15 * 60))
        if !on || (s.working == 0 && s.waiting == 0) {
            for a in current { await a.end(content, dismissalPolicy: .immediate) }
            return
        }
        if let a = current.first {
            if a.content.state != s { await a.update(content) }
            return
        }
        guard ActivityAuthorizationInfo().areActivitiesEnabled else { return }
        _ = try? Activity.request(attributes: AgentsActivity(machine: "agent-monitor"), content: content)
    }
}

extension AppStore {
    func glance() -> AgentsActivity.ContentState {
        let oldest = needsYouItems.min { $0.at < $1.at }
        let name = oldest.map { $0.title ?? (($0.cwd ?? "") as NSString).lastPathComponent } ?? ""
        return .init(working: threads(in: .working).count, waiting: needsYouThreadCount, oldestName: name,
                     oldestSince: oldest.map { Date(timeIntervalSince1970: TimeInterval($0.at) / 1000) })
    }
}

/// A small Keychain item both the app and the widget can read: free Apple accounts have no App Groups.
enum GlanceStore {
    private static var group: String? { Bundle.main.object(forInfoDictionaryKey: "AMKeychainGroup") as? String }
    private static func base() -> [String: Any] {
        var q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "agent-monitor.glance", kSecAttrAccount as String: "glance"]
        if let g = group, !g.hasPrefix("$"), !g.hasPrefix(".") { q[kSecAttrAccessGroup as String] = g }
        return q
    }
    static func read() -> Data? {
        var q = base()
        q[kSecReturnData as String] = true
        var out: AnyObject?
        return SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess ? out as? Data : nil
    }
    static func write(_ data: Data) {
        let attrs: [String: Any] = [kSecValueData as String: data, kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlock]
        if SecItemUpdate(base() as CFDictionary, attrs as CFDictionary) == errSecItemNotFound {
            SecItemAdd(base().merging(attrs) { $1 } as CFDictionary, nil)
        }
    }
}

/// What the widget shows: the counts and when the app wrote them.
struct GlanceSnapshot: Codable {
    var state: AgentsActivity.ContentState
    var at: Date
}
