import Foundation
import UserNotifications

/// The alert kinds a person can switch and give a sound in Settings, Alerts.
enum NotifyKind: String, CaseIterable, Identifiable {
    case needsYou, question, finished, error, spend
    var id: String { rawValue }
    var label: String {
        switch self {
        case .needsYou: return "Needs you"
        case .question: return "Questions"
        case .finished: return "Finished"
        case .error: return "Errors"
        case .spend: return "Spend"
        }
    }
    var detail: String {
        switch self {
        case .needsYou: return "Approvals, plans and terminals that wait for you."
        case .question: return "An agent asks you a question."
        case .finished: return "A thread finished its turn."
        case .error: return "A thread stopped on an error."
        case .spend: return "A spend limit is near or reached."
        }
    }
    var defaultOn: Bool { self != .finished }
    var defaultSound: NotifySound { self == .finished ? .ping : .chime }
    /// Daemon alert kind (notify.go) to a setting kind.
    static func from(alertKind k: String) -> NotifyKind {
        switch k {
        case "finished": return .finished
        case "error": return .error
        case "spend": return .spend
        case "question": return .question
        default: return .needsYou
        }
    }
}

/// Sounds bundled in the app; vibrate plays a silent file so iOS still gives the haptic.
enum NotifySound: String, CaseIterable, Identifiable {
    case system, chime, ping, knock, vibrate, none
    var id: String { rawValue }
    var label: String {
        switch self {
        case .system: return "Default"
        case .chime: return "Chime"
        case .ping: return "Ping"
        case .knock: return "Knock"
        case .vibrate: return "Vibrate only"
        case .none: return "None"
        }
    }
    var sound: UNNotificationSound? {
        switch self {
        case .system: return .default
        case .chime: return UNNotificationSound(named: UNNotificationSoundName("approval.caf"))
        case .ping: return UNNotificationSound(named: UNNotificationSoundName("ping.wav"))
        case .knock: return UNNotificationSound(named: UNNotificationSoundName("knock.wav"))
        case .vibrate: return UNNotificationSound(named: UNNotificationSoundName("silence.wav"))
        case .none: return nil
        }
    }
}

/// Alert preferences in UserDefaults; the @AppStorage keys in AlertSettingsView match these.
enum NotifyPrefs {
    static let d = UserDefaults.standard
    static func enabledKey(_ k: NotifyKind) -> String { "notify.on.\(k.rawValue)" }
    static func soundKey(_ k: NotifyKind) -> String { "notify.sound.\(k.rawValue)" }
    static var allOn: Bool { d.object(forKey: "notify.all") as? Bool ?? true }
    static func on(_ k: NotifyKind) -> Bool { allOn && (d.object(forKey: enabledKey(k)) as? Bool ?? k.defaultOn) }
    static func sound(_ k: NotifyKind) -> NotifySound { NotifySound(rawValue: d.string(forKey: soundKey(k)) ?? "") ?? k.defaultSound }
    static var quietOn: Bool { d.bool(forKey: "notify.quiet") }
    static var quietFrom: Int { d.object(forKey: "notify.quietFrom") as? Int ?? 22 }
    static var quietTo: Int { d.object(forKey: "notify.quietTo") as? Int ?? 7 }

    /// Quiet hours keep the banner but drop the sound.
    static func inQuietHours(_ now: Date = Date()) -> Bool {
        guard quietOn else { return false }
        let h = Calendar.current.component(.hour, from: now)
        return quietFrom <= quietTo ? (h >= quietFrom && h < quietTo) : (h >= quietFrom || h < quietTo)
    }
}

/// Where a tapped notification leads.
enum NotifyRoute: Equatable {
    case thread(String)
    case approvals(String?)
    case alerts
    case web(URL) // an https page, such as the install page of a new build; it opens in Safari

