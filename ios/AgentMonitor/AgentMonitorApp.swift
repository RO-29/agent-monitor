import SwiftUI

@main
struct AgentMonitorApp: App {
    @State private var store = AppStore()
    @Environment(\.scenePhase) private var scenePhase

    init() {
        Theme.applyAppearance()
        Notifier.shared.installDelegate()
        SigningWatch.register()
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(store)
                .tint(Tok.accent)
                .task {
                    Notifier.shared.onRoute = { [store] r in store.open(r) }
                    #if DEBUG
                    if DebugRoute.screen == nil { Notifier.shared.requestAuthorization() }
                    #else
                    Notifier.shared.requestAuthorization()
                    #endif
                    store.start()
                    // A cold launch can open already active, so the phase change below never fires.
                    Task { await SigningWatch.tick() }
                }
                .onChange(of: scenePhase) { _, phase in
                    // Reconnect when returning to foreground; the socket is
                    // often dropped while backgrounded.
                    // Only a real background ends the Undo window; pulling down Control Center is .inactive.
                    if phase == .active { store.connect() } else if phase == .background { store.flushPendingDecisions() }
                    store.setForeground(phase == .active)
                    if phase == .active { Task { await SigningWatch.tick() } }
                }
                // Bark pushes open agentmonitor://thread/<id>, agentmonitor://approvals or agentmonitor://alerts.
                .onOpenURL { url in
                    if let r = NotifyRoute(url: url) { store.open(r) }
                }
        }
    }
}
