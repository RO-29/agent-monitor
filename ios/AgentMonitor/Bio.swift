import CryptoKit
import Foundation
import LocalAuthentication

/// Face ID for approvals: a Secure Enclave key that signs only after Face ID. The daemon holds the public key
/// and refuses approvals and admin changes from this phone without a fresh signature (HTTP 428, "bio": true).
enum Bio {
    private static let blobKey = "bioKeyBlob"
    private static let onKey = "bioApprovals"

    static var available: Bool {
        var err: NSError?
        return SecureEnclave.isAvailable && LAContext().canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: &err)
    }
    /// True after the daemon accepted this phone's key.
    /// True while a Face ID sheet shows, so the app lock cover stays away from it.
    @MainActor static var prompting = false

    static var enabled: Bool { UserDefaults.standard.bool(forKey: onKey) && UserDefaults.standard.data(forKey: blobKey) != nil }

    private static func key(context: LAContext? = nil) throws -> SecureEnclave.P256.Signing.PrivateKey {
        guard let blob = UserDefaults.standard.data(forKey: blobKey) else { throw APIError(message: "No Face ID key on this phone. Turn on Face ID for approvals in Settings.") }
        return try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: blob, authenticationContext: context)
    }

    /// Makes a new key (no Face ID prompt yet) and returns its public key for the daemon.
    static func newKey() throws -> String {
        guard let ac = SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, [.privateKeyUsage, .biometryCurrentSet], nil) else {
            throw APIError(message: "Could not set up the Face ID key")
        }
        let k = try SecureEnclave.P256.Signing.PrivateKey(accessControl: ac)
        UserDefaults.standard.set(k.dataRepresentation, forKey: blobKey)
        return k.publicKey.x963Representation.base64EncodedString()
    }

    static func forget() {
        UserDefaults.standard.removeObject(forKey: blobKey)
        UserDefaults.standard.set(false, forKey: onKey)
    }

    static func markEnabled() { UserDefaults.standard.set(true, forKey: onKey) }

    /// The X-AM-Bio header for this request; Face ID shows here. The message matches bioMessage in the daemon.
    static func header(for req: URLRequest, reason: String) async throws -> String {
        let ms = Int64(Date().timeIntervalSince1970 * 1000)
        let path = req.url.flatMap { URLComponents(url: $0, resolvingAgainstBaseURL: false)?.path } ?? ""
        let body = req.httpBody ?? Data()
        let hash = SHA256.hash(data: body).map { String(format: "%02x", $0) }.joined()
        let msg = "agent-monitor-bio\n\(req.httpMethod ?? "GET")\n\(path)\n\(ms)\n\(hash)"
        let ctx = LAContext()
        ctx.localizedReason = reason
        await MainActor.run { prompting = true }
        defer { Task { @MainActor in prompting = false } }
        let sig: P256.Signing.ECDSASignature
        do {
            sig = try await Task.detached { try key(context: ctx).signature(for: Data(msg.utf8)) }.value
        } catch let e as LAError where e.code == .userCancel || e.code == .appCancel || e.code == .systemCancel {
            throw APIError(message: "Face ID cancelled. Nothing was sent.")
        } catch {
            if (error as NSError).code == -128 { throw APIError(message: "Face ID cancelled. Nothing was sent.") }
            throw error
        }
        return "\(ms).\(sig.derRepresentation.base64EncodedString())"
    }

    /// A readable reason for the Face ID prompt.
    static func reason(for req: URLRequest) -> String {
        let p = req.url?.path ?? ""
        if p.hasPrefix("/api/secrets/") { return "Give a secret to an agent" }
        if p.hasSuffix("/decide") || p.hasSuffix("/respond") || p.hasSuffix("/answer") || p.contains("/keys/") || p.contains("/send/") { return "Approve an agent request" }
        return "Change a setting on the Mac"
    }
}
