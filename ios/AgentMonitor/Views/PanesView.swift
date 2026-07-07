import SwiftUI

struct PanesView: View {
    @Environment(AppStore.self) private var store
    @State private var renaming: PaneRegistration?
    @State private var aliasDraft = ""
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Group {
                if store.panes.isEmpty {
                    EmptyHint(icon: "rectangle.split.3x1",
                              title: "No registered panes",
                              subtitle: "Agents register a tmux pane on launch so they can be driven and messaged.")
                } else {
                    List {
                        if let error { ErrorBanner(message: error).listRowInsets(EdgeInsets()) }
                        ForEach(store.panes) { pane in
                            PaneRow(pane: pane)
                                .swipeActions {
                                    Button {
                                        renaming = pane
                                        aliasDraft = pane.alias ?? ""
                                    } label: { Label("Rename", systemImage: "pencil") }
                                    .tint(.blue)
                                }
                        }
                    }
                }
            }
            .navigationTitle("Panes")
            .toolbar { ToolbarItem(placement: .topBarTrailing) { ConnectionChip() } }
            .refreshable { await store.hydrate() }
            .alert("Rename pane", isPresented: Binding(get: { renaming != nil }, set: { if !$0 { renaming = nil } })) {
                TextField("Alias", text: $aliasDraft)
                Button("Cancel", role: .cancel) { renaming = nil }
                Button("Save") { Task { await saveAlias() } }
            } message: {
                Text("Give this agent a friendly name.")
            }
        }
    }

    private func saveAlias() async {
        guard let pane = renaming else { return }
        do {
            try await APIClient.shared.renamePane(pane.agentId, alias: aliasDraft)
            error = nil
        } catch { self.error = error.localizedDescription }
        renaming = nil
    }
}

struct PaneRow: View {
    let pane: PaneRegistration
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Image(systemName: Theme.icon(for: pane.tool))
                Text(pane.displayName).font(.subheadline.weight(.semibold))
                Spacer()
                Text(pane.paneId).font(.caption.monospaced()).foregroundStyle(.secondary)
            }
            Text(pane.cwd).font(.caption2.monospaced()).foregroundStyle(.secondary).lineLimit(1)
            HStack(spacing: 10) {
                Label(pane.tool, systemImage: "cpu")
                Label(pane.source, systemImage: "bolt")
                Label(relativeTime(pane.lastSeenAt), systemImage: "clock")
            }
            .font(.caption2).foregroundStyle(.tertiary)
        }
        .padding(.vertical, 2)
    }
}
