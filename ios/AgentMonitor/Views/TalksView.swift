import SwiftUI

struct TalksView: View {
    @Environment(AppStore.self) private var store
    @State private var error: String?
    @State private var working: Set<String> = []
    @State private var composing = false

    var body: some View {
        NavigationStack {
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
                        .padding()
                    }
                }
            }
            .navigationTitle("Talks")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { ConnectionChip() }
                ToolbarItem(placement: .topBarTrailing) {
                    Button { composing = true } label: { Image(systemName: "square.and.pencil") }
                        .disabled(store.panes.isEmpty)
                }
            }
            .sheet(isPresented: $composing) { ComposeTalkView() }
            .refreshable { await store.hydrate() }
        }
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
                    .font(.caption.weight(.semibold))
                Spacer()
                statusPill
            }
            Text(talk.message).font(.subheadline).textSelection(.enabled)

            if let reply = talk.reply, !reply.isEmpty {
                Text("Reply: \(reply)")
                    .font(.caption).foregroundStyle(.secondary)
                    .padding(8)
                    .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
            }
            if let reason = talk.reason, !reason.isEmpty {
                Text(reason).font(.caption2).foregroundStyle(.red)
            }

            HStack {
                Text(relativeTime(talk.createdAt)).font(.caption2).foregroundStyle(.tertiary)
                Spacer()
                if talk.isPending {
                    Button("Deny") { Task { await onRespond(false) } }
                        .buttonStyle(.bordered).tint(.red).controlSize(.small)
                    Button("Allow") { Task { await onRespond(true) } }
                        .buttonStyle(.borderedProminent).tint(.green).controlSize(.small)
                }
            }
            .disabled(working)
        }
        .padding()
        .background(Color(.systemBackground), in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(Color(.separator).opacity(0.4)))
    }

    private var statusPill: some View {
        Text(talk.status)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 8).padding(.vertical, 3)
            .background(statusColor.opacity(0.15), in: Capsule())
            .foregroundStyle(statusColor)
    }
    private var statusColor: Color {
        switch talk.status {
        case "pending", "pending_reply": return .orange
        case "delivered", "replied": return .green
        case "denied", "timeout", "error": return .red
        default: return .secondary
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
                    Text(error).font(.caption).foregroundStyle(.red)
                }
            }
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
