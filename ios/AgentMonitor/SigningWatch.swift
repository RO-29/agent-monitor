import BackgroundTasks
import Foundation

/// Once a day reads the app's own signing profile end; inside 3 days it asks the build Mac for a new build
/// every 40 min until an install with a later end arrives, and shows one notification per end date.
@MainActor
enum SigningWatch {
    nonisolated static let taskID = "com.rohit.agentmonitor.signing"
    static let renewWithin: TimeInterval = 3 * 86400
    static let checkEvery: TimeInterval = 86400
    static let askEvery: TimeInterval = 40 * 60

    private static let store = UserDefaults.standard
    private static let kChecked = "signing.checkedAt"
    private static let kPending = "signing.pendingEnd"
    private static let kAsked = "signing.askedAt"
    private static let kNotified = "signing.notifiedEnd"

    /// The end of the profile this build carries; nil in the simulator, which has none.
    static let profileEnd: Date? = {
        guard let url = Bundle.main.url(forResource: "embedded", withExtension: "mobileprovision"),
              let data = try? Data(contentsOf: url),
              let a = data.range(of: Data("<?xml".utf8)),
              let b = data.range(of: Data("</plist>".utf8), in: a.lowerBound..<data.endIndex),
              let plist = try? PropertyListSerialization.propertyList(from: data[a.lowerBound..<b.upperBound], format: nil) as? [String: Any]
        else { return nil }
        return plist["ExpirationDate"] as? Date
    }()

    /// Must run before launch ends; iOS then wakes the app for `taskID` when it decides to.
    nonisolated static func register() {
        BGTaskScheduler.shared.register(forTaskWithIdentifier: taskID, using: nil) { task in
            nonisolated(unsafe) let task = task
            Task { @MainActor in
                let ok = await tick()
                task.setTaskCompleted(success: ok)
            }
        }
    }

    static func schedule() {
        let req = BGAppRefreshTaskRequest(identifier: taskID)
        req.earliestBeginDate = Date().addingTimeInterval(store.double(forKey: kPending) > 0 ? askEvery : checkEvery)
        try? BGTaskScheduler.shared.submit(req)
    }

    /// Safe to call often: each step keeps its own interval.
    @discardableResult
    static func tick(now: Date = Date()) async -> Bool {
        defer { schedule() }
        guard let end = profileEnd else { return true }
        let pending = store.double(forKey: kPending)
        if pending > 0 {
            guard end.timeIntervalSince1970 <= pending + 60 else {
                store.removeObject(forKey: kPending)
                store.removeObject(forKey: kAsked)
                return true
            }
        } else {
            guard now.timeIntervalSince1970 - store.double(forKey: kChecked) >= checkEvery else { return true }
            store.set(now.timeIntervalSince1970, forKey: kChecked)
            guard end.timeIntervalSince(now) < renewWithin else { return true }
            store.set(end.timeIntervalSince1970, forKey: kPending)
        }
        guard now.timeIntervalSince1970 - store.double(forKey: kAsked) >= askEvery else { return true }
        store.set(now.timeIntervalSince1970, forKey: kAsked)
        let ms = Int64(end.timeIntervalSince1970 * 1000)
        let sent = (try? await APIClient.shared.post("/api/ios/renew", body: ["expires": ms])) != nil
        if store.double(forKey: kNotified) != end.timeIntervalSince1970 {
            store.set(end.timeIntervalSince1970, forKey: kNotified)
            let when = end.formatted(.dateTime.weekday(.abbreviated).hour().minute())
            Notifier.shared.signing(
                title: "Agent Monitor stops opening \(when)",
                body: sent ? "The build Mac will install a new build when this iPhone is on its Wi-Fi or USB."
                           : "The build Mac did not answer. The app asks again every 40 min.")
        }
        return sent
    }
}
