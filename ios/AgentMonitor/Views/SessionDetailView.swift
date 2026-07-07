import SwiftUI
import UIKit

// Trace view — the iOS port of the web dashboard's redesigned session detail:
// a KPI strip, an activity ribbon, a faceted toolbar, and one unified stream of
// prose turns with their tool calls nested underneath. Plus "Open on Mac",
// which raises the session's terminal over the tailnet.
struct SessionDetailView: View {
    let sessionID: String
    @Environment(AppStore.self) private var store

    @State private var detail: SessionDetail?
    @State private var loading = false
    @State private var error: String?
    @State private var showBridge = false

    enum Facet: String, CaseIterable { case all = "All", prose = "Prose", tools = "Tools", errors = "Errors", you = "You" }
    @State private var facet: Facet = .all
    @State private var toolFilter = "all"
    @State private var query = ""
    @State private var focusMsg: String?
    @State private var focusing = false

    private var session: Session? { store.sessionsByID[sessionID] ?? detail?.session }
    private var stream: TraceStream { Trace.build(detail) }

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 14) {
                header
                if let error { ErrorBanner(message: error) }
                if loading && detail == nil {
                    ProgressView().frame(maxWidth: .infinity).padding(.top, 40)
                } else {
                    kpiStrip
                    let s = stream
                    if s.events.count >= 24 { RibbonView(bins: Trace.bins(s, n: 40), duration: fmtDurationMs(s.tsMax - s.tsMin)) }
                    facetBar(s)
                    streamBody(s)
                    filesSection
                }
            }
            .padding()
        }
        .navigationTitle(session?.headline ?? "Session")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .automatic), prompt: "Search this session")
        .toolbar {
            ToolbarItemGroup(placement: .topBarTrailing) {
                Button { Task { await openOnMac() } } label: {
                    Image(systemName: focusing ? "hourglass" : "macwindow.on.rectangle")
                }.disabled(focusing)
                Button { showBridge = true } label: { Image(systemName: "keyboard") }
            }
        }
        .sheet(isPresented: $showBridge) { if let s = session { PaneBridgeView(session: s) } }
        .task(id: sessionID) { await load() }
        .refreshable { await load() }
        .overlay(alignment: .bottom) { toast }
    }

    // MARK: header

    @ViewBuilder private var header: some View {
        if let s = session {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 8) {
                    Image(systemName: Theme.icon(for: s.tool)).foregroundStyle(Theme.color(for: s.state))
                    Text(s.tool.capitalized).font(.subheadline.weight(.semibold))
                    if let m = s.model, !m.isEmpty {
                        Text(m.replacingOccurrences(of: "claude-", with: ""))
                            .font(.caption2.monospaced()).foregroundStyle(.secondary)
                    }
                    Spacer()
                    StateBadge(state: s.state)
                }
                Text(s.project).font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1)
                if let msg = s.permissionMessage, !msg.isEmpty, s.state == .awaitingPermission {
                    HStack(alignment: .top, spacing: 6) {
                        Image(systemName: "lock.shield")
                        Text(msg).font(.caption)
                    }
                    .padding(8)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.orange.opacity(0.14), in: RoundedRectangle(cornerRadius: 8))
                    .foregroundStyle(.orange)
                }
                chainNav
                spawnBar
                resumeBar
            }
        }
    }

    // MARK: resume

    // For an exited session, the command to pick it back up. Claude's session id
    // is its resume id; shown for any non-running session with a resume CLI.
    @ViewBuilder private var resumeBar: some View {
        if let s = session, s.state != .running, let cmd = s.resumeCommand {
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 6) {
                    Image(systemName: "arrow.clockwise.circle").foregroundStyle(.green)
                    Text("Resume this session").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Spacer()
                    Button {
                        UIPasteboard.general.string = cmd
                        show("Copied resume command")
                    } label: { Label("Copy", systemImage: "doc.on.doc").font(.caption2) }
                        .buttonStyle(.bordered).controlSize(.small)
                }
                Text(cmd)
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundStyle(.primary).textSelection(.enabled)
                    .padding(8).frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.green.opacity(0.06), in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.green.opacity(0.2), lineWidth: 1))
        }
    }

    // MARK: chain navigation

    // Prev/next through linked (e.g. /clear-continued) sessions, position dots,
    // and a jump into the merged whole-chain timeline.
    @ViewBuilder private var chainNav: some View {
        if let c = store.chain(for: sessionID), let idx = c.sessions.firstIndex(of: sessionID) {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 6) {
                    Image(systemName: "link").font(.caption2).foregroundStyle(.purple)
                    Text("Linked chain · \(c.ref)").font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    Spacer(minLength: 4)
                    Text("\(idx + 1) of \(c.sessions.count)").font(.caption2.monospaced()).foregroundStyle(.secondary)
                }
                HStack(spacing: 10) {
                    chainStep("chevron.left", "prev", idx > 0 ? c.sessions[idx - 1] : nil)
                    HStack(spacing: 5) {
                        ForEach(Array(c.sessions.enumerated()), id: \.offset) { i, sid in
                            NavigationLink(value: sid) {
                                Circle()
                                    .fill(sid == sessionID ? Color.purple : Color(.tertiarySystemFill))
                                    .frame(width: 9, height: 9)
                                    .overlay(Circle().stroke(Color.purple.opacity(sid == sessionID ? 0 : 0.4), lineWidth: 1))
                            }.buttonStyle(.plain)
                        }
                    }
                    chainStep("chevron.right", "next", idx + 1 < c.sessions.count ? c.sessions[idx + 1] : nil)
                    Spacer(minLength: 4)
                    NavigationLink(value: c) {
                        Text("⛓ Whole chain (\(c.sessions.count))")
                            .font(.caption.weight(.semibold)).foregroundStyle(.purple)
                            .padding(.horizontal, 10).padding(.vertical, 5)
                            .background(Color.purple.opacity(0.14), in: Capsule())
                    }.buttonStyle(.plain)
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.purple.opacity(0.07), in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.purple.opacity(0.2), lineWidth: 1))
        }
    }

    @ViewBuilder private func chainStep(_ icon: String, _ label: String, _ target: String?) -> some View {
        if let target {
            NavigationLink(value: target) {
                Label(label, systemImage: icon).font(.caption2).labelStyle(.iconOnly)
                    .padding(6).background(Color(.secondarySystemBackground), in: Circle())
            }.buttonStyle(.plain)
        } else {
            Image(systemName: icon).font(.caption2).foregroundStyle(.tertiary)
                .padding(6).background(Color(.secondarySystemBackground), in: Circle())
        }
    }

    // MARK: spawn edges

    // The child agents this session spawned (Agent/Task/Workflow), plus a link
    // back to the session that spawned this one.
    @ViewBuilder private var spawnBar: some View {
        let kids = store.spawnChildren[sessionID] ?? []
        let parentID = store.spawnParent[sessionID]
        if !kids.isEmpty || parentID != nil {
            VStack(alignment: .leading, spacing: 6) {
                if let parentID {
                    NavigationLink(value: parentID) {
                        Label("spawned by \(store.sessionsByID[parentID]?.headline ?? "parent")",
                              systemImage: "arrow.turn.left.up")
                            .font(.caption).foregroundStyle(.purple).lineLimit(1)
                    }.buttonStyle(.plain)
                }
                if !kids.isEmpty {
                    HStack(spacing: 5) {
                        Image(systemName: "arrow.branch").font(.caption2).foregroundStyle(.purple)
                        Text("\(kids.count) spawned track\(kids.count == 1 ? "" : "s")")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    ForEach(kids.prefix(6)) { k in
                        NavigationLink(value: k.id) {
                            Text("\(k.name) · \(firstLine(k.prompt).prefix(48))")
                                .font(.caption2.monospaced()).foregroundStyle(.purple).lineLimit(1)
                                .padding(.horizontal, 8).padding(.vertical, 4)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 7))
                                .overlay(RoundedRectangle(cornerRadius: 7).stroke(Color.purple.opacity(0.3), lineWidth: 1))
                        }.buttonStyle(.plain)
                    }
                }
            }
        }
    }

    // MARK: KPI strip

    @ViewBuilder private var kpiStrip: some View {
        let s = stream
        let tk = session?.tokens ?? TokenUsage()
        let dur = s.tsMax > s.tsMin ? (s.tsMax - s.tsMin)
                : ((session.map { $0.lastActivityAt - $0.startedAt }) ?? 0)
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                KPICard(k: "Tokens", v: compactCount(tk.total), sub: "in \(compactCount(tk.input)) · out \(compactCount(tk.output))")
                KPICard(k: "Turns", v: "\(session?.messageCount ?? s.counts.all)", sub: "\(s.counts.prose) asst · \(s.counts.user) you")
                KPICard(k: "Tools", v: "\(s.counts.tools)", sub: s.toolCounts.first.map { "\($0.name) ×\($0.count)" } ?? "—")
                KPICard(k: "Errors", v: "\(s.counts.errors)", sub: "flagged", tint: s.counts.errors > 0 ? .red : nil)
                KPICard(k: "Duration", v: fmtDurationMs(dur), sub: relativeTime(s.tsMax))
                KPICard(k: "Files", v: "\(session?.filesTouched ?? (detail?.files?.count ?? 0))", sub: "\(session?.bgTasksCount ?? 0) bg")
            }
        }
    }

    // MARK: facet bar

    private var flatMode: Bool { facet != .all || toolFilter != "all" || !query.trimmingCharacters(in: .whitespaces).isEmpty }

    @ViewBuilder private func facetBar(_ s: TraceStream) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 6) {
                    facetChip(.all, s.counts.all)
                    facetChip(.prose, s.counts.prose)
                    facetChip(.tools, s.counts.tools)
                    facetChip(.errors, s.counts.errors, tint: .red)
                    facetChip(.you, s.counts.user)
                }
            }
            if !s.toolCounts.isEmpty {
                Menu {
                    Button("All tools") { toolFilter = "all" }
                    ForEach(s.toolCounts) { tc in
                        Button("\(tc.name) · \(tc.count)") { toolFilter = tc.name; if facet == .all { facet = .tools } }
                    }
                } label: {
                    HStack(spacing: 4) {
                        Image(systemName: "line.3.horizontal.decrease.circle")
                        Text(toolFilter == "all" ? "All tools" : toolFilter).font(.caption)
                    }
                }
            }
        }
    }

    private func facetChip(_ f: Facet, _ count: Int, tint: Color = .accentColor) -> some View {
        let on = facet == f
        return Button {
            facet = f; if f == .errors { toolFilter = "all" }
        } label: {
            HStack(spacing: 5) {
                Text(f.rawValue).font(.caption.weight(.medium))
                Text("\(count)").font(.caption2.monospaced())
                    .foregroundStyle(on ? tint : .secondary)
            }
            .padding(.horizontal, 11).padding(.vertical, 6)
            .background((on ? tint.opacity(0.16) : Color(.secondarySystemBackground)), in: Capsule())
            .foregroundStyle(on ? tint : .primary)
        }
        .buttonStyle(.plain)
    }

    // MARK: stream

    @ViewBuilder private func streamBody(_ s: TraceStream) -> some View {
        let links = childLinks(s)
        if flatMode {
            let rows = flatRows(s)
            if rows.isEmpty {
                EmptyHint(icon: "magnifyingglass", title: "No matches", subtitle: "Try a different facet or search.")
            } else {
                ForEach(rows.prefix(400)) { item in
                    switch item {
                    case .msg(let m): FlatMsgRow(msg: m)
                    case .tool(let t): ToolRowView(tool: t, showTime: true, childID: links[t.id])
                    }
                }
                if rows.count > 400 {
                    Text("Showing first 400 of \(rows.count) — narrow with search.")
                        .font(.caption2).foregroundStyle(.secondary).padding(.top, 4)
                }
            }
        } else if s.turns.isEmpty {
            EmptyHint(icon: "text.bubble", title: "No activity", subtitle: "Nothing parsed yet for this session.")
        } else {
            ForEach(s.turns) { TurnCard(turn: $0, childLinks: links) }
        }
    }

    private enum FlatItem: Identifiable {
        case msg(DetailMsg), tool(TraceTool)
        var id: String { switch self { case .msg(let m): return "m" + m.id; case .tool(let t): return "t" + t.id } }
        var ts: Int64 { switch self { case .msg(let m): return m.ts; case .tool(let t): return t.ts } }
    }

    private func flatRows(_ s: TraceStream) -> [FlatItem] {
        let q = query.lowercased().trimmingCharacters(in: .whitespaces)
        var rows: [FlatItem] = []
        let wantMsg = facet == .all || facet == .prose || facet == .you
        let wantTool = facet == .all || facet == .tools || facet == .errors
        if wantMsg && toolFilter == "all" {
            for m in s.keptMsgs {
                if facet == .prose && m.role != "assistant" { continue }
                if facet == .you && m.role != "user" { continue }
                rows.append(.msg(m))
            }
        }
        if wantTool {
            for t in s.flatTools {
                if facet == .errors && !t.isError { continue }
                if toolFilter != "all" && t.name != toolFilter { continue }
                rows.append(.tool(t))
            }
        }
        if !q.isEmpty {
            rows = rows.filter {
                switch $0 {
                case .msg(let m): return m.text.lowercased().contains(q)
                case .tool(let t): return (t.name + " " + t.args + " " + t.result).lowercased().contains(q)
                }
            }
        }
        return rows.sorted { $0.ts < $1.ts }
    }

    // MARK: files

    @ViewBuilder private var filesSection: some View {
        let files = detail?.files ?? []
        if !files.isEmpty {
            DisclosureGroup {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(files, id: \.self) { f in
                        Text(f.replacingOccurrences(of: NSHomeDirectory(), with: "~"))
                            .font(.caption2.monospaced()).foregroundStyle(.secondary)
                            .lineLimit(1).textSelection(.enabled)
                    }
                }.padding(.top, 4)
            } label: {
                Text("Files touched · \(files.count)").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
            }
        }
    }

    // MARK: toast

    @ViewBuilder private var toast: some View {
        if let focusMsg {
            Text(focusMsg)
                .font(.caption).foregroundStyle(.white)
                .padding(.horizontal, 14).padding(.vertical, 9)
                .background(.black.opacity(0.82), in: Capsule())
                .padding(.bottom, 12)
                .transition(.move(edge: .bottom).combined(with: .opacity))
        }
    }

    // MARK: actions

    private func openOnMac() async {
        focusing = true; defer { focusing = false }
        do {
            let r = try await APIClient.shared.focus(sessionID)
            if r.ok {
                show("↗ \(r.terminal ?? "terminal") · \(r.sessionName ?? "") \(r.paneId ?? "")")
            } else {
                show(r.reason == "unregistered" ? "No tmux pane for this session" : "Couldn't focus (\(r.reason ?? "?"))")
            }
        } catch { show("Focus failed: \(error.localizedDescription)") }
    }

    private func show(_ m: String) {
        withAnimation { focusMsg = m }
        Task { try? await Task.sleep(nanoseconds: 2_600_000_000); withAnimation { focusMsg = nil } }
    }

    private func load() async {
        loading = true; defer { loading = false }
        await store.fetchChains()   // keep prev/next + spawn edges current
        do { detail = try await APIClient.shared.detail(sessionID); error = nil }
        catch { self.error = error.localizedDescription }
    }

    // Map each Agent/Task/Workflow track tool → the child session it launched,
    // by prompt-prefix match against this session's spawn children (mirror of
    // the web matchedChildFor).
    private func childLinks(_ s: TraceStream) -> [String: String] {
        let kids = store.spawnChildren[sessionID] ?? []
        guard !kids.isEmpty else { return [:] }
        func norm(_ x: String) -> String {
            String(x.lowercased().split(whereSeparator: { $0 == " " || $0 == "\n" || $0 == "\t" })
                .joined(separator: " ").prefix(40))
        }
        var map: [String: String] = [:]
        for t in s.flatTools {
            let isTrack = t.name == "Agent" || t.name == "Workflow" || (t.name == "Task" && t.isSubagent) || t.isSub
            guard isTrack else { continue }
            let pv = norm(toolPreview(name: t.name, args: t.args))
            guard !pv.isEmpty else { continue }
            for k in kids {
                let kp = norm(k.prompt)
                if !kp.isEmpty && (pv.hasPrefix(kp) || kp.hasPrefix(pv)) { map[t.id] = k.id; break }
            }
        }
        return map
    }
}

