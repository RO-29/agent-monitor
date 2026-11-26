import Foundation
import UIKit
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

    func installDelegate() {
        UNUserNotificationCenter.current().delegate = self
        NotifyAction.register()
    }

    /// Sets the delegate and reads the permission without the system prompt; askIfNeeded shows it later.
    func prepare() {
        UNUserNotificationCenter.current().delegate = self
        NotifyAction.register()
        Task { await refreshAuthorization() }
    }

    /// The system prompt shows once, at the first banner, so the reason is clear; that banner posts after a grant.
    private func askIfNeeded(then: @escaping @MainActor () -> Void) {
        #if DEBUG
        if DebugRoute.screen != nil { return }
        #endif
        guard !asked else { return }
        asked = true
        Task {
            guard await UNUserNotificationCenter.current().notificationSettings().authorizationStatus == .notDetermined else { return }
            let granted = (try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])) ?? false
            authorized = granted
            if granted { then() }
        }
    }
    private var asked = false

    func requestAuthorization() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        NotifyAction.register()
        center.requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            Task { @MainActor in self.authorized = granted }
        }
    }

    /// Reads the iOS permission again: the user can turn alerts on or off in Settings while the app runs.
    @discardableResult
    func refreshAuthorization() async -> UNAuthorizationStatus {
        let st = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
        authorized = st == .authorized || st == .provisional || st == .ephemeral
        return st
    }

    private func post(id: String, kind: NotifyKind, title: String, body: String, info: [String: String], force: Bool = false, category: String? = nil) {
        guard force || NotifyPrefs.on(kind) else { return }
        guard authorized || force else {
            askIfNeeded { [weak self] in self?.post(id: id, kind: kind, title: title, body: body, info: info, category: category) }
            return
        }
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = NotifyPrefs.inQuietHours() ? nil : NotifyPrefs.sound(kind).sound
        content.interruptionLevel = kind == .finished ? .active : .timeSensitive
        content.threadIdentifier = info["threadId"] ?? info["sessionId"] ?? kind.rawValue
        content.userInfo = info
        if let category { content.categoryIdentifier = category }
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    /// A session waiting in its terminal; a tap opens that session's thread.
    func sessionAsk(sessionID: String, question: Bool, title: String, body: String) {
        post(id: "approval-\(sessionID)", kind: question ? .question : .needsYou, title: title, body: body, info: ["sessionId": sessionID],
             category: question ? nil : NotifyAction.prompt)
    }

    /// An MCP permission request; a tap opens Approvals on it.
    func approvalAsk(id: String, title: String, body: String) {
        post(id: "approval-\(id)", kind: .needsYou, title: title, body: body, info: ["approvals": "1", "approvalId": id])
    }

    /// A tool permission request on this server; Allow and Deny answer it from the banner.
    func permissionAsk(id: String, title: String, body: String) {
        post(id: "approval-\(id)", kind: .needsYou, title: title, body: body, info: ["approvals": "1", "approvalId": id], category: NotifyAction.permission)
    }

    /// The result of a banner action that did not work; a tap opens the thread.
    func actionFailed(_ message: String, threadId: String?) {
        post(id: "action-\(Date().timeIntervalSince1970)", kind: .error, title: "Not sent", body: message,
             info: threadId.map { ["threadId": $0] } ?? ["alerts": "1"], force: true)
    }

    /// An alert from the daemon's notification centre; a tap opens its thread, else Approvals.
    func alert(id: String, kind: NotifyKind, title: String, body: String, threadId: String?, url: String? = nil,
               prompt: Bool = false, force: Bool = false) {
        var info: [String: String] = [:]
        if let t = threadId, !t.isEmpty { info["threadId"] = t } else { info["alerts"] = "1" }
        if let u = url, u.hasPrefix("https://") { info["url"] = u }
        // A thread alert gets Reply; one about a terminal prompt also gets Yes and No.
        let category = (threadId ?? "").isEmpty || kind == .spend ? nil : prompt ? NotifyAction.needs : NotifyAction.reply
        post(id: "alert-\(id)", kind: kind, title: title, body: body, info: info, force: force, category: category)
    }

    /// The app's signing profile ends soon; SigningWatch sends it once per end date.
    func signing(title: String, body: String) {
        post(id: "signing", kind: .needsYou, title: title, body: body, info: [:], force: true)
    }

    /// Settings test: a sample of this kind with its current sound, even when the kind is off. False when iOS blocks alerts.
    func test(_ kind: NotifyKind, threadId: String?) async -> Bool {
        if await refreshAuthorization() == .notDetermined {
            _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])
            await refreshAuthorization()
        }
        guard authorized else { return false }
        post(id: "test-\(kind.rawValue)-\(Date().timeIntervalSince1970)", kind: kind, title: "Test: \(kind.label)",
             body: "This is how a \(kind.label.lowercased()) alert sounds.", info: threadId.map { ["threadId": $0] } ?? [:], force: true)
        return true
    }

    /// Replaces a banner after its action worked, with no sound, so the user sees the answer went.
    func actionDone(id: String, title: String, body: String, group: String) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.interruptionLevel = .passive
        content.threadIdentifier = group
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
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
        let info = response.notification.request.content.userInfo
        let action = response.actionIdentifier
        if action != UNNotificationDefaultActionIdentifier, action != UNNotificationDismissActionIdentifier {
            let text = (response as? UNTextInputNotificationResponse)?.userText
            await NotifyAction.run(action, info: info, text: text, bannerID: response.notification.request.identifier,
                                   title: response.notification.request.content.title)
            return
        }
        guard let r = NotifyRoute(info) else { return }
        await MainActor.run { self.handle(r) }
    }
}

