import SwiftUI

// Approvals tab: the daemon's one needs-you list (/api/needs-you), oldest first, one card per item.
// An older daemon has no such list; AppStore then builds the same items from the older endpoints.
struct PermissionsView: View {
    @Environment(AppStore.self) private var store
    @State private var error: String?
    @State private var working: Set<String> = []

    var body: some View {
        let items = store.needsYouItems
        NavigationStack {
            ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if let error { InlineError(message: error).padding(.horizontal, 4) }
                    ForEach(items) { it in
                        card(it)
                            .id(focusID(it))
                            .overlay(focusRing(focusID(it)))
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 8)
                // An empty stack must still fill the width, or the scroll view shrinks to a narrow band.
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            // The last card's buttons must clear the floating tab bar.
            .contentMargins(.bottom, 96, for: .scrollContent)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Tok.bg)
            .onChange(of: store.focusApproval, initial: true) { _, id in
                guard let id else { return }
                withAnimation { proxy.scrollTo(id, anchor: .top) }
                Task { try? await Task.sleep(nanoseconds: 2_500_000_000); if store.focusApproval == id { store.focusApproval = nil } }
            }
            }
            .overlay {
                if items.isEmpty {
                    ContentUnavailableView {
                        Label("Nothing needs you", systemImage: "checkmark.shield")
                    } description: {
                        Text("Questions and permission requests from your agents show here.")
                    }
                }
            }
            .navigationTitle("Approvals")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { ConnectionChip(onlyWhenDown: true) }
                ToolbarItem(placement: .topBarTrailing) { ComposeButton() }
            }
            .refreshable { await store.hydrate() }
            .task { await store.loadApprovals(); await store.loadNeedsYou() }
        }
    }

    @ViewBuilder private func card(_ it: NeedsYouItem) -> some View {
        switch it.kind {
        case "approval":
            if let a = store.approvalsByID[it.key] ?? it.approval {
                HookApprovalCard(a: a, title: it.title, onOpen: { store.openThread(store.threadID(of: a)) })
            }
        case "mcp":
            if let req = it.mcp {
                PermissionCard(req: req, working: working.contains(req.id),
                               onRespond: { allow in await respondMCP(req, allow: allow) })
            }
        case "question":
            if let sid = it.sessionId { TermQuestionCard(sessionID: sid, item: it) }
        case "error":
            if let tid = it.threadId {
                ErrorNeedCard(item: it, working: working.contains(it.key),
                              onOpen: { store.openThread(tid) },
                              onDismiss: { await dismiss(it, tid) })
            }
        default:
            if let sid = it.sessionId {
                // The older daemon sends no question items, so its card polls for one as before.
                if it.legacy == true, agentKey(it.agent ?? "") == "claude" { TermQuestionCard(sessionID: sid) }
                SessionApprovalCard(
                    item: it,
                    waitingOnly: store.sessionsByID[sid]?.state == .awaitingInput,
                    working: working.contains(sid),
                    onKeys: { keys in await sendKeys(sid, keys) },
                    onAnswer: { yes in await answerPrompt(sid, yes: yes, key: it.promptKey) },
                    onReply: { text in await replyPane(sid, text: text) },
                    onDismiss: { await dismissPrompt(sid) }
                )
            }
        }
    }

    /// Notification taps carry an approval id, an MCP request id or a session id.
    private func focusID(_ it: NeedsYouItem) -> String {
        switch it.kind {
        case "approval", "error": return it.key
        case "mcp": return it.mcp?.id ?? it.key
        default: return it.sessionId ?? it.key
        }
    }

    @ViewBuilder private func focusRing(_ id: String) -> some View {
        if store.focusApproval == id {
            RoundedRectangle(cornerRadius: 18, style: .continuous).stroke(Tok.accent, lineWidth: 2.5)
                .allowsHitTesting(false)
        }
    }

    // MARK: responders

    private func respondMCP(_ req: PermissionRequest, allow: Bool) async {
        working.insert(req.id); defer { working.remove(req.id) }
        do {
            try await APIClient.shared.respondPermission(req.id, allow: allow)
            store.dropPermission(req.id)
            error = nil
        } catch let e as APIError where e.status == 409 || e.status == 404 {
            // 404: the request is gone (answered on another surface, or its caller ended).
            error = "Answered elsewhere."
            store.dropPermission(req.id)
        } catch {
            self.error = error.localizedDescription
            await store.loadNeedsYou()
        }
    }

    private func dismiss(_ it: NeedsYouItem, _ tid: String) async {
        working.insert(it.key); defer { working.remove(it.key) }
        do { try await APIClient.shared.drivenSettle(tid); error = nil }
        catch { self.error = error.localizedDescription }
        await store.loadNeedsYou()
    }

    private func sendKeys(_ sid: String, _ keys: [String]) async {
        working.insert(sid); defer { working.remove(sid) }
        do { try await APIClient.shared.keys(sid, keys); error = nil }
        catch { self.error = error.localizedDescription }
        await store.loadNeedsYou()
    }

    private func answerPrompt(_ sid: String, yes: Bool, key: String?) async {
        working.insert(sid); defer { working.remove(sid) }
        do { try await APIClient.shared.answerPrompt(sid, yes: yes, key: key); error = nil }
        catch { self.error = error.localizedDescription }
        await store.loadNeedsYou()
    }

    private func dismissPrompt(_ sid: String) async {
        working.insert(sid); defer { working.remove(sid) }
        do { try await APIClient.shared.dismissPrompt(sid); error = nil }
        catch { self.error = error.localizedDescription }
        await store.loadNeedsYou()
    }

    private func replyPane(_ sid: String, text: String) async {
        working.insert(sid); defer { working.remove(sid) }
        do { try await APIClient.shared.replyViaPane(sid, text: text); error = nil }
        catch { self.error = error.localizedDescription }
        await store.loadNeedsYou()
    }
}

