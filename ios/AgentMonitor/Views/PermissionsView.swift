import SwiftUI

// Unified Approvals tab. Two sources of "needs a human":
//   1. MCP permission requests (/api/permissions) — answered tmux-free via
//      /api/permission/:id/respond. Exact allow/deny semantics.
//   2. Sessions parked in awaiting-permission / awaiting-input (from the
//      Notification/Stop hooks) — a native TUI prompt. Answered over the tmux
//      pane bridge (Enter = allow, Esc = deny, free text = reply).
struct PermissionsView: View {
    @Environment(AppStore.self) private var store
    @State private var error: String?
    @State private var working: Set<String> = []

    var body: some View {
        NavigationStack {
            Group {
                if store.permissions.isEmpty && store.awaitingSessions.isEmpty {
                    EmptyHint(icon: "checkmark.shield",
                              title: "No pending approvals",
                              subtitle: "Permission prompts and agents waiting for input appear here in real time.")
                } else {
                    ScrollView {
                        if let error { ErrorBanner(message: error) }
                        VStack(spacing: 12) {
                            ForEach(store.permissions) { req in
                                PermissionCard(
                                    req: req,
                                    working: working.contains(req.id),
                                    onRespond: { allow in await respondMCP(req, allow: allow) }
                                )
                            }
                            ForEach(store.awaitingSessions) { s in
                                SessionApprovalCard(
                                    session: s,
                                    hasPane: store.hasPane(s.id),
                                    working: working.contains(s.id),
                                    onKeys: { keys in await sendKeys(s, keys) },
                                    onReply: { text in await replyPane(s, text: text) }
                                )
                            }
                        }
                        .padding()
                    }
                }
            }
            .navigationTitle("Approvals")
            .toolbar { ToolbarItem(placement: .topBarTrailing) { ConnectionChip() } }
            .refreshable { await store.hydrate() }
        }
    }

    // MARK: responders

    private func respondMCP(_ req: PermissionRequest, allow: Bool) async {
        working.insert(req.id); defer { working.remove(req.id) }
        do {
            try await APIClient.shared.respondPermission(req.id, allow: allow)
            store.dropPermission(req.id)
            error = nil
        } catch { self.error = error.localizedDescription }
    }

    private func sendKeys(_ s: Session, _ keys: [String]) async {
        working.insert(s.id); defer { working.remove(s.id) }
        do { try await APIClient.shared.keys(s.id, keys); error = nil }
        catch { self.error = error.localizedDescription }
    }

    private func replyPane(_ s: Session, text: String) async {
        working.insert(s.id); defer { working.remove(s.id) }
        do { try await APIClient.shared.replyViaPane(s.id, text: text); error = nil }
        catch { self.error = error.localizedDescription }
    }
}

// MARK: - MCP permission request card (tmux-free)

struct PermissionCard: View {
    let req: PermissionRequest
    let working: Bool
    let onRespond: (Bool) async -> Void

    private var sessionName: String {
        guard let c = req.cwd, !c.isEmpty else { return req.sessionId ?? "unknown" }
        return (c as NSString).lastPathComponent
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Image(systemName: "shield.lefthalf.filled").foregroundStyle(.orange)
                Text(req.toolName).font(.headline)
                Spacer()
                Tag(text: "tmux-free", color: .green)
                Text(relativeTime(req.createdAt)).font(.caption2).foregroundStyle(.secondary)
            }

            Text(sessionName).font(.caption).foregroundStyle(.secondary)

            if !req.inputPairs.isEmpty {
                VStack(spacing: 4) {
                    ForEach(req.inputPairs.prefix(8), id: \.0) { pair in
                        MetaRow(label: pair.0, value: pair.1, mono: true)
                    }
                }
                .padding(8)
                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
            }

            HStack(spacing: 12) {
                Button { Task { await onRespond(false) } } label: {
                    Label("Deny", systemImage: "xmark").frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered).tint(.red)

                Button { Task { await onRespond(true) } } label: {
                    Label("Allow", systemImage: "checkmark").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent).tint(.green)
            }
            .disabled(working)
            .overlay { if working { ProgressView() } }
        }
        .padding()
        .background(Color.orange.opacity(0.06), in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.orange.opacity(0.25)))
    }
}

// MARK: - hook-detected awaiting-session card (tmux pane bridge)

// Card for a session parked at one of Claude's terminal prompts — a Yes/No
// permission prompt or an AskUserQuestion multiple-choice. Shows the LIVE pane
// so you see the exact question + numbered options, and answers it by driving
// the tmux pane: tap an option number, Yes (Enter) / No (Esc), or type a reply.
struct SessionApprovalCard: View {
    let session: Session
    let hasPane: Bool
    let working: Bool
    let onKeys: ([String]) async -> Void
    let onReply: (String) async -> Void