// MARK: - pieces

private struct KPICard: View {
    let k: String; let v: String; var sub: String = ""; var tint: Color? = nil
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(k.uppercased()).font(.system(size: 9, weight: .semibold)).foregroundStyle(.secondary).tracking(0.6)
            Text(v).font(.system(.title3, design: .monospaced).weight(.bold)).foregroundStyle(tint ?? .primary)
            if !sub.isEmpty { Text(sub).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1) }
        }
        .frame(minWidth: 84, alignment: .leading)
        .padding(.horizontal, 12).padding(.vertical, 9)
        .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
    }
}

private struct RibbonView: View {
    let bins: [RibbonBin]
    let duration: String
    private var maxTotal: CGFloat { CGFloat(max(1, bins.map { $0.total }.max() ?? 1)) }
    private let h: CGFloat = 54
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("ACTIVITY OVER \(duration)").font(.system(size: 9, weight: .semibold)).foregroundStyle(.secondary).tracking(0.6)
                Spacer()
                legend(.purple, "asst"); legend(.cyan, "tools"); legend(.blue, "you"); legend(.red, "err")
            }
            HStack(alignment: .bottom, spacing: 1) {
                ForEach(bins) { b in
                    VStack(spacing: 0) {
                        Spacer(minLength: 0)
                        seg(b.assistant, .purple); seg(b.tool, .cyan); seg(b.user, .blue); seg(b.error, .red)
                    }
                    .frame(maxWidth: .infinity)
                }
            }
            .frame(height: h)
            .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
        }
    }
    private func seg(_ c: Int, _ color: Color) -> some View {
        Rectangle().fill(color).frame(height: c == 0 ? 0 : max(1, CGFloat(c) / maxTotal * h))
    }
    private func legend(_ c: Color, _ t: String) -> some View {
        HStack(spacing: 3) { RoundedRectangle(cornerRadius: 1).fill(c).frame(width: 7, height: 7); Text(t).font(.system(size: 9)).foregroundStyle(.secondary) }
    }
}

