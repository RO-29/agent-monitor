import SwiftUI

struct RuleSuggestion: Decodable, Identifiable, Hashable {
    let key: String
    let agent: String?
    let tool: String
    let pattern: String
    let cwdPrefix: String?
    let count: Int
    let lastAt: Int64
    let example: String
    var id: String { key }
}

/// Settings, Suggested rules: calls allowed by hand 3 or more times in 30 days, never denied.
struct RuleSuggestionsView: View {
    @State private var list: [RuleSuggestion]?
    @State private var error: String?
    @State private var busy: String?

    private struct Wrap: Decodable { var suggestions: [RuleSuggestion]? }

    var body: some View {
        Form {
            if let error {
                Section { Label(error, systemImage: "exclamationmark.triangle").foregroundStyle(Tok.err) }
                    .listRowBackground(Tok.panel)
            }
            if let list {
                if list.isEmpty {
                    Section {
                        Text("No suggestions. A call shows here after you allow it by hand 3 times in 30 days and never deny it.")
                            .foregroundStyle(Tok.muted)
                    }
                    .listRowBackground(Tok.panel)
                }
                ForEach(list) { s in
                    Section {
                        VStack(alignment: .leading, spacing: 4) {
                            Text("\(s.tool) matching \(s.pattern)").font(.body.monospaced())
                            Text(meta(s)).font(.footnote).foregroundStyle(Tok.muted)
                            Text(s.example).font(.footnote.monospaced()).foregroundStyle(Tok.muted).lineLimit(2)
                        }
                        .accessibilityElement(children: .combine)
                        HStack {
                            Button("Add rule") { Task { await act(s, add: true) } }
                                .buttonStyle(.borderedProminent)
                            Spacer()
                            Button("Dismiss", role: .destructive) { Task { await act(s, add: false) } }
                                .accessibilityLabel("Dismiss \(s.pattern)")
                        }
                        .buttonStyle(.bordered)
                        .disabled(busy != nil)
                    }
                    .listRowBackground(Tok.panel)
                }
            } else if error == nil {
                ProgressView().frame(maxWidth: .infinity).listRowBackground(Color.clear)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Suggested rules")
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .refreshable { await load() }
    }

    private func meta(_ s: RuleSuggestion) -> String {
        var parts = ["allowed \(s.count) times", "last \(relativeTime(s.lastAt))"]
        if let c = s.cwdPrefix, !c.isEmpty { parts.append("in \((c as NSString).lastPathComponent)") }
        if let a = s.agent, !a.isEmpty { parts.insert(a, at: 0) }
        return parts.joined(separator: " · ")
    }

    private func load() async {
        do {
            list = try await APIClient.shared.get("/api/approvals/rules/suggest", as: Wrap.self).suggestions ?? []
            error = nil
        } catch {
            self.error = "Could not load suggestions: \(error.localizedDescription)"
        }
    }

    private func act(_ s: RuleSuggestion, add: Bool) async {
        busy = s.key
        defer { busy = nil }
        do {
            if add {
                var body: [String: Any] = ["action": "allow", "tool": s.tool, "pattern": s.pattern, "note": "suggested: you allowed it \(s.count) times"]
                if let a = s.agent, !a.isEmpty { body["agent"] = a }
                if let c = s.cwdPrefix, !c.isEmpty { body["cwdPrefix"] = c }
                try await APIClient.shared.post("/api/approvals/rules", body: body)
                ToastCenter.shared.show("Rule added", icon: "checkmark.shield")
            } else {
                try await APIClient.shared.post("/api/approvals/rules/suggest/\(s.key)/dismiss")
            }
            list?.removeAll { $0.key == s.key }
        } catch {
            ToastCenter.shared.show(add ? "Rule not saved" : "Not dismissed", icon: "exclamationmark.triangle.fill")
        }
    }
}

/// The daemon's reminder delay for an unanswered "needs you" alert; 0 = off.
struct ReminderSection: View {
    @State private var minutes: Int?
    @State private var failed = false
    private struct Notify: Decodable { var remindMin: Int? }
    private struct Wrap: Decodable { var notify: Notify? }

    var body: some View {
        Section {
            if let m = minutes {
                Picker("Remind after", selection: Binding(get: { m }, set: { v in Task { await save(v) } })) {
                    ForEach([0, 5, 10, 15, 30, 60], id: \.self) { Text($0 == 0 ? "Off" : "\($0) min").tag($0) }
                }
            } else if failed {
                Text("The daemon did not answer.").foregroundStyle(Tok.muted)
            } else {
                ProgressView()
            }
        } header: {
            Text("Reminder")
        } footer: {
            Text("When nobody answers a request and nobody opens its alert, it comes once more as \"Still waiting\". This setting lives on the Mac.")
        }
        .listRowBackground(Tok.panel)
        .task { await load() }
    }

    private func load() async {
        do {
            minutes = try await APIClient.shared.get("/api/settings", as: Wrap.self).notify?.remindMin ?? 10
        } catch {
            failed = true
        }
    }

    private func save(_ v: Int) async {
        let old = minutes
        minutes = v
        do {
            try await APIClient.shared.post("/api/settings", body: ["notify": ["remindMin": v]])
        } catch {
            minutes = old
            ToastCenter.shared.show("Reminder not saved", icon: "exclamationmark.triangle.fill")
        }
    }
}
