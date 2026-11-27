// Native notifications from the daemon's /api/notify/stream.
import Cocoa
import LocalAuthentication
import UserNotifications

struct NotifyItem: Decodable {
    let id: String
    let kind: String
    let title: String
    let body: String?
    let threadId: String?
    let url: String?
    let quiet: Bool?
    let resolved: Bool?
    let count: Int?
}

struct PendingApprovals: Decodable { let pending: [PendingApproval] }
struct PendingApproval: Decodable {
    let id: String
    let kind: String?
    let sessionId: String?
    let drivenId: String?
    let rootId: String?
}

// authorized adds the device token when the daemon is remote.
func authorized(_ r: inout URLRequest) {
    if !kToken.isEmpty { r.setValue("Bearer " + kToken, forHTTPHeaderField: "Authorization") }
}

// NotifyStream reads the server-sent stream and reconnects with Last-Event-ID and a capped backoff.
final class NotifyStream: NSObject, URLSessionDataDelegate {
    private var session: URLSession?
    private var buffer = Data()
    private var lastId = ""
    private var delay: TimeInterval = 2
    private var stopped = true
    private let onItem: (NotifyItem) -> Void

    init(onItem: @escaping (NotifyItem) -> Void) {
        self.onItem = onItem
        super.init()
    }

    func start() {
        stopped = false
        if session == nil {
            let cfg = URLSessionConfiguration.default
            cfg.timeoutIntervalForRequest = 90
            cfg.timeoutIntervalForResource = 7 * 24 * 3600
            session = URLSession(configuration: cfg, delegate: self, delegateQueue: .main)
        }
        connect()
    }

    // stop cancels the request and breaks the session's strong reference to this delegate.
    func stop() {
        stopped = true
        session?.invalidateAndCancel()
        session = nil
        buffer.removeAll()
    }

    private func connect() {
        guard !stopped, let s = session, let url = URL(string: kBase + "/api/notify/stream") else { return }
        var r = URLRequest(url: url)
        r.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        if !lastId.isEmpty { r.setValue(lastId, forHTTPHeaderField: "Last-Event-ID") }
        if !kToken.isEmpty { r.setValue("Bearer " + kToken, forHTTPHeaderField: "Authorization") }
        buffer.removeAll()
        s.dataTask(with: r).resume()
    }

    func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse, completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        let ok = (response as? HTTPURLResponse)?.statusCode == 200
        if ok { delay = 2 }
        completionHandler(ok ? .allow : .cancel)
    }

    func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        buffer.append(data)
        let sep = Data("\n\n".utf8)
        while let range = buffer.range(of: sep) {
            let chunk = buffer.subdata(in: buffer.startIndex..<range.lowerBound)
            buffer.removeSubrange(buffer.startIndex..<range.upperBound)
            parse(String(decoding: chunk, as: UTF8.self))
        }
    }

    private func parse(_ event: String) {
        var data = ""
        for line in event.split(separator: "\n") {
            if line.hasPrefix("id: ") { lastId = String(line.dropFirst(4)) }
            if line.hasPrefix("data: ") { data += line.dropFirst(6) }
        }
        guard let d = data.data(using: .utf8), !d.isEmpty, let item = try? JSONDecoder().decode(NotifyItem.self, from: d) else { return }
        onItem(item)
    }

    func urlSession(_ s: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        guard !stopped, s === session else { return }
        let wait = delay
        delay = min(60, delay * 2)
        DispatchQueue.main.asyncAfter(deadline: .now() + wait) { [weak self] in self?.connect() }
    }
}