// MARK: - MCP permission request card

struct PermissionCard: View {
    let req: PermissionRequest
    let working: Bool
    let onRespond: (Bool) async -> Void
    @State private var confirmHigh = false

    private var sessionName: String {
        guard let c = req.cwd, !c.isEmpty else { return req.sessionId ?? "unknown" }
        return (c as NSString).lastPathComponent
    }
    /// The same rule as hook approvals and the web: allowing a high-risk call asks to confirm first.
    private var high: Bool { req.risk == "high" }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(humanToolName(req.toolName)).font(Typo.mono(13, .semibold)).foregroundStyle(Tok.fg)
                Spacer()
                Text(relativeTime(req.createdAt)).font(Typo.xs).foregroundStyle(Tok.muted)
            }

            HStack(spacing: 8) {
                if let r = req.risk, !r.isEmpty { RiskBadge(risk: r) }
                Text(sessionName).font(Typo.sm).foregroundStyle(Tok.muted)
            }
            if let why = req.riskReason, !why.isEmpty, req.risk != "low" {
                (Text("Why \(req.risk ?? "") risk: ").bold() + Text(why))
                    .font(Typo.ui(12.5)).foregroundStyle(high ? Tok.err : Tok.fg2)
            }

            if !req.inputPairs.isEmpty {
                VStack(spacing: 4) {
                    ForEach(req.inputPairs.prefix(8), id: \.0) { pair in
                        MetaRow(label: pair.0, value: pair.1, mono: true)
                    }
                }
                .padding(8)
                .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.row))
            .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
            }

            HStack(spacing: 12) {
                Button { Task { await onRespond(false) } } label: {
                    Label("Deny", systemImage: "xmark").frame(maxWidth: .infinity)
                }
                .buttonStyle(.am(.deny))

                Button { if high { confirmHigh = true } else { Task { await onRespond(true) } } } label: {
                    Label("Allow", systemImage: "checkmark").frame(maxWidth: .infinity)
                }
                .buttonStyle(.am(.allow))
            }
            .disabled(working)
            .overlay { if working { ProgressView() } }
        }
        .askCard(high ? Tok.err : Tok.need)
        .confirmationDialog("High-risk request", isPresented: $confirmHigh, titleVisibility: .visible) {
            Button("Allow", role: .destructive) { Task { await onRespond(true) } }
        } message: {
            Text(req.riskReason?.isEmpty == false ? req.riskReason! : "Allow this high-risk call?")
        }
    }
}

