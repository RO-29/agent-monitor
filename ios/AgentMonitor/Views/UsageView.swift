import SwiftUI

// Usage for every agent (GET /api/usage/limits): one foldable row each with a one-line summary;
// open, it shows the plan windows and the tokens per model in each window.
struct UsageStrip: View {
    static let order = ["claude", "codex", "opencode", "cursor", "pi"]
    @State private var usage: [AgentUsage] = []
    @State private var loaded = false

    private var rows: [AgentUsage] {
        var out = Self.order.map { k in usage.first { agentKey($0.agent) == k } ?? AgentUsage(agent: k) }
        out += usage.filter { !Self.order.contains(agentKey($0.agent)) }
        return out
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if !loaded {
                ProgressView().frame(maxWidth: .infinity).padding(.vertical, 8)
            } else {
                ForEach(Array(rows.enumerated()), id: \.element.agent) { i, a in
                    if i > 0 { Rectangle().fill(Tok.line).frame(height: 0.5) }
                    AgentUsageRow(a: a)
                }
            }
        }
        .task {
            while !Task.isCancelled {
                #if DEBUG
                if DebugRoute.sample { usage = Self.samples(); loaded = true; return }
                #endif
                if let u = try? await APIClient.shared.usageLimits() { usage = u.agents ?? [] }
                loaded = true
                try? await Task.sleep(nanoseconds: 60_000_000_000)
            }
        }
    }

    #if DEBUG
    static func samples() -> [AgentUsage] {
        let h: Int64 = 3_600_000
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        func m(_ model: String, _ w: String, _ i: Int64, _ o: Int64, _ c: Double, _ n: Int) -> ModelUsage {
            ModelUsage(model: model, window: w, inTokens: i, outTokens: o, cacheRead: i * 12, cacheWrite: i / 3, costUsd: c, messages: n)
        }
        return [
            AgentUsage(agent: "claude", plan: "max", installed: true,
                       windows: [UsageWindow(key: "5h", label: "5 hours", usedPercent: 42, resetsAt: now + 2 * h),
                                 UsageWindow(key: "weekly", label: "Week", usedPercent: 76, resetsAt: now + 50 * h)],
                       models: [m("claude-opus-4-5", "5h", 1_200_000, 84_000, 9.42, 61), m("claude-fable-5", "5h", 310_000, 22_000, 1.1, 12),
                                m("claude-sonnet-4-5", "5h", 90_000, 8_000, 0.41, 9), m("claude-haiku-4-5", "5h", 40_000, 3_100, 0.05, 20),
                                m("claude-opus-4-5", "weekly", 18_400_000, 1_100_000, 141.2, 820), m("claude-sonnet-4-5", "weekly", 2_300_000, 160_000, 9.9, 140)]),
            AgentUsage(agent: "codex", plan: "pro", installed: true,
                       windows: [UsageWindow(key: "5h", label: "5 hours", usedPercent: 12, resetsAt: now + 4 * h),
                                 UsageWindow(key: "weekly", label: "Week", usedPercent: 31, resetsAt: now + 90 * h)],
                       models: [m("gpt-5-codex", "weekly", 4_100_000, 210_000, 0, 96)], credits: .object(["balance": .number(42)])),
            AgentUsage(agent: "opencode", installed: true, models: [m("anthropic/claude-sonnet-4-5", "weekly", 520_000, 41_000, 2.3, 30)]),
            AgentUsage(agent: "cursor", note: "Cursor keeps usage on its servers.", installed: true),
            AgentUsage(agent: "pi", installed: false),
        ]
    }
    #endif
}

private struct AgentUsageRow: View {
    let a: AgentUsage
    @AppStorage private var open: Bool

    init(a: AgentUsage) {
        self.a = a
        _open = AppStorage(wrappedValue: false, "usage.open." + agentKey(a.agent))
    }

