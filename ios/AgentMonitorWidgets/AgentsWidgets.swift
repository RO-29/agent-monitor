import ActivityKit
import SwiftUI
import WidgetKit

// The same type as in the app; ActivityKit matches it by name.
struct AgentsActivity: ActivityAttributes {
    struct ContentState: Codable, Hashable {
        var working: Int
        var waiting: Int
        var oldestName: String
        var oldestSince: Date?
    }
    var machine: String
}

private let needColor = Color(red: 0.94, green: 0.68, blue: 0.29)
private let runColor = Color(red: 0.30, green: 0.76, blue: 0.85)

struct GlanceEntry: TimelineEntry {
    let date: Date
    let state: AgentsActivity.ContentState
    let updated: Date?
}

struct GlanceProvider: TimelineProvider {
    func placeholder(in context: Context) -> GlanceEntry {
        GlanceEntry(date: .now, state: .init(working: 2, waiting: 1, oldestName: "api-server", oldestSince: .now.addingTimeInterval(-240)), updated: .now)
    }
    func getSnapshot(in context: Context, completion: @escaping (GlanceEntry) -> Void) { completion(read()) }
    func getTimeline(in context: Context, completion: @escaping (Timeline<GlanceEntry>) -> Void) {
        completion(Timeline(entries: [read()], policy: .after(.now.addingTimeInterval(15 * 60))))
    }
    private func read() -> GlanceEntry {
        let d = UserDefaults(suiteName: "group.com.rohit.agentmonitor")
        let s = d?.data(forKey: "glance").flatMap { try? JSONDecoder().decode(AgentsActivity.ContentState.self, from: $0) }
        let at = d?.double(forKey: "glanceAt") ?? 0
        return GlanceEntry(date: .now, state: s ?? .init(working: 0, waiting: 0, oldestName: "", oldestSince: nil), updated: at > 0 ? Date(timeIntervalSince1970: at) : nil)
    }
}

struct GlanceView: View {
    let e: GlanceEntry
    @Environment(\.widgetFamily) private var family

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 14) {
                count(e.state.waiting, "wait", needColor)
                count(e.state.working, "work", runColor)
            }
            if e.state.waiting > 0, !e.state.oldestName.isEmpty {
                VStack(alignment: .leading, spacing: 1) {
                    Text(e.state.oldestName).font(.caption.weight(.semibold)).lineLimit(1)
                    if let t = e.state.oldestSince { Text(t, style: .relative).font(.caption2).foregroundStyle(.secondary) }
                }
            } else if e.state.working == 0 {
                Text("All agents idle").font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
            if let u = e.updated {
                (Text("Updated ") + Text(u, style: .relative) + Text(" ago")).font(.caption2).foregroundStyle(.tertiary).lineLimit(1)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .widgetURL(URL(string: "agentmonitor://approvals"))
    }

    private func count(_ n: Int, _ label: String, _ c: Color) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            Text("\(n)").font(.system(size: 30, weight: .bold, design: .rounded)).foregroundStyle(n > 0 ? c : .secondary)
            Text(label).font(.caption2.weight(.medium)).foregroundStyle(.secondary)
        }
    }
}

struct AgentsWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "AgentsWidget", provider: GlanceProvider()) { e in
            GlanceView(e: e).containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Agents")
        .description("How many agents wait for you and how many work.")
        .supportedFamilies([.systemSmall, .systemMedium])
    }
}

struct AgentsLiveActivity: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: AgentsActivity.self) { ctx in
            HStack(spacing: 16) {
                label(ctx.state.waiting, "waiting", needColor)
                label(ctx.state.working, "working", runColor)
                Spacer()
                if ctx.state.waiting > 0, let t = ctx.state.oldestSince {
                    VStack(alignment: .trailing, spacing: 1) {
                        Text(ctx.state.oldestName).font(.caption.weight(.semibold)).lineLimit(1)
                        Text(t, style: .relative).font(.caption2).foregroundStyle(.secondary)
                    }
                }
            }
            .padding(14)
            .widgetURL(URL(string: "agentmonitor://approvals"))
        } dynamicIsland: { ctx in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) { label(ctx.state.waiting, "waiting", needColor) }
                DynamicIslandExpandedRegion(.trailing) { label(ctx.state.working, "working", runColor) }
                DynamicIslandExpandedRegion(.bottom) {
                    if ctx.state.waiting > 0 { Text(ctx.state.oldestName).font(.caption).lineLimit(1) }
                }
            } compactLeading: {
                Text("\(ctx.state.waiting)").foregroundStyle(ctx.state.waiting > 0 ? needColor : .secondary).bold()
            } compactTrailing: {
                Text("\(ctx.state.working)").foregroundStyle(runColor)
            } minimal: {
                Text("\(ctx.state.waiting > 0 ? ctx.state.waiting : ctx.state.working)").foregroundStyle(ctx.state.waiting > 0 ? needColor : runColor)
            }
            .widgetURL(URL(string: "agentmonitor://approvals"))
        }
    }

    private func label(_ n: Int, _ s: String, _ c: Color) -> some View {
        HStack(spacing: 5) {
            Text("\(n)").font(.title3.weight(.bold)).foregroundStyle(n > 0 ? c : .secondary)
            Text(s).font(.caption).foregroundStyle(.secondary)
        }
    }
}

@main
struct AgentsWidgetBundle: WidgetBundle {
    var body: some Widget {
        AgentsWidget()
        AgentsLiveActivity()
    }
}