// MARK: - terminal prompt card

// A permission prompt shown in an agent's terminal with no agent-monitor request behind it.
// Shows the bottom of the terminal and answers with the keys a person would press there.
struct SessionApprovalCard: View {
    let item: NeedsYouItem
    /// Older daemon only: the session ended its turn and waits for a reply.
    var waitingOnly = false
    let working: Bool
    let onKeys: ([String]) async -> Void
    let onAnswer: (Bool) async -> Void
    let onReply: (String) async -> Void
    var onDismiss: (() async -> Void)? = nil

    @State private var reply = ""
    @State private var capture = ""
    @State private var polling = true
    /// After Up or Down, Yes confirms the choice the user moved to, not choice 1.
    @State private var movedCursor = false
    @FocusState private var replyFocused: Bool

    private var sid: String { item.sessionId ?? "" }
    private var who: String { agentName(item.agent ?? "") }
    private var accent: Color { waitingOnly ? Tok.accent : Tok.need }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 6) {
                AgentGlyph(agent: item.agent ?? "", size: 14)
                Text(item.displayTitle).font(Typo.ui(14, .semibold)).foregroundStyle(Tok.fg).lineLimit(2)
                Spacer()
                Text(relativeTime(item.at)).font(Typo.xs).foregroundStyle(Tok.muted)
            }

            HStack(spacing: 6) {
                Circle().fill(accent).frame(width: 6, height: 6)
                Text(waitingOnly ? "\(who) is waiting on you" : "\(who) asks in the terminal")
                    .font(Typo.ui(12, .medium)).foregroundStyle(accent)
            }

            if !item.answerable {
                if let msg = item.message, !msg.isEmpty { promptBox(msg, mono: false) }
                Text(whyText(item.why ?? (item.legacy == true ? "Answer it in the terminal: agent-monitor cannot reach this session" : nil)))
                    .font(Typo.ui(12.5)).foregroundStyle(Tok.fg2)
                if let onDismiss, !waitingOnly {
                    Button { Task { await onDismiss() } } label: {
                        Text("Dismiss").lineLimit(1).frame(maxWidth: .infinity)
                    }.buttonStyle(.am(full: true)).disabled(working)
                }
            } else {
                // The bottom of the terminal: the prompt and its numbered options.
                promptBox(capture.isEmpty ? (item.message ?? "…") : cleanTail(capture, 16), mono: !capture.isEmpty)

                // A digit picks and confirms an option, so no Enter follows it.
                HStack(spacing: 8) {
                    ForEach(1...4, id: \.self) { n in
                        Button("\(n)") { Task { await onKeys(["\(n)"]) } }
                            .font(Typo.mono(14, .semibold))
                            .buttonStyle(.am(full: true))
                            .accessibilityLabel("Option \(n)")
                    }
                }
                .disabled(working)

                HStack(spacing: 8) {
                    Button { movedCursor = true; Task { await onKeys(["Up"]) } } label: { Image(systemName: "chevron.up") }
                        .buttonStyle(.am()).accessibilityLabel("Up")
                    Button { movedCursor = true; Task { await onKeys(["Down"]) } } label: { Image(systemName: "chevron.down") }
                        .buttonStyle(.am()).accessibilityLabel("Down")
                    Button { let m = movedCursor; movedCursor = false; Task { if m { await onKeys(["Enter"]) } else { await onAnswer(true) } } } label: {
                        Label("Yes", systemImage: "return").lineLimit(1).frame(maxWidth: .infinity)
                    }.buttonStyle(.am(.allow))
                    Button { movedCursor = false; Task { await onAnswer(false) } } label: {
                        Label("No", systemImage: "escape").lineLimit(1).frame(maxWidth: .infinity)
                    }.buttonStyle(.am(.deny))
                }
                .disabled(working)

                // Free text answers "No, and tell the agent what to do instead".
                HStack(spacing: 8) {
                    TextField("Type a reply…", text: $reply, axis: .vertical)
                        .amField().lineLimit(1...4)
                        .focused($replyFocused)
                    Button {
                        let t = reply; reply = ""; replyFocused = false
                        Task { await onReply(t) }
                    } label: { Image(systemName: "paperplane.fill") }
                        .buttonStyle(.am(.pri))
                        .disabled(reply.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || working)
                        .accessibilityLabel("Send reply")
                }
            }
        }
        .overlay(alignment: .topTrailing) { if working { ProgressView().padding(6) } }
        .askCard(accent)
        .task(id: sid) { if item.answerable && !sid.isEmpty { await pollCapture() } }
        .onChange(of: item.promptKey) { movedCursor = false } // a new prompt starts at choice 1
        .onDisappear { polling = false }
    }

    private func promptBox(_ text: String, mono: Bool) -> some View {
        Text(text)
            .font(mono ? Typo.mono(11) : Typo.sm)
            .foregroundStyle(mono ? Tok.fg : Tok.fg2)
            .lineLimit(mono ? nil : 5)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(8)
            .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.row))
            .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
    }

    // Strip ANSI/CSI escape sequences (defensive: the daemon's plain=1 already
    // removes them) and return the last `n` non-blank lines, i.e. the current
    // prompt at the bottom of the pane rather than the startup banner on top.
    private func cleanTail(_ s: String, _ n: Int) -> String {
        let stripped = s.replacingOccurrences(
            of: "\u{1B}\\[[0-9;?]*[ -/]*[@-~]", with: "", options: .regularExpression)
        var lines = stripped.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        while let last = lines.last, last.trimmingCharacters(in: .whitespaces).isEmpty { lines.removeLast() }
        if lines.count > n { lines = Array(lines.suffix(n)) }
        return lines.joined(separator: "\n")
    }

    // Poll the live pane so the shown prompt tracks what Claude is displaying.
    private func pollCapture() async {
        polling = true
        while polling && !Task.isCancelled {
            if let v = try? await APIClient.shared.paneView(sid, lines: 16), v.ok {
                capture = (v.content ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            }
            try? await Task.sleep(nanoseconds: 2_000_000_000)
        }
    }
}

