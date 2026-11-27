import SwiftUI

/// The Needs you tab: the waiting requests, or the recent alerts.
struct NeedsTabView: View {
    @Environment(AppStore.self) private var store

    var body: some View {
        if store.needsRecent { AlertsView() } else { PermissionsView() }
    }
}