    @State private var reply = ""
    @State private var capture = ""
    @State private var polling = true
    @FocusState private var replyFocused: Bool

    private var isPermission: Bool { session.state == .awaitingPermission }
    private var accent: Color { isPermission ? .orange : .blue }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Image(systemName: Theme.icon(for: session.tool)).foregroundStyle(accent)
                Text(session.project).font(.headline).lineLimit(1)
                Spacer()
                Tag(text: hasPane ? "live" : "no pane", color: hasPane ? .green : .gray)
                Text(relativeTime(session.lastActivityAt)).font(.caption2).foregroundStyle(.secondary)
            }

            HStack(spacing: 6) {
                Circle().fill(accent).frame(width: 6, height: 6)
                Text(isPermission ? "Claude is asking permission" : "Claude is waiting on you")
                    .font(.caption.weight(.medium)).foregroundStyle(accent)
            }

            if !hasPane {
                if let msg = session.permissionMessage ?? session.lastMessage, !msg.isEmpty {
                    promptBox(msg, mono: false)
                }
                Text("This session isn't in a registered tmux pane, so its prompt can't be answered remotely. Run it inside tmux (or via `agent-monitor run`) to answer Claude's questions from here.")
                    .font(.caption2).foregroundStyle(.secondary)
            } else {
                // The live terminal — the actual prompt + its options (bottom
                // of the pane, escapes stripped).
                promptBox(capture.isEmpty ? "…" : cleanTail(capture, 16), mono: true)

                // Option numbers cover both Yes/No permission menus and
                // AskUserQuestion choices. A digit selects+confirms in Claude's
                // TUI, so no trailing Enter.
                HStack(spacing: 8) {
                    ForEach(1...4, id: \.self) { n in
                        Button("\(n)") { Task { await onKeys(["\(n)"]) } }
                            .font(.callout.monospaced().weight(.semibold))
                            .frame(maxWidth: .infinity)
                            .buttonStyle(.bordered)
                    }
                }
                .disabled(working)

                HStack(spacing: 8) {
                    Button { Task { await onKeys(["Up"]) } } label: { Image(systemName: "chevron.up") }
                        .buttonStyle(.bordered)
                    Button { Task { await onKeys(["Down"]) } } label: { Image(systemName: "chevron.down") }
                        .buttonStyle(.bordered)
                    Button { Task { await onKeys(["Enter"]) } } label: {
                        Label("Yes / Enter", systemImage: "return").frame(maxWidth: .infinity)
                    }.buttonStyle(.borderedProminent).tint(.green)
                    Button { Task { await onKeys(["Escape"]) } } label: {
                        Label("No / Esc", systemImage: "escape").frame(maxWidth: .infinity)
                    }.buttonStyle(.bordered).tint(.red)
                }
                .disabled(working)

                // Free-text reply — for "No, tell Claude what to do differently"
                // or answering an awaiting-input prompt.
                HStack(spacing: 8) {
                    TextField("Type a reply…", text: $reply, axis: .vertical)
                        .textFieldStyle(.roundedBorder).lineLimit(1...4)
                        .focused($replyFocused)
                    Button {
                        let t = reply; reply = ""; replyFocused = false
                        Task { await onReply(t) }
                    } label: { Image(systemName: "paperplane.fill") }
                        .buttonStyle(.borderedProminent)
                        .disabled(reply.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || working)
                }
            }
        }
        .overlay(alignment: .topTrailing) { if working { ProgressView().padding(6) } }
        .padding()
        .background(accent.opacity(0.06), in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(accent.opacity(0.25)))
        .task(id: session.id) { if hasPane { await pollCapture() } }
        .onDisappear { polling = false }
    }

    private func promptBox(_ text: String, mono: Bool) -> some View {
        Text(text)
            .font(mono ? .system(size: 11, design: .monospaced) : .caption)
            .foregroundStyle(mono ? .primary : .secondary)
            .lineLimit(mono ? nil : 5)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(8)
            .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
    }

    // Strip ANSI/CSI escape sequences (defensive — the daemon's plain=1 already
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
            if let v = try? await APIClient.shared.paneView(session.id, lines: 16), v.ok {
                capture = (v.content ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            }
            try? await Task.sleep(nanoseconds: 2_000_000_000)
        }
    }
}

// Small rounded tag used on the cards.
private struct Tag: View {
    let text: String; let color: Color
    var body: some View {
        Text(text.uppercased())
            .font(.system(size: 8, weight: .bold)).tracking(0.4)
            .padding(.horizontal, 6).padding(.vertical, 2)
            .background(color.opacity(0.16), in: Capsule()).foregroundStyle(color)
    }
}