    init?(_ info: [AnyHashable: Any]) {
        if let s = info["url"] as? String, s.hasPrefix("https://"), let u = URL(string: s) { self = .web(u); return }
        if let t = info["threadId"] as? String, !t.isEmpty { self = .thread(t); return }
        if let s = info["sessionId"] as? String, !s.isEmpty { self = .thread(s); return }
        if info["approvals"] != nil { self = .approvals(info["approvalId"] as? String); return }
        if info["alerts"] != nil { self = .alerts; return }
        return nil
    }

    init?(url: URL) {
        guard url.scheme == "agentmonitor" else { return nil }
        let id = url.pathComponents.dropFirst().first.flatMap { $0.removingPercentEncoding }
        switch url.host {
        case "thread": guard let id, !id.isEmpty else { return nil }; self = .thread(id)
        case "approvals": self = .approvals(id)
        case "alerts": self = .alerts
        default: return nil
        }
    }
}

// Local notifications fired by the app while its live connection runs (no APNs account).
// The delegate is set at launch so a tap that cold-starts the app is not lost.
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifier()

    private var authorized = false
    /// A tap that arrived before the app set `onRoute`.
    private var pending: NotifyRoute?

    /// Opens the target of a tapped banner; set by the app. A route that came first is delivered at once.
    var onRoute: ((NotifyRoute) -> Void)? {
        didSet { if let r = pending, let f = onRoute { pending = nil; f(r) } }
    }

    func installDelegate() { UNUserNotificationCenter.current().delegate = self }

    func requestAuthorization() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            Task { @MainActor in self.authorized = granted }
        }
    }

    private func post(id: String, kind: NotifyKind, title: String, body: String, info: [String: String], force: Bool = false) {
        guard authorized || force, force || NotifyPrefs.on(kind) else { return }
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = NotifyPrefs.inQuietHours() ? nil : NotifyPrefs.sound(kind).sound
        content.interruptionLevel = kind == .finished ? .active : .timeSensitive
        content.threadIdentifier = info["threadId"] ?? info["sessionId"] ?? kind.rawValue
        content.userInfo = info
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    /// A session waiting in its terminal; a tap opens that session's thread.
    func sessionAsk(sessionID: String, question: Bool, title: String, body: String) {
        post(id: "approval-\(sessionID)", kind: question ? .question : .needsYou, title: title, body: body, info: ["sessionId": sessionID])
    }

    /// An MCP permission request; a tap opens Approvals on it.
    func approvalAsk(id: String, title: String, body: String) {
        post(id: "approval-\(id)", kind: .needsYou, title: title, body: body, info: ["approvals": "1", "approvalId": id])
    }

    /// An alert from the daemon's notification centre; a tap opens its thread, else Approvals.
    func alert(id: String, kind: NotifyKind, title: String, body: String, threadId: String?, url: String? = nil) {
        var info: [String: String] = [:]
        if let t = threadId, !t.isEmpty { info["threadId"] = t } else { info["alerts"] = "1" }
        if let u = url, u.hasPrefix("https://") { info["url"] = u }
        post(id: "alert-\(id)", kind: kind, title: title, body: body, info: info)
    }

    /// Settings test: a sample of this kind with its current sound, even when the kind is off.
    func test(_ kind: NotifyKind, threadId: String?) {
        let go = { [self] in
            post(id: "test-\(kind.rawValue)-\(Date().timeIntervalSince1970)", kind: kind, title: "Test: \(kind.label)",
                 body: "This is how a \(kind.label.lowercased()) alert sounds.", info: threadId.map { ["threadId": $0] } ?? [:], force: true)
        }
        if authorized { go(); return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            Task { @MainActor in self.authorized = granted; if granted { go() } }
        }
    }

    func clearAlert(id: String) {
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: ["alert-\(id)"])
    }

    func clear(id: String) {
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: ["approval-\(id)"])
    }

    func handle(_ route: NotifyRoute) {
        if let f = onRoute { f(route) } else { pending = route }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                            willPresent notification: UNNotification) async
        -> UNNotificationPresentationOptions {
        [.banner, .sound, .list]
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                            didReceive response: UNNotificationResponse) async {
        guard let r = NotifyRoute(response.notification.request.content.userInfo) else { return }
        await MainActor.run { self.handle(r) }
    }
}
