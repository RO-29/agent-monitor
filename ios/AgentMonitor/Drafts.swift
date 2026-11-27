import Foundation

/// Unsent text and uploaded attachments per input box, in Application Support/drafts.json.
/// Typing is written 0.4 s after the last change and at once on background or clear; a successful send clears it.
@MainActor
final class DraftStore {
    static let shared = DraftStore()

    struct Draft: Codable, Equatable {
        var text: String
        var attachments: [OAttachment] = []
        var at: Double = Date().timeIntervalSince1970
    }

    private var map: [String: Draft] = [:]
    private let url: URL? = {
        guard let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first else { return nil }
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.appendingPathComponent("drafts.json")
    }()

    private init() {
        guard let url, let d = try? Data(contentsOf: url), let m = try? JSONDecoder().decode([String: Draft].self, from: d) else { return }
        let cutoff = Date().timeIntervalSince1970 - 30 * 86_400
        map = m.filter { $0.value.at > cutoff }
    }

    func get(_ key: String) -> Draft? { map[key] }
    func text(_ key: String) -> String { map[key]?.text ?? "" }

    func set(_ key: String, text: String, attachments: [OAttachment] = []) {
        let empty = text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && attachments.isEmpty
        if empty {
            guard map[key] != nil else { return }
            map[key] = nil
            flush()
        } else {
            if let was = map[key], was.text == text, was.attachments == attachments { return }
            map[key] = Draft(text: text, attachments: attachments)
            save()
        }
    }

    func clear(_ key: String) { set(key, text: "") }

    /// Keys starting with a prefix, for clearing every field of one question card.
    func clear(prefix: String) {
        let keys = map.keys.filter { $0.hasPrefix(prefix) }
        guard !keys.isEmpty else { return }
        for k in keys { map[k] = nil }
        save()
    }

    private var pending: Task<Void, Never>?

    private func save() {
        pending?.cancel()
        pending = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            self?.write()
        }
    }

    /// Writes now; the app calls it when it goes to the background.
    func flush() {
        pending?.cancel()
        pending = nil
        write()
    }

    private func write() {
        guard let url, let d = try? JSONEncoder().encode(map) else { return }
        try? d.write(to: url, options: .atomic)
    }
}