/// Buttons on banners: answer a terminal prompt or a permission, or reply to a thread, without opening the app.
enum NotifyAction {
    static let prompt = "am.prompt", permission = "am.perm", reply = "am.reply", needs = "am.needs"
    static let yes = "am.yes", no = "am.no", allow = "am.allow", deny = "am.deny", send = "am.send"

    static func register() {
        // Face ID cannot show in the background, so with Face ID for approvals these buttons open the app.
        let open: UNNotificationActionOptions = Bio.enabled ? [.authenticationRequired, .foreground] : [.authenticationRequired]
        let yesA = UNNotificationAction(identifier: yes, title: "Yes", options: open)
        let noA = UNNotificationAction(identifier: no, title: "No", options: open.union(.destructive))
        let allowA = UNNotificationAction(identifier: allow, title: "Allow", options: open)
        let denyA = UNNotificationAction(identifier: deny, title: "Deny", options: [.authenticationRequired, .destructive])
        let replyA = UNTextInputNotificationAction(identifier: send, title: "Reply", options: [.authenticationRequired],
                                                   textInputButtonTitle: "Send", textInputPlaceholder: "Message the agent")
        UNUserNotificationCenter.current().setNotificationCategories([
            UNNotificationCategory(identifier: prompt, actions: [yesA, noA], intentIdentifiers: []),
            UNNotificationCategory(identifier: permission, actions: [allowA, denyA], intentIdentifiers: []),
            UNNotificationCategory(identifier: reply, actions: [replyA], intentIdentifiers: []),
            UNNotificationCategory(identifier: needs, actions: [yesA, noA, replyA], intentIdentifiers: []),
        ])
    }

    /// The Face ID prompt needs the app on screen; a foreground action starts before it gets there.
    static func waitUntilActive() async {
        for _ in 0..<30 {
            if await MainActor.run(body: { UIApplication.shared.applicationState == .active }) { return }
            try? await Task.sleep(for: .milliseconds(100))
        }
    }

    /// Maps a thread id to the session a reply should go to; the app sets it from the store.
    @MainActor static var replyTarget: ((String) async -> String)?

    static func run(_ action: String, info: [AnyHashable: Any], text: String?, bannerID: String, title: String) async {
        let thread = (info["sessionId"] as? String) ?? (info["threadId"] as? String)
        let api = APIClient.shared
        var done = ""
        if Bio.enabled, [yes, no, allow].contains(action) { await waitUntilActive() }
        do {
            switch action {
            case allow, deny:
                guard let id = info["approvalId"] as? String else { return }
                try await api.respondPermission(id, allow: action == allow)
                done = action == allow ? "Allowed" : "Denied"
            case yes, no:
                guard let sid = thread else { return }
                // The daemon answers only the prompt the card saw: take its fingerprint first.
                let items = try await api.needsYou().items ?? []
                guard let it = items.first(where: { $0.kind == "prompt" && $0.sessionId == sid }) else {
                    await fail("No prompt is open in that terminal now.", thread)
                    return
                }
                guard it.answerable, let key = it.promptKey else {
                    await fail(it.why ?? "Answer it in the terminal.", thread)
                    return
                }
                try await api.answerPrompt(sid, yes: action == yes, key: key)
                done = action == yes ? "Answered Yes" : "Answered No"
            case send:
                let body = (text ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
                guard let raw = thread, !body.isEmpty else { return }
                // A resumed thread has a newer session than the one the banner named.
                // A cold launch from the banner: the store sets the closure, then loads the threads.
                var target = await MainActor.run { replyTarget }
                var tries = 0
                while target == nil && tries < 30 {
                    try? await Task.sleep(for: .milliseconds(100))
                    target = await MainActor.run { replyTarget }
                    tries += 1
                }
                let id = await target?(raw) ?? raw
                if id.contains(":") {
                    _ = try await api.cliSendFull(id, text: body, attachments: [])
                } else {
                    try await api.drivenSend(id, text: body, attachments: [])
                }
                done = "Sent: " + String(body.prefix(80))
            default:
                return
            }
        } catch {
            await fail(error.localizedDescription, thread)
            return
        }
        // Plain String copies keep the main-actor hop Sendable.
        let msg = done, group = thread ?? "done"
        if !msg.isEmpty {
            await MainActor.run { Notifier.shared.actionDone(id: bannerID, title: msg, body: title, group: group) }
        }
    }

    @MainActor private static func fail(_ message: String, _ thread: String?) {
        Notifier.shared.actionFailed(message, threadId: thread)
    }
}
