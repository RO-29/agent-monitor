import LocalAuthentication
import SwiftUI

let kAppLockKey = "faceIDLock"

/// Covers the app until Face ID (or the passcode) unlocks it; the cover also hides the app in the app switcher.
struct AppLockGate: ViewModifier {
    @AppStorage(kAppLockKey) private var lockOn = false
    @Environment(\.scenePhase) private var phase
    @State private var locked = UserDefaults.standard.bool(forKey: kAppLockKey)
    @State private var trying = false

    func body(content: Content) -> some View {
        content
            .overlay {
                // The Face ID sheet makes the scene inactive; the cover must not flash over it.
                if lockOn && (locked || (phase != .active && !Bio.prompting)) {
                    LockCover(locked: locked, unlock: unlock).transition(.opacity)
                }
            }
            .onChange(of: phase) { _, p in
                if p == .background { locked = true }
                if p == .active, locked, lockOn { unlock() }
            }
            .onChange(of: lockOn) { _, on in if !on { locked = false } }
            .task { if lockOn, locked, phase == .active { unlock() } }
    }

    private func unlock() {
        guard lockOn, !trying else { return }
        trying = true
        Bio.prompting = true
        let ctx = LAContext()
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: "Unlock Agent Monitor") { ok, _ in
            DispatchQueue.main.async {
                trying = false
                Bio.prompting = false
                if ok { withAnimation(.snappy) { locked = false } }
            }
        }
    }
}

private struct LockCover: View {
    let locked: Bool
    let unlock: () -> Void

    var body: some View {
        ZStack {
            Rectangle().fill(.ultraThickMaterial).ignoresSafeArea()
            VStack(spacing: 14) {
                Image(systemName: "lock.fill").font(.system(size: 34, weight: .semibold)).foregroundStyle(Tok.fg2)
                Text("Agent Monitor is locked").font(.headline).foregroundStyle(Tok.fg)
                if locked {
                    Button("Unlock with Face ID", action: unlock).buttonStyle(.am(.pri))
                }
            }
        }
        .accessibilityElement(children: .contain)
    }
}

extension APIClient {
    /// Makes this phone's Face ID key and gives the public key to the Mac.
    func bioEnroll() async throws {
        let pub = try Bio.newKey()
        do {
            try await post("/api/devices/me/bio-key", body: ["publicKey": pub])
            Bio.markEnabled()
            NotifyAction.register()
        } catch {
            Bio.forget()
            throw error
        }
    }

    /// Removes the key on the Mac (Face ID confirms it), then on this phone.
    func bioRemove() async throws {
        var req = URLRequest(url: try url("/api/devices/me/bio-key"))
        req.httpMethod = "DELETE"
        authorize(&req)
        let (data, resp) = try await perform(req)
        try Self.check(resp, data)
        Bio.forget()
        NotifyAction.register()
    }
}
