import SwiftUI

// The daemon's notification centre (/api/notify) as a native list grouped by day.
struct AlertsView: View {
    @Environment(AppStore.self) private var store
    @State private var filter = ""

    private static let filters: [(String, String, String)] = [
        ("", "All", "tray"), ("needs-you", "Needs you", "exclamationmark.circle"), ("finished", "Finished", "checkmark.circle"),
        ("error", "Errors", "xmark.octagon"), ("spend", "Spend", "dollarsign.circle"),
    ]

    private var groups: [(String, [AlertItem])] {
        let items = store.alerts.filter { filter.isEmpty || $0.kind == filter }
        let cal = Calendar.current
        var out: [(String, [AlertItem])] = []
        for a in items {
            let d = Date(timeIntervalSince1970: Double(a.at) / 1000)
            let label = cal.isDateInToday(d) ? "Today" : cal.isDateInYesterday(d) ? "Yesterday" : "Earlier"
            if out.last?.0 == label { out[out.count - 1].1.append(a) } else { out.append((label, [a])) }
        }
        return out
    }

    var body: some View {
        NavigationStack {
            List {
                if let err = store.alertsError, store.alerts.isEmpty {
                    InlineError(message: err).listRowBackground(Tok.bg).listRowSeparator(.hidden)
                }
                ForEach(groups, id: \.0) { g in
                    Section {
                        ForEach(g.1) { a in row(a) }
                    } header: {
                        Text(g.0).font(.subheadline.weight(.semibold)).foregroundStyle(Tok.muted).textCase(nil)
                    }
                }
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
            .background(Tok.bg)
            .overlay {
                if groups.isEmpty && (store.alertsError == nil || !store.alerts.isEmpty) {
                    ContentUnavailableView {
                        Label(filter.isEmpty ? "No notifications" : "Nothing of this kind", systemImage: "bell.slash")
                    } description: {
                        Text("Threads that need you, finish, fail or hit a spend limit appear here.")
                    }
                }
            }
            .navigationTitle("Recent")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button { store.needsRecent = false } label: { Label("Waiting", systemImage: "chevron.left") }
                        .labelStyle(.titleAndIcon)
                }
                ToolbarItemGroup(placement: .topBarTrailing) { menu; ComposeButton() }
            }
            .refreshable { await store.loadAlerts() }
        }
    }

    private var menu: some View {
        Menu {
            Picker("Show", selection: $filter) {
                ForEach(Self.filters, id: \.0) { f in Label(f.1, systemImage: f.2).tag(f.0) }
            }
            Divider()
            Button { Task { await store.markAllAlertsRead() } } label: {
                Label("Mark all as read", systemImage: "checkmark.circle")
            }
            .disabled(store.alertsUnread == 0)
        } label: {
            Image(systemName: filter.isEmpty ? "line.3.horizontal.decrease.circle" : "line.3.horizontal.decrease.circle.fill")
                .accessibilityLabel("Filter and actions")
        }
    }

    private func row(_ a: AlertItem) -> some View {
        Button { open(a) } label: { AlertRow(a: a) }
            .buttonStyle(.plain)
            .listRowBackground(Tok.bg)
            .listRowSeparatorTint(Tok.line)
            .listRowInsets(EdgeInsets(top: 0, leading: 8, bottom: 0, trailing: 16))
            .swipeActions(edge: .leading) {
                if !a.isRead {
                    Button { Task { await store.markAlertsRead([a.id]) } } label: {
                        Label("Read", systemImage: "envelope.open")
                    }
                    .tint(Tok.accent)
                }
            }
            .contextMenu {
                if let t = a.threadId, !t.isEmpty {
                    Button { open(a) } label: { Label("Open thread", systemImage: "bubble.left") }
                }
                if !a.isRead {
                    Button { Task { await store.markAlertsRead([a.id]) } } label: { Label("Mark as read", systemImage: "envelope.open") }
                }
            }
    }

    private func open(_ a: AlertItem) {
        Task { await store.markAlertsRead([a.id]) }
        if let t = a.threadId, !t.isEmpty { store.openThread(t) }
        else if let s = a.url, s.hasPrefix("https://"), let u = URL(string: s) { UIApplication.shared.open(u) } // the install page
    }
}

private struct AlertRow: View {
    let a: AlertItem

    private var kind: (icon: String, label: String, tone: Tone) {
        if a.resolved ?? false { return ("checkmark", "Resolved", .ok) }
        switch a.kind {
        case "needs-you": return ("exclamationmark", "Needs you", .need)
        case "finished": return ("checkmark", "Finished", .ok)
        case "error": return ("xmark", "Error", .err)
        case "spend": return ("dollarsign", "Spend", .acc)
        default: return ("bell", a.kind.capitalized, .mute)
        }
    }

    var body: some View {
        let k = kind
        HStack(alignment: .top, spacing: 10) {
            Circle().fill(a.isRead ? .clear : Tok.accent).frame(width: 8, height: 8).padding(.top, 14)
                .accessibilityHidden(true)
            Image(systemName: k.icon)
                .font(.footnote.weight(.bold))
                .foregroundStyle(k.tone.fg)
                .frame(width: 30, height: 30)
                .background(k.tone.soft, in: RoundedRectangle(cornerRadius: 8))
                .padding(.top, 2)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(a.title)
                        .font(.body.weight(a.isRead ? .regular : .semibold))
                        .foregroundStyle(a.isRead ? Tok.fg2 : Tok.fg)
                        .lineLimit(2)
                    Spacer(minLength: 4)
                    Text(relativeTime(a.at)).font(.subheadline).monospacedDigit().foregroundStyle(Tok.muted)
                }
                if let b = a.body, !b.isEmpty {
                    Text(b).font(.subheadline).foregroundStyle(Tok.fg2).lineLimit(3)
                }
                HStack(spacing: 8) {
                    Text(k.label).foregroundStyle(k.tone.fg)
                    if let c = a.count, c > 1 { Text("\(c) events") }
                }
                .font(.footnote.weight(.medium))
                .foregroundStyle(Tok.muted)
            }
            .multilineTextAlignment(.leading)
        }
        .padding(.vertical, 10)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityValue(a.isRead ? "" : "Unread")
    }
}
