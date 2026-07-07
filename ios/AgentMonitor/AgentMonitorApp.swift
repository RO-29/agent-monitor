import SwiftUI

@main
struct AgentMonitorApp: App {
    @State private var store = AppStore()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(store)
                .task { Notifier.shared.requestAuthorization(); store.start() }
                .onChange(of: scenePhase) { _, phase in
                    // Reconnect when returning to foreground; the socket is
                    // often dropped while backgrounded.
                    if phase == .active { store.connect() }
                }
        }
    }
}
