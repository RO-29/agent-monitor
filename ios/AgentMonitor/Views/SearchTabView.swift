import SwiftUI

/// The Search tab: threads by name, folder or branch, then the messages that hold the words.
struct SearchTabView: View {
    @Environment(AppStore.self) private var store
    @State private var query = ""

    private var threads: [V3Thread] {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        guard !q.isEmpty else { return [] }
        return (store.threads + store.peerThreads)
            .filter { t in [t.name, t.repoName, t.ref ?? "", t.agent, t.machine ?? ""].contains { $0.lowercased().contains(q) } }
            .sorted { $0.lastAt > $1.lastAt }
    }

    var body: some View {
        NavigationStack {
            List {
                if query.isEmpty {
                    ContentUnavailableView("Search", systemImage: "magnifyingglass",
                                           description: Text("Find a thread by name, folder or branch, or search what you asked your agents."))
                        .listRowBackground(Tok.bg).listRowSeparator(.hidden)
                } else {
                    let found = threads
                    if !found.isEmpty {
                        Section("Threads") {
                            ForEach(found.prefix(30)) { t in
                                Button { store.openThread(t.id) } label: {
                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(t.name).font(.subheadline.weight(.medium)).foregroundStyle(Tok.fg).lineLimit(2)
                                        Text([t.repoName, store.group(of: t).label].filter { !$0.isEmpty }.joined(separator: " · "))
                                            .font(.caption).foregroundStyle(Tok.muted)
                                    }
                                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                                }
                            }
                        }
                        .listRowBackground(Tok.bg)
                    }
                    MessageSearchRows(query: query)
                }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
            .background(Tok.bg)
            .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .always), prompt: "Threads and messages")
            .navigationTitle("Search")
        }
    }
}