    private var notInstalled: Bool { a.installed == false }
    private var hasData: Bool { a.hasReading || a.hasModels || a.hasLocal }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Button { if hasData || a.note != nil { withAnimation(.snappy) { open.toggle() } } } label: {
                HStack(spacing: 8) {
                    AgentGlyph(agent: a.agent, size: 15)
                    Text(agentName(a.agent)).font(.subheadline.weight(.semibold)).foregroundStyle(notInstalled ? Tok.muted : Tok.fg)
                    if let p = a.plan, !p.isEmpty {
                        Text(p.capitalized).font(.caption.weight(.medium)).foregroundStyle(Tok.muted)
                    }
                    Spacer(minLength: 6)
                    summary
                    if hasData || a.note != nil {
                        Image(systemName: "chevron.right").font(.caption2.weight(.semibold)).foregroundStyle(Tok.muted)
                            .rotationEffect(.degrees(open ? 90 : 0))
                    }
                }
                .frame(minHeight: 40)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityValue(open ? "Expanded" : "Collapsed")
            if open {
                VStack(alignment: .leading, spacing: 12) {
                    if a.hasReading {
                        HStack(spacing: 16) { ForEach(a.windows, id: \.key) { meter($0) } }
                    }
                    ForEach(["5h", "weekly"], id: \.self) { w in
                        let ms = (a.models ?? []).filter { $0.window == w }.sorted { ($0.costUsd ?? 0, $0.inTokens ?? 0) > ($1.costUsd ?? 0, $1.inTokens ?? 0) }
                        if !ms.isEmpty { models(w, ms) }
                    }
                    if !a.hasModels, a.hasLocal {
                        ForEach(a.local ?? [], id: \.key) { l in
                            let t = l.tokens
                            HStack {
                                Text(windowName(l.key)).foregroundStyle(Tok.fg2)
                                Spacer()
                                Text("\(compactCount(t.input + t.cacheCreate)) in · \(compactCount(t.output)) out").monospacedDigit()
                            }
                            .font(.footnote)
                        }
                    }
                    if let c = credits { Text(c).font(.footnote).foregroundStyle(Tok.fg2) }
                    if let n = a.note, !n.isEmpty { Text(n).font(.caption).foregroundStyle(Tok.muted) }
                }
                .padding(.leading, 23)
                .padding(.bottom, 8)
                .transition(.opacity)
            }
        }
    }

    /// The closed row's right side: plan percents, else this window's tokens, else a state.
    @ViewBuilder private var summary: some View {
        if notInstalled {
            Text("Not installed").font(.caption).foregroundStyle(Tok.muted)
        } else if a.hasReading {
            HStack(spacing: 8) {
                ForEach(a.windows.filter { $0.usedPercent != nil }, id: \.key) { w in
                    let p = w.usedPercent ?? 0
                    Text("\(w.key == "5h" ? "5h" : "wk") \(Int(p))%")
                        .foregroundStyle(p >= 90 ? Tok.err : p >= 70 ? Tok.need : Tok.fg2)
                }
            }
            .font(.caption.monospacedDigit().weight(.medium))
        } else if let t = topTokens {
            Text(t).font(.caption.monospacedDigit()).foregroundStyle(Tok.fg2)
        } else {
            Text(a.installed == true ? "No data" : "No reading").font(.caption).foregroundStyle(Tok.muted)
        }
    }

    private var topTokens: String? {
        let ms = a.models ?? []
        if !ms.isEmpty {
            let w = ms.contains { $0.window == "5h" } ? "5h" : "weekly"
            let n = ms.filter { $0.window == w }.reduce(Int64(0)) { $0 + ($1.inTokens ?? 0) + ($1.outTokens ?? 0) }
            return "\(compactCount(n)) tok \(w == "5h" ? "5h" : "wk")"
        }
        if let l = a.local?.first(where: { $0.tokens.total > 0 }) { return "\(compactCount(l.tokens.total)) tok" }
        return nil
    }

    /// credits: {unlimited?, balance? (number or string), hasCredits?, spendUsd?}; shown only when present.
    private var credits: String? {
        guard case .object(let o)? = a.credits else { return nil }
        if case .bool(true)? = o["unlimited"] { return "Credits: unlimited" }
        switch o["balance"] {
        case .number(let n)?: return "Credits: " + String(format: n == n.rounded() ? "%.0f" : "%.2f", n)
        case .string(let b)? where !b.isEmpty: return "Credits: \(b)"
        default: break
        }
        if case .bool(true)? = o["hasCredits"] { return "Has credits" }
        return nil
    }

    private func windowName(_ key: String) -> String { key == "5h" ? "5 hours" : "Week" }

    private func meter(_ w: UsageWindow) -> some View {
        let p = w.usedPercent ?? 0
        let tint = p >= 90 ? Tok.err : p >= 70 ? Tok.need : Tok.ok
        return VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(windowName(w.key)).foregroundStyle(Tok.fg2)
                Spacer()
                Text(w.usedPercent.map { "\(Int($0))%" } ?? "-").monospacedDigit().foregroundStyle(Tok.fg)
            }
            .font(.footnote)
            ProgressView(value: min(max(p, 0), 100), total: 100).tint(tint)
            if let r = w.resetsAt, r > 0 {
                Text("Resets in " + resetIn(r)).font(.caption2).foregroundStyle(Tok.muted)
            }
        }
        .frame(maxWidth: .infinity)
        .accessibilityElement(children: .combine)
    }

    private func models(_ w: String, _ ms: [ModelUsage]) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(windowName(w)).font(.caption.weight(.semibold)).foregroundStyle(Tok.muted)
            Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 3) {
                ForEach(ms, id: \.model) { m in
                    GridRow {
                        Text(modelName(m.model)).font(.footnote.weight(.medium)).foregroundStyle(Tok.fg).lineLimit(1)
                        Text("\(compactCount(m.inTokens ?? 0)) in").gridColumnAlignment(.trailing)
                        Text("\(compactCount(m.outTokens ?? 0)) out").gridColumnAlignment(.trailing)
                        Text((m.costUsd ?? 0) > 0 ? String(format: "$%.2f", m.costUsd ?? 0) : "").gridColumnAlignment(.trailing)
                    }
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(Tok.fg2)
                    .accessibilityElement(children: .combine)
                }
            }
        }
    }

    private func resetIn(_ at: Int64) -> String {
        let mins = max(0, Int((Double(at) / 1000 - Date().timeIntervalSince1970) / 60))
        if mins < 60 { return "\(mins) m" }
        let h = mins / 60
        if h < 24 { return "\(h) h \(mins % 60) m" }
        return "\(h / 24) d \(h % 24) h"
    }
}

/// "claude-opus-4-5-20251101" to "Opus 4.5"; other names pass through without a provider prefix.
func modelName(_ raw: String) -> String {
    let m = raw.split(separator: "/").last.map(String.init) ?? raw
    let l = m.lowercased()
    for fam in ["opus", "fable", "sonnet", "haiku"] where l.contains(fam) {
        let tail = l.components(separatedBy: fam).last ?? ""
        let nums = tail.split(separator: "-").filter { $0.allSatisfy(\.isNumber) && $0.count <= 2 }.prefix(2)
        return fam.capitalized + (nums.isEmpty ? "" : " " + nums.joined(separator: "."))
    }
    return m
}
