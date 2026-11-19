import Foundation
import Security

/// Central place for the daemon endpoint; the default comes from the build setting AM_DEFAULT_SERVER, editable in Settings.
enum Config {
    static let defaultServer: String = {
        let v = (Bundle.main.object(forInfoDictionaryKey: "AMDefaultServer") as? String ?? "").trimmingCharacters(in: .whitespaces)
        return v.isEmpty || v.hasPrefix("$(") ? "http://127.0.0.1:7777" : v
    }()

    /// A paired device token (amd_...) or the password, entered in Settings; sent as a Bearer header.
    static let defaultPassword = ""

    static var serverURL: String {
        get { UserDefaults.standard.string(forKey: "serverURL") ?? defaultServer }
        set { UserDefaults.standard.set(newValue, forKey: "serverURL") }
    }

    /// Kept in the Keychain; a value left in UserDefaults by an older build moves there once.
    static var password: String {
        get {
            passwordLock.lock(); defer { passwordLock.unlock() }
            if let c = cachedPassword { return c }
            let v = Keychain.read(passwordAccount) ?? migrateLegacyPassword() ?? defaultPassword
            // A read before first unlock fails; leave the cache empty so the next access retries.
            cachedPassword = v.isEmpty ? nil : v
            return v
        }
        set {
            passwordLock.lock(); defer { passwordLock.unlock() }
            if newValue.isEmpty { Keychain.delete(passwordAccount) } else { Keychain.write(passwordAccount, newValue) }
            cachedPassword = newValue.isEmpty ? nil : newValue
        }
    }

    private static let passwordAccount = "serverPassword"
    private static let passwordLock = NSLock()
    private static var cachedPassword: String?

    private static func migrateLegacyPassword() -> String? {
        guard let old = UserDefaults.standard.string(forKey: passwordAccount) else { return nil }
        if old.isEmpty || Keychain.write(passwordAccount, old) {
            UserDefaults.standard.removeObject(forKey: passwordAccount)
        }
        return old
    }

    /// host:port of an address, lowercased, to tell whether a new server URL points somewhere else.
    static func hostKey(_ s: String) -> String {
        guard let u = URL(string: s.trimmingCharacters(in: .whitespaces)), let h = u.host?.lowercased() else {
            return s.trimmingCharacters(in: .whitespaces).lowercased()
        }
        return u.port.map { "\(h):\($0)" } ?? h
    }

    /// `Authorization` header value, or nil when no password is configured.
    static var authHeader: String? {
        let pw = password.trimmingCharacters(in: .whitespaces)
        return pw.isEmpty ? nil : "Bearer \(pw)"
    }

    /// Base REST URL, normalised (no trailing slash).
    static var baseURL: URL? {
        var s = serverURL.trimmingCharacters(in: .whitespaces)
        if s.hasSuffix("/") { s.removeLast() }
        return URL(string: s)
    }

    /// WebSocket URL derived from the base (http→ws, https→wss) + /ws.
    static var wsURL: URL? {
        guard var comps = URLComponents(string: serverURL.trimmingCharacters(in: .whitespaces)) else { return nil }
        comps.scheme = (comps.scheme == "https") ? "wss" : "ws"
        comps.path = "/ws"
        comps.query = nil
        return comps.url
    }
}

/// Generic-password items for this app, readable after first unlock and never synced or restored to another device.
enum Keychain {
    private static let service = Bundle.main.bundleIdentifier ?? "agent-monitor"

    private static func query(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account]
    }

    static func read(_ account: String) -> String? {
        var q = query(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data else { return nil }
        return String(data: d, encoding: .utf8)
    }

    @discardableResult
    static func write(_ account: String, _ value: String) -> Bool {
        let data = Data(value.utf8)
        let attrs: [String: Any] = [kSecValueData as String: data,
                                    kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly]
        let st = SecItemUpdate(query(account) as CFDictionary, attrs as CFDictionary)
        if st == errSecSuccess { return true }
        guard st == errSecItemNotFound else { return false }
        var add = query(account)
        add.merge(attrs) { _, b in b }
        return SecItemAdd(add as CFDictionary, nil) == errSecSuccess
    }

    static func delete(_ account: String) {
        SecItemDelete(query(account) as CFDictionary)
    }
}

// Pair links from Settings, Devices on the Mac: {base}/pair#<code>, where the code is single-use for 5 minutes.
enum PairLink {
    enum Parsed: Equatable {
        case token(String, base: String?)
        case code(String, base: String)
    }

    struct Failure: LocalizedError {
        let message: String
        var errorDescription: String? { message }
    }

    /// Reads a pasted pair link, or a bare amd_ token.
    static func parse(_ raw: String) throws -> Parsed {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.hasPrefix("amd_"), !s.contains(where: \.isWhitespace) { return .token(s, base: nil) }
        guard let c = URLComponents(string: s), let scheme = c.scheme?.lowercased(),
              scheme == "http" || scheme == "https", let host = c.host, !host.isEmpty else {
            throw Failure(message: "Paste the whole pair link (http://.../pair#...) or a device token that starts with amd_.")
        }
        let base = "\(scheme)://\(host.contains(":") && !host.hasPrefix("[") ? "[\(host)]" : host)" + (c.port.map { ":\($0)" } ?? "")
        var value = ""
        if let frag = c.fragment, !frag.isEmpty {
            value = keyed(frag) ?? frag
        } else if let q = c.queryItems?.first(where: { $0.name == "code" || $0.name == "token" })?.value {
            value = q
        }
        value = (value.removingPercentEncoding ?? value).trimmingCharacters(in: .whitespacesAndNewlines)
        if value.isEmpty { throw Failure(message: "This link has no pair code. Copy the full pair link from Settings, Devices on the Mac.") }
        return value.hasPrefix("amd_") ? .token(value, base: base) : .code(value, base: base)
    }

    private static func keyed(_ frag: String) -> String? {
        for part in frag.split(separator: "&") {
            let kv = part.split(separator: "=", maxSplits: 1).map(String.init)
            if kv.count == 2, kv[0] == "code" || kv[0] == "token" { return kv[1] }
        }
        return nil
    }

    /// Shows only the amd_ prefix and the last 4 characters of a saved token.
    static func mask(_ token: String) -> String {
        let t = token.trimmingCharacters(in: .whitespaces)
        if t.isEmpty { return "Not set" }
        if t.hasPrefix("amd_") { return "amd_…" + String(t.suffix(4)) }
        return "Password set"
    }
}
