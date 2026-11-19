import SwiftUI

// Side.tsx on a phone: workspace facts, git state, edited files, the resume command, linked agents and subagents.
// One inset grouped list; each part is a disclosure row that remembers whether it is open.
struct ThreadDetailsSheet: View {
    let thread: V3Thread?
    let session: Session?
    let sessionID: String?
    var edited: [WFile] = []
    var linkedCount = 0
    var onOpenLinked: (() -> Void)? = nil

    @Environment(\.dismiss) private var dismiss
    @State private var place: SessionPlace?
    @State private var kids: [SessionChild] = []
    @State private var kidsError: String?
    @State private var loaded = false
    @State private var git: OGit?
    @State private var gitError: String?
    @State private var gitLoaded = false
    @State private var copied = 0

    private var agent: String { thread?.agent ?? session?.tool ?? "" }
    private var cwd: String { session?.cwd ?? thread?.cwd ?? "" }

    var body: some View {
        NavigationStack {
            List {
                Section {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(thread?.name ?? session?.headline ?? "Thread")
                            .font(.system(.title3, design: .serif, weight: .semibold))
                            .foregroundStyle(Tok.fg)
                        HStack(spacing: 8) {
                            AgentGlyph(agent: agent, size: 14)
                            Text(agentName(agent)).foregroundStyle(Tok.agent(agent))
                            if let s = session { StateBadge(state: s.state) }
                        }
                        .font(.subheadline.weight(.medium))
                    }
                    .padding(.vertical, 4)
                }
                .listRowBackground(Tok.panel)
                Section {
                    FoldRow("Workspace", systemImage: "folder", key: "details.workspace") { workspace }
                    if !cwd.isEmpty {
                        FoldRow("Git", systemImage: "arrow.triangle.branch", key: "details.git") {
                            if let d = git?.dirty, d > 0 { Text("\(d) changed") }
                        } content: { gitRows }
                    }
                    if !edited.isEmpty {
                        FoldRow("Edited in this session", systemImage: "pencil", key: "details.edited") {
                            Text("+\(edited.reduce(0) { $0 + $1.add }) \u{2212}\(edited.reduce(0) { $0 + $1.del })")
                        } content: {
                            ForEach(edited.prefix(30), id: \.path) { f in fileRow(f.path, add: f.add, del: f.del, mark: f.kind == "add" ? "A" : "M") }
                            if edited.count > 30 {
                                Text("and \(edited.count - 30) more").font(.footnote).foregroundStyle(Tok.muted)
                            }
                        }
                    }
                    if let cmd = resumeCommand(tool: agent, sessionID: session?.sessionId ?? nativeID, cwd: cwd) {
                        FoldRow("Resume in a terminal", systemImage: "terminal", key: "details.resume", openByDefault: false) {
                            Text(cmd).font(.footnote.monospaced()).foregroundStyle(Tok.fg2).textSelection(.enabled)
                            Button {
                                UIPasteboard.general.string = cmd
                                copied += 1
                            } label: { Label("Copy resume command", systemImage: "doc.on.doc") }
                        }
                    }
                    FoldRow("Subagents", systemImage: "person.2", key: "details.subagents") {
                        if !kids.isEmpty { Text("\(kids.count)") }
                    } content: { subagents }
                    if let onOpenLinked {
                        Button {
                            dismiss()
                            onOpenLinked()
                        } label: {
                            HStack {
                                Label("Linked Agents", systemImage: "point.3.connected.trianglepath.dotted").foregroundStyle(Tok.fg)
                                Spacer()
                                if linkedCount > 0 { Text("\(linkedCount)").foregroundStyle(Tok.muted).monospacedDigit() }
                                Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(Tok.muted.opacity(0.6))
                            }
                        }
                    }
                }
                .listRowBackground(Tok.panel)
            }
            .listStyle(.insetGrouped)
            .listSectionSpacing(.compact)
            .scrollContentBackground(.hidden)
            .background(Tok.bg)
            .navigationTitle("Details")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
            .sensoryFeedback(.success, trigger: copied)
            .task { await loadKids() }
            .task { if let sid = sessionID { place = try? await APIClient.shared.place(sid) } }
            .task(id: cwd) { await loadGit() }
        }
    }

    @ViewBuilder private var workspace: some View {
        if let p = place {
            LabeledContent("Runs in") {
                VStack(alignment: .trailing, spacing: 2) {
                    Text(p.label).font(.subheadline)
                    if let d = p.detail, !d.isEmpty { Text(d).font(.caption).foregroundStyle(Tok.muted) }
                }
            }
            if let o = p.origin, !o.isEmpty { LabeledContent("Started from", value: o) }
            if p.journey.count > 1 {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Journey").font(.caption).foregroundStyle(Tok.muted)
                    ForEach(Array(p.journey.enumerated()), id: \.offset) { i, j in
                        HStack(spacing: 6) {
                            Text("\(i + 1).").font(.caption.monospaced()).foregroundStyle(Tok.muted)
                            Text(j.label).font(.subheadline)
                            Spacer()
                            if j.at > 0 { Text(relativeTime(j.at) + " ago").font(.caption).foregroundStyle(Tok.muted) }
                        }
                    }
                }
            }
        }
        if let r = thread?.repoName, !r.isEmpty { LabeledContent("Repo", value: r) }
        if git?.branch?.isEmpty ?? true, let b = session?.branch ?? thread?.ref, !b.isEmpty { mono("Branch", b) }
        if let m = session?.model ?? thread?.model, !m.isEmpty { mono("Model", m) }
        if let t = thread { LabeledContent("Started", value: relativeTime(t.startedAt) + " ago") }
        if let n = thread?.turns { LabeledContent("Turns", value: "\(n)") }
        if let s = session {
            LabeledContent("Tokens", value: "\(compactCount(s.tokens.input)) in, \(compactCount(s.tokens.output)) out")
        }
        if let ids = thread?.sessionIDs, ids.count > 1 { LabeledContent("Sessions", value: "\(ids.count)") }
        if !cwd.isEmpty {
            VStack(alignment: .leading, spacing: 4) {
                Text("Folder").foregroundStyle(Tok.fg)
                Text(cwd).font(.footnote.monospaced()).foregroundStyle(Tok.fg2).textSelection(.enabled)
            }
            .contextMenu { Button { UIPasteboard.general.string = cwd } label: { Label("Copy", systemImage: "doc.on.doc") } }
        }
    }

    @ViewBuilder private var gitRows: some View {
        if !gitLoaded {
            ProgressView()
        } else if let gitError {
            Text(gitError).font(.subheadline).foregroundStyle(Tok.muted)
        } else if let g = git {
            if g.isRepo == false {
                Text("This folder is not a git repository.").font(.subheadline).foregroundStyle(Tok.muted)
            } else {
                if let b = g.branch, !b.isEmpty { mono("Branch", b) }
                if let u = g.upstream, !u.isEmpty {
                    LabeledContent("Upstream") {
                        Text("\(u)  \u{2191}\(g.ahead ?? 0) \u{2193}\(g.behind ?? 0)").font(.subheadline.monospaced()).lineLimit(1)
                    }
                }
                if let h = g.head, !h.isEmpty { mono("Head", String(h.prefix(8))) }
                let files = g.files ?? []
                LabeledContent("Changes", value: files.isEmpty && (g.dirty ?? 0) == 0 ? "Clean" : "\(max(g.dirty ?? 0, files.count)) files")
                ForEach(files.prefix(20), id: \.path) { f in
                    fileRow(f.path, add: f.add ?? 0, del: f.del ?? 0, mark: gitMark(f.status))
                }
                if files.count > 20 {
                    Text("and \(files.count - 20) more").font(.footnote).foregroundStyle(Tok.muted)
                }
            }
        } else {
            Text("No git state for this folder.").font(.subheadline).foregroundStyle(Tok.muted)
        }
    }

    private func gitMark(_ status: String) -> String {
        switch status {
        case "added": return "A"
        case "deleted": return "D"
        case "renamed": return "R"
        case "untracked": return "?"
        default: return "M"
        }
    }

    private func fileRow(_ path: String, add: Int, del: Int, mark: String) -> some View {
        HStack(spacing: 8) {
            Text(mark)
                .font(.caption.monospaced().weight(.bold))
                .foregroundStyle(mark == "D" ? Tok.err : mark == "A" || mark == "?" ? Tok.ok : Tok.need)
                .frame(width: 14)
            Text(path.split(separator: "/").suffix(2).joined(separator: "/"))
                .font(.footnote.monospaced()).foregroundStyle(Tok.fg).lineLimit(1).truncationMode(.head)
            Spacer(minLength: 6)
            if add > 0 { Text("+\(add)").foregroundStyle(Tok.ok) }
            if del > 0 { Text("\u{2212}\(del)").foregroundStyle(Tok.err) }
        }
        .font(.caption.monospacedDigit())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(path), \(add) added, \(del) removed")
        .contextMenu { Button { UIPasteboard.general.string = path } label: { Label("Copy Path", systemImage: "doc.on.doc") } }
    }

    private func mono(_ label: String, _ value: String) -> some View {
        LabeledContent(label) { Text(value).font(.subheadline.monospaced()).lineLimit(1) }
    }

    private var nativeID: String { String((sessionID ?? "").split(separator: ":", maxSplits: 1).last ?? "") }

    @ViewBuilder private var subagents: some View {
        if !loaded {
            ProgressView()
        } else if let kidsError {
            Text(kidsError).font(.subheadline).foregroundStyle(Tok.muted)
        } else if kids.isEmpty {
            Text("No subagents or background agents in this session.").font(.subheadline).foregroundStyle(Tok.muted)
        } else {
            ForEach(kids) { k in
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    StatusDot(color: color(k.status), size: 8)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(k.title).font(.subheadline).foregroundStyle(Tok.fg).lineLimit(2)
                        HStack(spacing: 6) {
                            AgentGlyph(agent: k.agent, size: 11)
                            Text([k.subagentType, k.background == true ? "background" : nil].compactMap { $0 }.joined(separator: " · "))
                            Spacer(minLength: 4)
                            Text(k.status == "running" ? relativeTime(k.startedAt) : fmtDuration(max(0, (k.endedAt ?? k.startedAt) - k.startedAt)))
                                .font(.caption.monospaced())
                        }
                        .font(.caption)
                        .foregroundStyle(Tok.muted)
                    }
                }
                .accessibilityElement(children: .combine)
            }
        }
    }

    private func color(_ status: String) -> Color {
        switch status {
        case "running": return Tok.run
        case "failed", "killed": return Tok.err
        case "completed": return Tok.ok
        default: return Tok.idle
        }
    }

    private func loadKids() async {
        defer { loaded = true }
        #if DEBUG
        if DebugRoute.sample { kids = DebugRoute.sampleKids(); return }
        #endif
        guard let sid = sessionID else { kidsError = "No session to read."; return }
        do {
            kids = try await APIClient.shared.children(sid).children
            kidsError = nil
        } catch let e as APIError where e.status == 404 {
            kidsError = "This daemon does not list subagents yet."
        } catch {
            kidsError = error.localizedDescription
        }
    }

    private func loadGit() async {
        defer { gitLoaded = true }
        guard !cwd.isEmpty else { return }
        #if DEBUG
        if DebugRoute.sample { git = DebugRoute.sampleGit(); return }
        #endif
        do {
            git = try await APIClient.shared.cwdGit(cwd)
            gitError = nil
        } catch let e as APIError where e.status == 404 {
            gitError = "This daemon does not report git state yet."
        } catch let e as APIError where e.status == 400 || e.status == 403 {
            gitError = "Git state is not available for this folder."
        } catch {
            gitError = error.localizedDescription
        }
    }
}

