import SwiftUI

struct TalksView: View {
    @Environment(AppStore.self) private var store
    @State private var error: String?
    @State private var working: Set<String> = []
    @State private var composing = false

    var body: some View {
        Group {
                if store.talks.isEmpty {
                    EmptyHint(icon: "bubble.left.and.bubble.right",
                              title: "No talks",
                              subtitle: "Inter-agent messages and requests to reach your agents show up here.")
                } else {
                    ScrollView {
                        if let error { ErrorBanner(message: error) }
                        VStack(spacing: 12) {
                            ForEach(store.talks) { talk in
                                TalkCard(
                                    talk: talk,
                                    working: working.contains(talk.id),
                                    onRespond: { allow in await respond(talk, allow: allow) }
                                )
                            }
                        }
                        .padding(.horizontal, 12)
                        .padding(.vertical, 14)
                    }
                }
            }
            .background(Tok.bg)
            .navigationTitle("Talks")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { composing = true } label: { Image(systemName: "square.and.pencil") }
                        .disabled(store.panes.isEmpty)
                }
            }
            .sheet(isPresented: $composing) { ComposeTalkView() }
            .refreshable { await store.hydrate() }
    }

    private func respond(_ talk: Talk, allow: Bool) async {
        working.insert(talk.id); defer { working.remove(talk.id) }
        do {
            try await APIClient.shared.respondTalk(talk.id, allow: allow)
            error = nil
        } catch { self.error = error.localizedDescription }
    }
}

struct TalkCard: View {
    let talk: Talk
    let working: Bool
    let onRespond: (Bool) async -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("\(talk.fromLabel) → \(talk.toLabel ?? talk.toAgent)")
                    .font(Typo.ui(12, .semibold)).foregroundStyle(Tok.fg2)
                Spacer()
                statusPill
            }
            Text(talk.message).font(Typo.prose).foregroundStyle(Tok.fg).lineSpacing(3).textSelection(.enabled)

            if let reply = talk.reply, !reply.isEmpty {
                Text("Reply: \(reply)")
                    .font(Typo.sm).foregroundStyle(Tok.muted)
                    .padding(8)
                    .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 8))
            }
            if let reason = talk.reason, !reason.isEmpty {
                Text(reason).font(Typo.xs).foregroundStyle(Tok.err)
            }

            HStack {
                Text(relativeTime(talk.createdAt)).font(Typo.xs).foregroundStyle(Tok.muted)
                Spacer()
                if talk.isPending {
                    Button("Deny") { Task { await onRespond(false) } }
                        .buttonStyle(.am(.deny)).controlSize(.small)
                    Button("Allow") { Task { await onRespond(true) } }
                        .buttonStyle(.am(.allow)).controlSize(.small)
                }
            }
            .disabled(working)
        }
        .amCard(padding: 14)
    }

    private var statusPill: some View {
        Pill(text: talk.status.replacingOccurrences(of: "_", with: " "), color: statusColor)
    }
    private var statusColor: Color {
        switch talk.status {
        case "pending", "pending_reply": return Tok.need
        case "delivered", "replied": return Tok.ok
        case "denied", "timeout", "error": return Tok.err
        default: return Tok.muted
        }
    }
}

struct ComposeTalkView: View {
    @Environment(AppStore.self) private var store
    @Environment(\.dismiss) private var dismiss
    @State private var recipient = ""
    @State private var message = ""
    @State private var error: String?
    @State private var sending = false

    var body: some View {
        NavigationStack {
            Form {
                Section("Recipient") {
                    Picker("Agent", selection: $recipient) {
                        Text("Select…").tag("")
                        ForEach(store.panes) { p in
                            Text("\(p.displayName) · \(p.paneId)").tag(p.agentId)
                        }
                    }
                }
                Section("Message") {
                    TextField("Message", text: $message, axis: .vertical).lineLimit(3...8)
                }
                if let error {
                    Text(error).font(Typo.sm).foregroundStyle(Tok.err)
                }
            }
            .amPage()
            .navigationTitle("New Talk")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Send") { Task { await send() } }
                        .disabled(recipient.isEmpty || message.isEmpty || sending)
                }
            }
        }
    }

    private func send() async {
        sending = true; defer { sending = false }
        do {
            try await APIClient.shared.requestTalk(to: recipient, message: message)
            dismiss()
        } catch { self.error = error.localizedDescription }
    }
}
