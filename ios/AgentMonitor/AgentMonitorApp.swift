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
                .modifier(AppLockGate())
                .environment(store)
                .tint(Tok.accent)
                .task {
                    Notifier.shared.onRoute = { [store] r in store.open(r) }
                    NotifyAction.replyTarget = { [store] id in await store.replyTargetLoaded(id) }
                    Notifier.shared.prepare()
                    store.start()
                    // A cold launch can open already active, so the phase change below never fires.
                    if scenePhase == .active { store.setForeground(true) }
                    Task { await SigningWatch.tick() }
                    while !Task.isCancelled {
                        // Before the first load the counts are zero, which would end the Live Activity.
                        if store.threadsLoaded { AgentsGlance.publish(store.glance()) }
                        try? await Task.sleep(for: .seconds(5))
                    }
                }
                .onChange(of: scenePhase) { _, phase in
                    // Reconnect when returning to foreground; the socket is
                    // often dropped while backgrounded.
                    // Only a real background ends the Undo window; pulling down Control Center is .inactive.
                    if phase == .active {
                        store.resume()
                        Task { await Notifier.shared.refreshAuthorization() }
                    } else if phase == .background {
                        store.backgroundAt = Date()
                        store.flushPendingDecisions()
                    }
                    // .inactive (Control Center, a banner pull) is no change: keep presence and the polls.
                    if phase != .inactive { store.setForeground(phase == .active) }
                    if phase == .active { Task { await SigningWatch.tick() } }
                }
                // Bark pushes open agentmonitor://thread/<id>, agentmonitor://approvals or agentmonitor://alerts.
                .onOpenURL { url in
                    if let r = NotifyRoute(url: url) { store.open(r) }
                }
        }
    }
}