/// Shell command that reopens a session in its folder; only Claude and Codex have a resume flag (format.ts).
func resumeCommand(tool: String, sessionID: String, cwd: String) -> String? {
    guard !sessionID.isEmpty else { return nil }
    let safe = cwd.range(of: #"^[\w@%+=:,./~-]+$"#, options: .regularExpression) != nil
    let q = safe ? cwd : "'" + cwd.replacingOccurrences(of: "'", with: #"'\''"#) + "'"
    let cd = cwd.isEmpty ? "" : "cd \(q) && "
    switch agentKey(tool) {
    case "claude": return "\(cd)claude --resume \(sessionID)"
    case "codex": return "\(cd)codex resume \(sessionID)"
    default: return nil
    }
}

/// A disclosure row in a list: icon, title, a trailing summary, and its rows when open; the state persists per key.
struct FoldRow<Accessory: View, Content: View>: View {
    let title: String
    let systemImage: String
    @AppStorage private var open: Bool
    private let accessory: Accessory
    private let content: Content

    init(_ title: String, systemImage: String, key: String, openByDefault: Bool = true,
         @ViewBuilder accessory: () -> Accessory = { EmptyView() }, @ViewBuilder content: () -> Content) {
        self.title = title
        self.systemImage = systemImage
        _open = AppStorage(wrappedValue: openByDefault, "open." + key)
        self.accessory = accessory()
        self.content = content()
    }

    var body: some View {
        DisclosureGroup(isExpanded: $open) {
            content
        } label: {
            HStack(spacing: 8) {
                Label(title, systemImage: systemImage).foregroundStyle(Tok.fg)
                Spacer(minLength: 8)
                accessory.font(.subheadline).foregroundStyle(Tok.muted).monospacedDigit()
            }
        }
        .tint(Tok.muted)
    }
}
