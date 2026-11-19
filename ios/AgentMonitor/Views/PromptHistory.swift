import SwiftUI

/// A prompt you typed, from the agents' own history files (GET /api/prompts).
struct PromptRun: Decodable, Hashable, Identifiable {
    var text: String
    var at: Int64?
    var agent: String?
    var sessionId: String?
    var cwd: String?
    var id: String { "\(sessionId ?? "")|\(at ?? 0)|\(text)" }
}

private struct PromptList: Decodable {
    var items: [PromptRun] { lossy?.compactMap(\.value) ?? [] }
    private var lossy: [Lossy<PromptRun>]?
    private enum CodingKeys: String, CodingKey { case lossy = "items" }
}

extension APIClient {
    func prompts(session: String? = nil, query: String = "", limit: Int = 300) async throws -> [PromptRun] {
        var q = "/api/prompts?bash=no&limit=\(limit)"
        if let session { q += "&session=\(enc(session))" }
        if !query.isEmpty { q += "&q=\(enc(query))" }
        return try await get(q, as: PromptList.self).items
    }
}

/// Earlier prompts, this session's first; a tap hands the text back (onPick) or copies it.
struct PromptHistoryList: View {
    @Environment(AppStore.self) private var store
    var session: String?
    var onPick: ((String) -> Void)?

    @State private var mine: [PromptRun] = []
    @State private var all: [PromptRun] = []
    @State private var loaded = false
    @State private var error: String?
    @State private var query = ""

    var body: some View {
        List {
            if let error { Section { InlineError(message: error) }.listRowBackground(Tok.panel) }
            if !mine.isEmpty && query.isEmpty {
                Section("This session") { ForEach(mine) { row($0) } }.listRowBackground(Tok.panel)
            }
            let others = all.filter { a in query.isEmpty ? !mine.contains { $0.text == a.text } : true }
            if !others.isEmpty {
                Section(query.isEmpty && session != nil ? "All sessions" : "") { ForEach(others) { row($0) } }.listRowBackground(Tok.panel)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .searchable(text: $query, prompt: "Search prompts or folders")
        .overlay {
            if !loaded { ProgressView() } else if error == nil && mine.isEmpty && all.isEmpty {
                ContentUnavailableView(query.isEmpty ? "No prompts yet" : "No match", systemImage: "text.bubble")
            }
        }
        .task(id: query) {
            if !query.isEmpty { try? await Task.sleep(for: .milliseconds(250)) }
            await load()
        }
        .refreshable { await load() }
    }

    private func row(_ p: PromptRun) -> some View {
        Button {
            if let onPick { onPick(p.text) } else { copyToPasteboard(p.text, label: "Prompt copied") }
        } label: {
            VStack(alignment: .leading, spacing: 4) {
                Text(p.text).font(.subheadline).foregroundStyle(Tok.fg).lineLimit(4).multilineTextAlignment(.leading)
                HStack(spacing: 6) {
                    Text(p.agent ?? "claude")
                    if let c = p.cwd, !c.isEmpty { Text((c as NSString).lastPathComponent) }
                    if let at = p.at { Text(Date(timeIntervalSince1970: Double(at) / 1000), style: .relative) }
                }
                .font(.caption).foregroundStyle(Tok.muted)
            }
        }
        .contextMenu {
            Button { copyToPasteboard(p.text, label: "Prompt copied") } label: { Label("Copy", systemImage: "doc.on.doc") }
            if let sid = p.sessionId {
                Button { store.openThread(sid) } label: { Label("Open Thread", systemImage: "bubble.left") }
            }
        }
    }

    private func load() async {
        do {
            let q = query.trimmingCharacters(in: .whitespaces)
            async let a = APIClient.shared.prompts(query: q)
            if let s = session, q.isEmpty { mine = (try? await APIClient.shared.prompts(session: s, limit: 100)) ?? [] } else { mine = [] }
            all = try await a
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
        loaded = true
    }
}

/// Sheet from the composer: pick an earlier prompt to put in the message box.
struct PromptHistorySheet: View {
    @Environment(\.dismiss) private var dismiss
    var session: String?
    var onPick: (String) -> Void

    var body: some View {
        NavigationStack {
            PromptHistoryList(session: session) { onPick($0); dismiss() }
                .navigationTitle("Earlier Prompts")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Close") { dismiss() } } }
        }
        .toastHost()
    }
}

/// Settings, History: every prompt and every ! command.
struct HistoryView: View {
    @AppStorage("history.mode") private var mode = "prompts"

    var body: some View {
        VStack(spacing: 0) {
            Picker("History", selection: $mode) {
                Text("Prompts").tag("prompts")
                Text("! Commands").tag("commands")
            }
            .pickerStyle(.segmented)
            .padding(.horizontal, 16).padding(.vertical, 8)
            if mode == "commands" { AllCommandsView() } else { PromptHistoryList() }
        }
        .background(Tok.bg)
        .navigationTitle("History")
        .navigationBarTitleDisplayMode(.inline)
    }
}
