import Foundation

/// Another Mac that runs the daemon. Its address lives in UserDefaults; its token lives in the Keychain.
struct Machine: Codable, Identifiable, Hashable {
    var id: String
    var name: String
    var url: String

    var baseURL: URL? {
        var s = url.trimmingCharacters(in: .whitespaces)
        if s.hasSuffix("/") { s.removeLast() }
        return URL(string: s)
    }
    var token: String? { Keychain.read(Machines.account(id)) }
    var client: APIClient { APIClient(machine: self) }
}

enum Machines {
    private static let key = "machines"

    static func account(_ id: String) -> String { "machine:\(id)" }

    static var all: [Machine] {
        guard let d = UserDefaults.standard.data(forKey: key) else { return [] }
        return (try? JSONDecoder().decode([Machine].self, from: d)) ?? []
    }

    private static func save(_ list: [Machine]) {
        UserDefaults.standard.set(try? JSONEncoder().encode(list), forKey: key)
    }

    /// Pairs with a link or a token; a code is traded for the token at that machine and never stored.
    static func add(name: String, link: String) async throws -> Machine {
        let parsed = try PairLink.parse(link)
        let base: String, token: String
        switch parsed {
        case .code(let code, let b):
            guard let u = URL(string: b) else { throw APIError(message: "Bad machine address") }
            token = try await APIClient.redeemPairCode(code, base: u)
            base = b
        case .token(let t, let b):
            guard let b else { throw APIError(message: "Paste the whole pair link: a bare token does not say which machine it is for.") }
            token = t
            base = b
        }
        if Config.hostKey(base) == Config.hostKey(Config.serverURL) {
            throw APIError(message: "This is the main server already.")
        }
        var list = all.filter { Config.hostKey($0.url) != Config.hostKey(base) }
        let n = name.trimmingCharacters(in: .whitespaces)
        let m = Machine(id: UUID().uuidString, name: n.isEmpty ? (URL(string: base)?.host ?? base) : n, url: base)
        guard Keychain.write(account(m.id), token) else { throw APIError(message: "The Keychain did not keep the token.") }
        for old in all where !list.contains(old) { Keychain.delete(account(old.id)) }
        list.append(m)
        save(list)
        return m
    }

    static func remove(_ id: String) {
        Keychain.delete(account(id))
        save(all.filter { $0.id != id })
    }

    static func rename(_ id: String, to name: String) {
        save(all.map { $0.id == id ? Machine(id: $0.id, name: name, url: $0.url) : $0 })
    }
}