private struct TurnCard: View {
    let turn: TraceTurn
    var childLinks: [String: String] = [:]
    @State private var textExpanded = false
    @State private var runExpanded = false
    private let cap = 7

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Rectangle().fill(railColor).frame(width: 2).cornerRadius(1)
            VStack(alignment: .leading, spacing: 5) {
                HStack(spacing: 8) {
                    Text(turn.role == "user" ? "YOU" : "ASSISTANT")
                        .font(.system(size: 10, weight: .bold)).tracking(1)
                        .foregroundStyle(turn.role == "user" ? .blue : .purple)
                    if turn.id != "orphans" { Text(relativeTime(turn.ts)).font(.caption2.monospaced()).foregroundStyle(.tertiary) }
                    Spacer()
                    if !turn.tools.isEmpty { Text("\(turn.tools.count) tool\(turn.tools.count == 1 ? "" : "s")").font(.caption2.monospaced()).foregroundStyle(.secondary) }
                }
                if !turn.text.isEmpty {
                    Text(turn.text)
                        .font(.callout)
                        .lineLimit(textExpanded ? nil : 6)
                        .textSelection(.enabled)
                        .onTapGesture { withAnimation { textExpanded.toggle() } }
                }
                let shown = (turn.tools.count > cap && !runExpanded) ? Array(turn.tools.prefix(cap)) : turn.tools
                ForEach(shown) { ToolRowView(tool: $0, showTime: false, childID: childLinks[$0.id]) }
                if turn.tools.count > cap && !runExpanded {
                    Button { withAnimation { runExpanded = true } } label: {
                        Text("+ \(turn.tools.count - cap) more tool calls").font(.caption2.monospaced()).foregroundStyle(.secondary)
                    }.buttonStyle(.plain)
                }
            }
        }
    }
    private var railColor: Color {
        if turn.hasErr { return .red.opacity(0.7) }
        return turn.role == "user" ? .blue : .purple.opacity(0.5)
    }
}

