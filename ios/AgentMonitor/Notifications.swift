import Foundation
import UserNotifications

// Local notifications for approval asks. Remote push (APNs) needs a paid Apple
// Developer account, so we fire *local* notifications from the app itself when
// its live connection sees a new approval — works in the foreground and while
// backgrounded as long as the socket is alive. A bundled custom sound
// (approval.caf) makes the alert instantly recognizable.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifier()

    /// The bundled chime; must match the filename in the app bundle.
    static let sound = UNNotificationSound(named: UNNotificationSoundName("approval.caf"))

    private var authorized = false

    func requestAuthorization() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            Task { @MainActor in self.authorized = granted }
        }
    }

    /// Fire an approval notification with the distinct tune. `id` dedupes so the
    /// same pending ask doesn't re-alert on every reconnect/upsert.
    func approvalAsk(id: String, title: String, body: String) {
        guard authorized else { return }
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = Self.sound
        content.interruptionLevel = .timeSensitive   // breaks through Focus when possible
        let req = UNNotificationRequest(identifier: "approval-\(id)", content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req)
    }

    /// Clear a delivered approval notification once it's been answered.
    func clear(id: String) {
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: ["approval-\(id)"])
    }

    // Show the banner + play the sound even when the app is in the foreground.
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                            willPresent notification: UNNotification) async
        -> UNNotificationPresentationOptions {
        [.banner, .sound, .list]
    }
}
