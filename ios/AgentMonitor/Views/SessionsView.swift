import SwiftUI

struct SessionsView: View {
    @Environment(AppStore.self) private var store

    var body: some View {
        NavigationStack {
            Group {
                if store.sessions.isEmpty {
                    EmptyHint(icon: "moon.zzz",
                              title: "No agents",
                              subtitle: "Launch an agent via the SessionStart hook or `agent-monitor run`.")
                } else {
                    List {
                        ForEach(store.sessions) { s in
                            NavigationLink(value: s.id) {
                                SessionRow(session: s, chain: store.chain(for: s.id))
                            }
                        }
                    }
                    .listStyle(.plain)
                }
            }
            .navigationTitle("Agents")
            .navigationDestination(for: String.self) { id in
                SessionDetailView(sessionID: id)
            }
            .navigationDestination(for: Chain.self) { chain in
                ChainMergedView(chain: chain)
            }
            .toolbar { ToolbarItem(placement: .topBarTrailing) { ConnectionChip() } }
            .refreshable { await store.hydrate() }
        }
    }
}

struct SessionRow: View {
    let session: Session
    var chain: Chain? = nil

    /// This session's 1-based position within its chain, if linked.
    private var chainPos: (Int, Int)? {
        guard let chain, let idx = chain.sessions.firstIndex(of: session.id) else { return nil }
        return (idx + 1, chain.sessions.count)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Image(systemName: Theme.icon(for: session.tool))
                    .foregroundStyle(Theme.color(for: session.state))
                Text(session.headline)
                    .font(.subheadline.weight(.semibold))
                    .lineLimit(1)
                if let (pos, total) = chainPos {
                    Text("⛓ \(pos)/\(total)")
                        .font(.system(size: 9.5, design: .monospaced))
                        .foregroundStyle(.purple)
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(Color.purple.opacity(0.14), in: Capsule())
                }
                Spacer(minLength: 4)
                Text(relativeTime(session.lastActivityAt))
                    .font(.caption2).foregroundStyle(.secondary)
            }

            HStack(spacing: 8) {
                StateBadge(state: session.state)
                Text(session.project)
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }

            if let msg = session.permissionMessage ?? session.lastMessage, !msg.isEmpty {
                Text(msg)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }

            HStack(spacing: 12) {
                if let m = session.model, !m.isEmpty {
                    Label(m, systemImage: "brain").labelStyle(.titleAndIcon)
                }
                if let b = session.branch, !b.isEmpty {
                    Label(b, systemImage: "arrow.triangle.branch").lineLimit(1)
                }
                Label("\(session.messageCount)", systemImage: "bubble.left")
                Label(compactCount(session.tokens.total), systemImage: "circle.hexagongrid")
            }
            .font(.caption2)
            .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 4)
    }
}