private struct ToolRowView: View {
    let tool: TraceTool
    var showTime: Bool
    var childID: String? = nil
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button { withAnimation { open.toggle() } } label: {
                HStack(spacing: 7) {
                    Circle().fill(dotColor).frame(width: 7, height: 7)
                    if showTime { Text(relativeTime(tool.ts)).font(.system(size: 10, design: .monospaced)).foregroundStyle(.tertiary) }
                    Text(tool.name).font(.system(size: 11.5, weight: .semibold, design: .monospaced)).foregroundStyle(toolColor(tool.name))
                    Text(firstLine(toolPreview(name: tool.name, args: tool.args)))
                        .font(.system(size: 11.5, design: .monospaced)).foregroundStyle(.secondary).lineLimit(1)
                    Spacer(minLength: 4)
                    if let childID {
                        NavigationLink(value: childID) {
                            Text("↳ open").font(.system(size: 9, weight: .bold))
                                .padding(.horizontal, 5).padding(.vertical, 1)
                                .background(Color.purple.opacity(0.16), in: Capsule()).foregroundStyle(.purple)
                        }.buttonStyle(.plain)
                    }
                    if tool.isError { badge("error", .red) }
                    else if tool.isSub || tool.isSubagent { badge("sub", .purple) }
                    if !tool.result.isEmpty { Text(fmtBytes(tool.result.count)).font(.system(size: 10, design: .monospaced)).foregroundStyle(.tertiary) }
                    else if tool.running { Text("running").font(.system(size: 10)).foregroundStyle(.yellow) }
                    Image(systemName: "chevron.right").font(.system(size: 9)).foregroundStyle(.tertiary).rotationEffect(.degrees(open ? 90 : 0))
                }
            }
            .buttonStyle(.plain)
            .padding(.vertical, 4)
            if open {
                VStack(alignment: .leading, spacing: 6) {
                    Text(tool.isSub ? "Prompt" : "Input").font(.caption2.weight(.bold)).foregroundStyle(.secondary)
                    CodeBlock(text: tool.args.isEmpty ? "(none)" : tool.args)
                    Text(tool.isSub ? "Result" : "Output").font(.caption2.weight(.bold)).foregroundStyle(.secondary)
                    CodeBlock(text: tool.result.isEmpty ? "— no output yet —" : tool.result)
                }
                .padding(.leading, 14).padding(.bottom, 6)
            }
        }
    }
    private var dotColor: Color {
        if tool.running { return .yellow }
        return tool.isError ? .red : .green
    }
    private func badge(_ t: String, _ c: Color) -> some View {
        Text(t.uppercased()).font(.system(size: 8, weight: .bold)).tracking(0.4)
            .padding(.horizontal, 5).padding(.vertical, 1)
            .background(c.opacity(0.16), in: Capsule()).foregroundStyle(c)
    }
}

