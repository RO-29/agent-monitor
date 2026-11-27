import ActivityKit
import Foundation
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
    static let group = "group.com.rohit.agentmonitor"
    static let liveKey = "liveActivityOn"

    static func publish(_ s: AgentsActivity.ContentState) {
        if let d = UserDefaults(suiteName: group), let data = try? JSONEncoder().encode(s) {
            let old = d.data(forKey: "glance")
            d.set(data, forKey: "glance")
            d.set(Date().timeIntervalSince1970, forKey: "glanceAt")
            if old != data { WidgetCenter.shared.reloadAllTimelines() }
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
