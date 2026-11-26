import SwiftUI

/// The subagents that run now, above the composer, so the chat shows work while the parent waits.
struct RunningKidsBar: View {
    let sessionID: String
    let kids: [SessionChild]
    @State private var all = false
    @State private var open: SessionChild?

    private var running: [SessionChild] { kids.filter { $0.status == "running" }.sorted { $0.startedAt < $1.startedAt } }

    var body: some View {
        let list = running
        // The sheet sits outside the if, so it stays open when the last subagent ends.
        Group {
            if !list.isEmpty { bar(list) }
        }
        .sheet(item: $open) { c in
            NavigationStack { SubagentTranscriptView(sessionID: sessionID, child: c) }
        }
    }

    private func bar(_ list: [SessionChild]) -> some View {
            TimelineView(.periodic(from: .now, by: 1)) { ctx in
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 6) {
                        Image(systemName: "circle.fill").font(.system(size: 7)).foregroundStyle(Tok.accent)
                            .symbolEffect(.pulse, options: .repeating)
                        Text(list.count == 1 ? "1 subagent running" : "\(list.count) subagents running")
                            .font(.footnote.weight(.semibold)).foregroundStyle(Tok.fg)
                    }
                    ForEach(all ? list : Array(list.prefix(3))) { c in
                        Button { open = c } label: {
                            HStack(spacing: 8) {
                                Text(c.subagentType ?? (c.kind == "subagent" ? "Subagent" : c.agent))
                                    .font(.caption2.weight(.medium)).foregroundStyle(Tok.fg2)
                                    .padding(.horizontal, 6).padding(.vertical, 2)
                                    .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 5))
                                Text(c.title).font(.footnote).foregroundStyle(Tok.fg2).lineLimit(1)
                                Spacer(minLength: 4)
                                Text(elapsed(c, ctx.date)).font(.caption.monospacedDigit()).foregroundStyle(Tok.muted)
                            }
                            .frame(minHeight: 32).contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .disabled(c.agentId == nil && c.sessionId == nil)
                        .accessibilityLabel("\(c.title), running for \(elapsed(c, ctx.date))")
                    }
                    if list.count > 3 {
                        Button(all ? "Show fewer" : "Show all \(list.count)") { all.toggle() }
                            .font(.footnote).foregroundStyle(Tok.accent)
                    }
                }
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Tok.panel, in: RoundedRectangle(cornerRadius: 12))
                .overlay(RoundedRectangle(cornerRadius: 12).stroke(Tok.line, lineWidth: 1))
                .padding(.horizontal, 12)
            }
    }

    private func elapsed(_ c: SessionChild, _ now: Date) -> String {
        let s = max(0, Int(now.timeIntervalSince1970) - Int(c.startedAt / 1000))
        return s < 60 ? "\(s)s" : s < 3600 ? "\(s / 60)m \(s % 60)s" : "\(s / 3600)h \(s / 60 % 60)m"
    }
}
