import SwiftUI

/// An agent's request_secret, answered here. Face ID signs the answer when Face ID for approvals is on.
struct SecretReq: Decodable, Identifiable, Hashable {
    var id: String
    var name: String
    var reason: String?
    var url: String?
    var agent: String?
    var cwd: String?
    var createdAt: Int64
}

private struct SecretList: Decodable { var secrets: [SecretReq] }

struct SecretCardsSection: View {
    @State private var list: [SecretReq] = []

    var body: some View {
        VStack(spacing: 12) {
            ForEach(list) { s in
                SecretCard(s: s) { list.removeAll { $0.id == s.id } }
            }
        }
        .task {
            while !Task.isCancelled {
                if let r = try? await APIClient.shared.get("/api/secrets", as: SecretList.self) { list = r.secrets }
                try? await Task.sleep(for: .seconds(3))
            }
        }
    }
}

private struct SecretCard: View {
    let s: SecretReq
    let done: () -> Void
    @State private var value = ""
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label {
                Text("\(s.agent ?? ((s.cwd ?? "") as NSString).lastPathComponent) asks for a secret").font(Typo.ui(13, .semibold))
            } icon: { Image(systemName: "key.fill").foregroundStyle(Tok.need) }
            Text(s.name).font(Typo.ui(15, .semibold)).foregroundStyle(Tok.fg)
            if let r = s.reason, !r.isEmpty { Text(r).font(Typo.ui(13)).foregroundStyle(Tok.fg2) }
            if let u = s.url.flatMap(URL.init(string:)), u.scheme == "https" { Link("Get it here: \(u.host ?? "")", destination: u).font(Typo.ui(13)) }
            SecureField("Secret", text: $value)
                .textContentType(.password)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .padding(10)
                .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 8))
            HStack {
                Button(busy ? "Sending…" : "Give secret") { Task { await answer(true) } }
                    .buttonStyle(.am(.pri)).disabled(busy || value.isEmpty)
                Button("Decline") { Task { await answer(false) } }.disabled(busy)
            }
            Text("The agent gets it once, through a pipe that is gone after one read or 10 minutes. It never goes into the chat.")
                .font(Typo.ui(11.5)).foregroundStyle(Tok.muted)
            if let error { Text(error).font(Typo.ui(12.5)).foregroundStyle(Tok.err) }
        }
        .padding(14)
        .background(Tok.panel, in: RoundedRectangle(cornerRadius: 12))
    }

    private func answer(_ save: Bool) async {
        busy = true
        defer { busy = false }
        do {
            let body: [String: Any] = save ? ["type": "save", "secret": value] : ["type": "decline"]
            _ = try await APIClient.shared.post("/api/secrets/\(s.id)/answer", body: body)
            value = ""
            done()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
