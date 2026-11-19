import SwiftUI

struct SessionsView: View {
    @Environment(AppStore.self) private var store

    var body: some View {
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
                            .listRowBackground(Tok.bg)
                            .listRowSeparatorTint(Tok.line)
                        }
                    }
                    .listStyle(.plain)
                    .amPage(Tok.bg)
                }
            }
            .background(Tok.bg)
            .navigationTitle("Agents")
            .navigationBarTitleDisplayMode(.inline)
            .navigationDestination(for: String.self) { id in
                SessionDetailView(sessionID: id)
            }
            .navigationDestination(for: Chain.self) { chain in
                ChainMergedView(chain: chain)
            }
            .refreshable { await store.hydrate() }
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
                AgentGlyph(agent: session.tool, size: 13)
                Text(session.headline)
                    .font(Typo.ui(13.5, .medium))
                    .foregroundStyle(Tok.fg)
                    .lineLimit(1)
                if let (pos, total) = chainPos {
                    Text("⛓ \(pos)/\(total)")
                        .font(.system(size: 9.5, design: .monospaced))
                        .foregroundStyle(Tok.sub)
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(Tok.sub.opacity(0.14), in: Capsule())
                }
                Spacer(minLength: 4)
                Text(relativeTime(session.lastActivityAt))
                    .font(Typo.xs).foregroundStyle(Tok.muted)
            }

            HStack(spacing: 8) {
                StateBadge(state: session.state)
                Text(session.project)
                    .font(Typo.sm).foregroundStyle(Tok.muted).lineLimit(1)
            }

            if let msg = session.permissionMessage ?? session.lastMessage, !msg.isEmpty {
                Text(msg)
                    .font(Typo.mono(11.5))
                    .foregroundStyle(Tok.fg2)
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
            .font(Typo.xs)
            .foregroundStyle(Tok.muted)
        }
        .padding(.vertical, 4)
    }
}