// Notifier posts stream items as banners and opens the thread in the app on click.
// The stream stays open with banners off, because every item also refreshes the Dock badge.
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    static let defaultsKey = "ShowNotifications"
    static let approvalCategory = "AM_APPROVAL"
    private var stream: NotifyStream?
    private var authorizing = false
    // open shows a daemon path or URL in the app; currentPath is the key window's page; changed fires on every stream item.
    var open: (String) -> Void = { _ in }
    var currentPath: () -> String? = { nil }
    var changed: () -> Void = {}

    // UNUserNotificationCenter needs a bundle id; an unbundled binary would crash.
    private var available: Bool { Bundle.main.bundleIdentifier != nil }

    var enabled: Bool {
        get { UserDefaults.standard.bool(forKey: Notifier.defaultsKey) }
        set {
            UserDefaults.standard.set(newValue, forKey: Notifier.defaultsKey)
            if newValue { authorize() } else if available { UNUserNotificationCenter.current().removeAllDeliveredNotifications() }
        }
    }

    override init() {
        UserDefaults.standard.register(defaults: [Notifier.defaultsKey: true])
        super.init()
    }

    func start() {
        authorize()
        guard stream == nil else { return }
        stream = NotifyStream { [weak self] item in
            self?.changed()
            self?.post(item)
        }
        stream?.start()
    }

    func stop() {
        stream?.stop()
        stream = nil
        if available { UNUserNotificationCenter.current().removeAllDeliveredNotifications() }
    }

    private func authorize() {
        guard available, enabled, !authorizing else { return }
        authorizing = true
        let c = UNUserNotificationCenter.current()
        c.delegate = self
        let allow = UNNotificationAction(identifier: "ALLOW", title: "Allow", options: [.authenticationRequired])
        let deny = UNNotificationAction(identifier: "DENY", title: "Deny", options: [.destructive])
        c.setNotificationCategories([UNNotificationCategory(identifier: Notifier.approvalCategory, actions: [allow, deny], intentIdentifiers: [], options: [])])
        c.requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }

    private func link(_ n: NotifyItem) -> String {
        if let t = n.threadId, !t.isEmpty { return "/w/" + (t.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? t) }
        if let u = n.url, !u.isEmpty { return u }
        return "/notifications"
    }

    private func post(_ n: NotifyItem) {
        guard available, enabled else { return }
        let id = "n-" + n.id
        if n.resolved == true {
            UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: [id])
            return
        }
        if n.quiet == true { return }
        let c = UNMutableNotificationContent()
        c.title = n.title
        c.body = n.body ?? ""
        c.sound = n.kind == "finished" ? nil : .default
        c.threadIdentifier = n.threadId ?? n.kind
        c.userInfo = ["link": link(n)]
        let show = { UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: c, trigger: nil)) }
        // The stream item carries no approval id, so a single-thread needs-you alert looks it up by thread.
        guard n.kind == "needs-you", (n.count ?? 1) <= 1, let thread = n.threadId, !thread.isEmpty else { return show() }
        permissionApproval(thread) { approval in
            if let a = approval {
                c.categoryIdentifier = Notifier.approvalCategory
                c.userInfo["approval"] = a
            }
            show()
        }
    }

    // permissionApproval finds the one pending permission request of a thread; a question, a plan or two requests give nil.
    private func permissionApproval(_ thread: String, _ done: @escaping (String?) -> Void) {
        guard let u = URL(string: kBase + "/api/approvals") else { return done(nil) }
        var r = URLRequest(url: u, timeoutInterval: 5)
        authorized(&r)
        URLSession.shared.dataTask(with: r) { d, _, _ in
            let list = d.flatMap { try? JSONDecoder().decode(PendingApprovals.self, from: $0) }?.pending ?? []
            let mine = list.filter { thread == $0.sessionId || thread == $0.drivenId || thread == $0.rootId }
            let id = mine.count == 1 && (mine[0].kind ?? "permission") == "permission" ? mine[0].id : nil
            DispatchQueue.main.async { done(id) }
        }.resume()
    }

    // The banner's Allow asks for Touch ID too, like an Allow in the window.
    private func confirmThenAllow(_ id: String) {
        guard TouchIDGate.enabled else { return decide(id, "allow") }
        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else { return decide(id, "allow") }
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: "approve an agent request") { [weak self] ok, _ in
            DispatchQueue.main.async { if ok { self?.decide(id, "allow") } }
        }
    }

    private func decide(_ id: String, _ behavior: String) {
        guard let url = URL(string: kBase + "/api/approvals/\(id)/decide") else { return }
        var r = URLRequest(url: url, timeoutInterval: 10)
        r.httpMethod = "POST"
        r.setValue("application/json", forHTTPHeaderField: "Content-Type")
        authorized(&r)
        var body: [String: Any] = ["behavior": behavior, "surface": "mac"]
        if behavior == "deny" { body["message"] = "Denied from a notification." }
        r.httpBody = try? JSONSerialization.data(withJSONObject: body)
        URLSession.shared.dataTask(with: r) { [weak self] _, resp, _ in
            let ok = (resp as? HTTPURLResponse).map { (200..<300).contains($0.statusCode) } ?? false
            DispatchQueue.main.async {
                if !ok { NSSound.beep() }
                self?.changed()
            }
        }.resume()
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse, withCompletionHandler done: @escaping () -> Void) {
        let info = response.notification.request.content.userInfo
        let link = info["link"] as? String ?? "/notifications"
        let approval = info["approval"] as? String ?? ""
        DispatchQueue.main.async {
            switch response.actionIdentifier {
            case "ALLOW" where !approval.isEmpty: self.confirmThenAllow(approval)
            case "DENY" where !approval.isEmpty: self.decide(approval, "deny")
            default: self.open(link)
            }
        }
        done()
    }

    // In the foreground, a banner for the thread already on screen is noise.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent n: UNNotification, withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
        let link = n.request.content.userInfo["link"] as? String
        DispatchQueue.main.async {
            let onScreen = NSApp.isActive && link != nil && self.currentPath() == link
            done(onScreen ? [] : [.banner, .list, .sound])
        }
    }
}