// MARK: - Failed driven thread

/// A driven thread that stopped with an error: the same card as the web panel, with Open and Dismiss.
struct ErrorNeedCard: View {
    let item: NeedsYouItem
    let working: Bool
    let onOpen: () -> Void
    let onDismiss: () async -> Void

    private var lines: [Substring] { (item.message ?? "").split(separator: "\n", maxSplits: 1, omittingEmptySubsequences: false) }
    private var cause: String { lines.first.map(String.init) ?? "" }
    private var rest: String { lines.count > 1 ? String(lines[1]).trimmingCharacters(in: .whitespacesAndNewlines) : "" }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Button(action: onOpen) {
                HStack(alignment: .top, spacing: 10) {
                    AgentGlyph(agent: item.agent ?? "", size: 18)
                        .frame(width: 32, height: 32)
                        .background(Tok.panel3, in: Circle())
                    VStack(alignment: .leading, spacing: 2) {
                        Text(item.displayTitle).font(.headline).foregroundStyle(Tok.fg).lineLimit(2).multilineTextAlignment(.leading)
                        (Text("\(agentName(item.agent ?? "")) stopped with an error").foregroundColor(Tok.err)
                            + Text(" · " + relativeTime(item.at)).foregroundColor(Tok.muted))
                            .font(.subheadline.weight(.medium)).lineLimit(2)
                    }
                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            Text(cause).font(Typo.ui(13.5, .medium)).foregroundStyle(Tok.err)
            if !rest.isEmpty {
                DisclosureGroup {
                    AskPre(text: rest, lines: 12).padding(.top, 6)
                } label: {
                    Text("Details").font(Typo.ui(12, .medium)).foregroundStyle(Tok.fg2)
                }
                .tint(Tok.muted)
            }
            ActionRow {
                Button(action: onOpen) { Text("Open") }
                    .buttonStyle(.am(.allow, big: true, full: true))
                Button { Task { await onDismiss() } } label: { Text("Dismiss") }
                    .buttonStyle(.am(.deny, big: true, full: true))
            }
            .disabled(working)
        }
        .askCard(Tok.err)
    }
}
