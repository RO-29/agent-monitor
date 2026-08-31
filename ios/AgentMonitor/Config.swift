import Foundation

/// Central place for the daemon endpoint. Defaults to the machine's Tailscale
/// IP so the app works over the tailnet out of the box; editable in Settings.
enum Config {
    static let defaultServer = "http://100.68.125.93:7777"

    /// Password the daemon requires for remote (non-loopback) access. Pre-seeded
    /// with the current daemon password so the app connects out of the box;
    /// editable in Settings. Sent as `Authorization: Bearer <password>` on every
    /// REST call and on the WebSocket handshake.
    static let defaultPassword = "ly9ba4-3mmv0g"

    static var serverURL: String {
        get { UserDefaults.standard.string(forKey: "serverURL") ?? defaultServer }
        set { UserDefaults.standard.set(newValue, forKey: "serverURL") }
    }

    static var password: String {
        get { UserDefaults.standard.string(forKey: "serverPassword") ?? defaultPassword }
        set { UserDefaults.standard.set(newValue, forKey: "serverPassword") }
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
