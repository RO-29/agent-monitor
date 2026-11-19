import SwiftUI

/// The agent's terminal as it is now: the last screen lines, refreshed every second, with keys and typing.
struct TerminalScreen: View {
    let sessionID: String
    @Environment(\.dismiss) private var dismiss
    @State private var screen = ""
    @State private var error: String?
    @State private var input = ""
    @State private var busy = false

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                ScrollViewReader { proxy in
                    ScrollView([.vertical, .horizontal]) {
                        Text(screen.isEmpty ? "…" : screen)
                            .font(.system(size: 11.5, design: .monospaced))
                            .foregroundStyle(Tok.fg)
                            .textSelection(.enabled)
                            .fixedSize(horizontal: true, vertical: false)
                            .padding(10)
                        Color.clear.frame(height: 1).id("end")
                    }
                    .background(Tok.panel2)
                    .onChange(of: screen) { _, _ in proxy.scrollTo("end", anchor: .bottom) }
                }
                if let error { Text(error).font(.footnote).foregroundStyle(Tok.err).padding(.horizontal, 12).padding(.top, 6) }
                keyRow
                HStack(spacing: 8) {
                    TextField("Type into the terminal", text: $input, axis: .vertical)
                        .font(.system(size: 14, design: .monospaced)).lineLimit(1...3).amField()
                    Button("Type") { Task { await type(enter: false) } }.buttonStyle(.am(.ghost)).disabled(input.isEmpty || busy)
                    Button { Task { await type(enter: true) } } label: { Image(systemName: "return") }
                        .buttonStyle(.am()).disabled(busy).accessibilityLabel("Type and press Enter")
                }
                .padding(10)
            }
            .navigationTitle("Terminal")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
            .task { await poll() }
        }
    }

    private var keyRow: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 6) {
                ForEach([("Esc", "Escape"), ("↑", "Up"), ("↓", "Down"), ("Tab", "Tab"), ("Enter", "Enter"), ("Ctrl-C", "C-c"), ("⌫", "BSpace")], id: \.1) { label, key in
                    Button(label) { Task { await press([key]) } }.font(.system(size: 13, weight: .semibold, design: .monospaced))
                        .buttonStyle(.am(.ghost)).disabled(busy)
                }
            }
            .padding(.horizontal, 10).padding(.top, 8)
        }
    }

    private func poll() async {
        while !Task.isCancelled {
            do {
                let v = try await APIClient.shared.paneView(sessionID, lines: 60)
                if v.ok { screen = v.content ?? ""; error = nil } else { error = "agent-monitor cannot read this terminal" }
            } catch { self.error = error.localizedDescription }
            try? await Task.sleep(nanoseconds: 1_000_000_000)
        }
    }

    private func press(_ keys: [String]) async {
        busy = true; defer { busy = false }
        do { try await APIClient.shared.keys(sessionID, keys); error = nil } catch { self.error = error.localizedDescription }
    }

    private func type(enter: Bool) async {
        busy = true; defer { busy = false }
        do { try await APIClient.shared.send(sessionID, text: input, enter: enter); input = ""; error = nil }
        catch { self.error = error.localizedDescription }
    }
}
