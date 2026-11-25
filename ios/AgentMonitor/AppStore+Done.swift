import Foundation

/// Threads the user asked to hear about when their turn ends, even with "Finished" alerts off.
enum DoneWatch {
    private static let key = "alert.doneThreads"
    static var ids: Set<String> { Set(UserDefaults.standard.stringArray(forKey: key) ?? []) }
    static func has(_ id: String) -> Bool { ids.contains(id) }
    static func set(_ id: String, _ on: Bool) {
        var s = ids
        if on { s.insert(id) } else { s.remove(id) }
        UserDefaults.standard.set(Array(s), forKey: key)
        var at = UserDefaults.standard.dictionary(forKey: atKey) ?? [:]
        at[id] = on ? Date().timeIntervalSince1970 * 1000 : nil
        UserDefaults.standard.set(at, forKey: atKey)
    }
    private static let atKey = "alert.doneThreadsAt"
    /// When the bell was armed, in ms; 0 for a bell armed before this was kept.
    static func armedAt(_ id: String) -> Double { UserDefaults.standard.dictionary(forKey: atKey)?[id] as? Double ?? 0 }
}

extension AppStore {
    /// A reply from a banner goes to the thread's newest session; a driven thread keeps its own id.
    func replyTarget(_ id: String) -> String {
        let t = thread(resolveThreadID(id))
        if let d = t?.drivenId, !d.isEmpty, !id.contains(":") { return d }
        return t?.latestSession ?? id
    }

    /// True once per armed thread: the caller alerts for its finished turn, and the bell turns off.
    func takeDoneWatch(_ threadId: String?) -> Bool {
        guard let t = threadId else { return false }
        let rid = resolveThreadID(t)
        for id in [rid, t] where DoneWatch.has(id) {
            DoneWatch.set(id, false)
            doneWatchTick += 1
            return true
        }
        return false
    }

    /// A turn that ended while the app was away turns its bell off, so a later turn does not alert for it.
    func settleDoneWatches(_ list: [AlertItem]) {
        for a in list where a.kind == "finished" {
            guard let t = a.threadId else { continue }
            for id in [resolveThreadID(t), t] where DoneWatch.has(id) && Double(a.at) > DoneWatch.armedAt(id) {
                DoneWatch.set(id, false)
                doneWatchTick += 1
            }
        }
    }
}