// MARK: - merged whole-chain view

// Stitches every session in a chain into one chronological timeline: combined
// KPIs + ribbon, then each member's stream under a "/clear · fresh context"
// divider. Member transcripts are lazy-loaded in parallel. iOS port of the
// web renderMergedChain.
struct ChainMergedView: View {
    let chain: Chain
    @Environment(AppStore.self) private var store

    @State private var details: [String: SessionDetail] = [:]
    @State private var loading = true

    private var members: [Session] { chain.sessions.compactMap { store.sessionsByID[$0] } }

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 14) {
                kpiStrip
                if loading && details.isEmpty {
                    ProgressView().frame(maxWidth: .infinity).padding(.top, 40)
                }
                ForEach(Array(members.enumerated()), id: \.element.id) { i, m in
                    divider(i, m)
                    if let s = details[m.id].map({ Trace.build($0) }) {
                        if s.turns.isEmpty {
                            Text("No activity.").font(.caption).foregroundStyle(.secondary)
                        } else {
                            ForEach(s.turns) { TurnCard(turn: $0) }
                        }
                    } else {
                        HStack(spacing: 6) { ProgressView().controlSize(.small); Text("Loading session \(i + 1)…").font(.caption).foregroundStyle(.secondary) }
                    }
                }
            }
            .padding()
        }
        .navigationTitle("Whole chain")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: chain.id) { await loadAll() }
    }

    // Combined KPIs. Tokens + turns come from each session's summary (always
    // present); tools/errors/span come from the transcripts as they stream in.
    @ViewBuilder private var kpiStrip: some View {
        let streams = members.compactMap { details[$0.id].map { d in Trace.build(d) } }
        let tkIn = members.reduce(Int64(0)) { $0 + $1.tokens.input }
        let tkOut = members.reduce(Int64(0)) { $0 + $1.tokens.output }
        let tkFull = members.reduce(Int64(0)) { $0 + $1.tokens.input + $1.tokens.output + $1.tokens.cacheRead + $1.tokens.cacheCreate }
        let turns = members.reduce(0) { $0 + $1.messageCount }
        let tools = streams.reduce(0) { $0 + $1.counts.tools }
        let errs = streams.reduce(0) { $0 + $1.counts.errors }
        let tsMin = streams.map(\.tsMin).filter { $0 > 0 }.min() ?? 0
        let tsMax = streams.map(\.tsMax).max() ?? 0
        let loaded = members.filter { details[$0.id] != nil }.count
        let partial = loaded < members.count ? " · \(loaded)/\(members.count)" : ""
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "link").foregroundStyle(.purple)
                Text("\(members.count) linked sessions · \(chain.ref)")
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    KPICard(k: "Sessions", v: "\(members.count)", sub: chain.ref)
                    KPICard(k: "Tokens", v: compactCount(tkFull), sub: "in \(compactCount(tkIn)) · out \(compactCount(tkOut))")
                    KPICard(k: "Turns", v: "\(turns)", sub: "across chain")
                    KPICard(k: "Tools", v: "\(tools)", sub: partial.isEmpty ? " " : "loading…")
                    KPICard(k: "Errors", v: "\(errs)", sub: "chain-wide\(partial)", tint: errs > 0 ? .red : nil)
                    KPICard(k: "Span", v: fmtDurationMs(tsMax - tsMin), sub: "first → last")
                }
            }
        }
    }

    private func divider(_ i: Int, _ m: Session) -> some View {
        HStack(spacing: 8) {
            Rectangle().fill(Color.purple.opacity(0.3)).frame(height: 1)
            VStack(alignment: .center, spacing: 1) {
                Text(i == 0 ? "SESSION 1" : "/CLEAR · SESSION \(i + 1)")
                    .font(.system(size: 9, weight: .bold)).tracking(0.8).foregroundStyle(.purple)
                Text("\(m.headline) · \(relativeTime(m.startedAt))")
                    .font(.system(size: 9, design: .monospaced)).foregroundStyle(.tertiary).lineLimit(1)
            }
            Rectangle().fill(Color.purple.opacity(0.3)).frame(height: 1)
        }
        .padding(.top, i == 0 ? 4 : 10)
    }

    private func loadAll() async {
        loading = true; defer { loading = false }
        await withTaskGroup(of: (String, SessionDetail?).self) { group in
            for sid in chain.sessions where details[sid] == nil {
                group.addTask { (sid, try? await APIClient.shared.detail(sid)) }
            }
            for await (sid, d) in group {
                if let d { details[sid] = d }
            }
        }
    }
}

private struct FlatMsgRow: View {
    let msg: DetailMsg
    @State private var open = false
    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button { withAnimation { open.toggle() } } label: {
                HStack(spacing: 7) {
                    Circle().fill(msg.role == "user" ? Color.blue : .purple).frame(width: 5, height: 5)
                    Text(relativeTime(msg.ts)).font(.system(size: 10, design: .monospaced)).foregroundStyle(.tertiary)
                    Text(msg.role == "user" ? "you" : "asst").font(.system(size: 11, weight: .semibold, design: .monospaced))
                        .foregroundStyle(msg.role == "user" ? .blue : .purple)
                    Text(firstLine(msg.text.isEmpty ? "(no text)" : msg.text)).font(.callout).foregroundStyle(.primary).lineLimit(1)
                    Spacer(minLength: 4)
                    Image(systemName: "chevron.right").font(.system(size: 9)).foregroundStyle(.tertiary).rotationEffect(.degrees(open ? 90 : 0))
                }
            }
            .buttonStyle(.plain).padding(.vertical, 4)
            if open, !msg.text.isEmpty {
                Text(msg.text).font(.callout).textSelection(.enabled).padding(.leading, 12).padding(.bottom, 6)
            }
        }
    }
}
