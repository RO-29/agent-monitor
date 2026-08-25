import SwiftUI

struct RootView: View {
    @Environment(AppStore.self) private var store

    var body: some View {
        TabView {
            SessionsView()
                .tabItem { Label("Agents", systemImage: "cpu") }
                .badge(store.attentionCount)

            PermissionsView()
                .tabItem { Label("Approvals", systemImage: "checkmark.shield") }
                .badge(store.approvalsCount)

            TalksView()
                .tabItem { Label("Talks", systemImage: "bubble.left.and.bubble.right") }
                .badge(store.pendingTalks.count)

            PanesView()
                .tabItem { Label("Panes", systemImage: "rectangle.split.3x1") }

            SettingsView()
                .tabItem { Label("Settings", systemImage: "gearshape") }
        }
    }
}

/// Live-connection chip reused in nav bars.
struct ConnectionChip: View {
    @Environment(AppStore.self) private var store
    var body: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(dotColor)
                .frame(width: 8, height: 8)
            Text(store.connection.label)
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }
    private var dotColor: Color {
        switch store.connection {
        case .connected: return .green
        case .connecting: return .yellow
        case .disconnected: return .red
        }
    }
}
