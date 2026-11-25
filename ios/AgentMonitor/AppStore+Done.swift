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
    }
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
}
