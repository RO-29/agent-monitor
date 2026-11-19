import SwiftUI

/// Drives the tmux pane behind a session: live capture + text/keys/cancel.
struct PaneBridgeView: View {
    let session: Session
    @Environment(\.dismiss) private var dismiss

    @State private var capture = "…"
    @State private var command = ""
    @State private var input = ""
    @State private var enter = true
    @State private var status: String?
    @State private var error: String?
    @State private var live = true

    // Quick keys mapped to tmux key names.
    private let quickKeys: [(String, [String])] = [
        ("Enter", ["Enter"]), ("Esc", ["Escape"]),
        ("Y", ["y"]), ("N", ["n"]),
        ("1", ["1"]), ("2", ["2"]), ("3", ["3"]),
        ("↑", ["Up"]), ("↓", ["Down"]),
        ("Tab", ["Tab"]), ("⌫", ["BSpace"]),
    ]

    var body: some View {
        NavigationStack {
            VStack(spacing: 12) {
                if let error { ErrorBanner(message: error) }
                if let status {
                    Text(status).font(Typo.xs).foregroundStyle(Tok.muted)
                }

                CodeBlock(text: capture)
                    .frame(maxHeight: .infinity)

                quickKeyBar

                HStack(spacing: 8) {
                    TextField("Send text to the pane…", text: $input, axis: .vertical)
                        .amField()
                        .lineLimit(1...4)
                    Button {
                        Task { await sendText() }
                    } label: { Image(systemName: "paperplane.fill") }
                        .buttonStyle(.am(.pri))
                        .disabled(input.isEmpty)
                }

                Toggle("Press Enter after sending", isOn: $enter)
                    .tint(Tok.ok)
                    .font(Typo.sm)

                Button(role: .destructive) {
                    Task { await cancel() }
                } label: {
                    Label("Send Ctrl-C (cancel)", systemImage: "xmark.octagon")
                }
                .buttonStyle(.am(.deny, big: true, full: true))
            }
            .padding(14)
            .background(Tok.panel)
            .navigationTitle(session.project)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button("Done") { live = false; dismiss() }
                }
                ToolbarItem(placement: .topBarTrailing) {
                    if !command.isEmpty {
                        Text(command).font(Typo.mono(11)).foregroundStyle(Tok.muted)
                    }
                }
            }
            .task { await pollLoop() }
            .onDisappear { live = false }
        }
    }

    private var quickKeyBar: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(quickKeys, id: \.0) { key in
                    Button(key.0) { Task { await sendKeys(key.1) } }
                        .font(Typo.mono(13))
                        .buttonStyle(.am())
                }
            }
        }
    }

    // MARK: Actions

    private func sendText() async {
        do {
            try await APIClient.shared.send(session.id, text: input, enter: enter)
            status = "Sent"; input = ""; error = nil
            await refresh()
        } catch { self.error = error.localizedDescription }
    }

    private func sendKeys(_ keys: [String]) async {
        do {
            try await APIClient.shared.keys(session.id, keys)
            status = "Keys: \(keys.joined(separator: " "))"; error = nil
            await refresh()
        } catch { self.error = error.localizedDescription }
    }

    private func cancel() async {
        do {
            try await APIClient.shared.cancel(session.id)
            status = "Sent Ctrl-C"; error = nil
            await refresh()
        } catch { self.error = error.localizedDescription }
    }

    private func pollLoop() async {
        while live && !Task.isCancelled {
            await refresh()
            try? await Task.sleep(for: .seconds(1.5))
        }
    }

    private func refresh() async {
        do {
            let v = try await APIClient.shared.paneView(session.id, lines: 45)
            if v.ok {
                capture = v.content ?? ""
                command = v.command ?? ""
                error = nil
            } else {
                error = v.error ?? "No pane registered for this session"
            }
        } catch { self.error = error.localizedDescription }
    }
}
