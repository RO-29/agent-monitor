import SwiftUI

// One hook approval (approvals.go): a tool permission, an AskUserQuestion question,
// or an ExitPlanMode plan. Decisions go to POST /api/approvals/{id}/decide.
struct HookApprovalCard: View {
    @Environment(AppStore.self) private var store
    let a: Approval
    var showHeader = true
    /// The needs-you item's title (the session title), as the web card uses.
    var title: String? = nil
    var onOpen: (() -> Void)? = nil
    @State private var decided: String?

    @State private var error: String?
    @State private var denyOpen = false
    @State private var denyMsg = ""
    @State private var picked: [String: [String]] = [:]
    @State private var other: [String: String] = [:]
    @State private var confirmBody: DecideBody?
    @State private var showInput = false
    @State private var groupOpen = false
    @State private var groupBusy = false

    /// Pending requests that one Always rule would cover, this one included.
    private var sameGroup: [Approval] {
        guard let g = a.group, a.isPending else { return [] }
        return store.approvalsByID.values.filter { $0.group == g && $0.isPending && !$0.highRisk && !$0.sentToTerminal && store.pendingDecisions[$0.id] == nil }
            .sorted { $0.createdAt < $1.createdAt }
    }

    private var locked: Bool { a.sentToTerminal || store.pendingDecisions[a.id] != nil }
    private var accent: Color { a.highRisk ? Tok.err : Tok.need }

    var body: some View {
        VStack(alignment: .leading, spacing: 9) {
            if showHeader { header }
            if a.terminalShown {
                HStack(spacing: 6) {
                    Image(systemName: "terminal").font(.system(size: 11))
                    Text(a.sentToTerminal ? "Sent to the terminal" : "Also shown in the terminal")
                }
                .font(Typo.sm).foregroundStyle(Tok.fg2)
            }
            if let p = store.pendingDecisions[a.id] {
                // The same 3 s Undo as the Home card, the web and the Mac panel.
                DecisionBar(p: p) { withAnimation(.snappy) { store.undoDecision(a.id) } }
            } else {
                switch a.kind {
                case "question": questionBody
                case "plan": planBody
                default: permissionBody
                }
            }
            if denyOpen && a.kind != "plan" { denyBox }
            if let error = error ?? store.decisionErrors[a.id] {
                Text(error).font(Typo.ui(12.5)).foregroundStyle(Tok.err)
            }
        }
        .askCard(accent)
        .onAppear(perform: restoreDrafts)
        .sensoryFeedback(trigger: decided) { _, new in
            switch new {
            case "allow": return .success
            case "deny": return .warning
            case "error": return .error
            default: return nil
            }
        }
        .confirmationDialog("High-risk request", isPresented: Binding(get: { confirmBody != nil }, set: { if !$0 { confirmBody = nil } }),
                            titleVisibility: .visible, presenting: confirmBody) { b in
            Button(b.always == true ? "Always Allow" : "Allow", role: .destructive) { send(b, confirmed: true) }
        } message: { _ in
            Text(a.riskReason.isEmpty ? "Allow this high-risk call?" : a.riskReason)
        }
    }

    // MARK: header

    @ViewBuilder private var header: some View {
        let h = ApprovalHeading(a: a, title: (title?.isEmpty == false ? title : nil) ?? store.approvalTitle(a), tint: accent)
        if let onOpen {
            Button(action: onOpen) { h.contentShape(Rectangle()) }
                .buttonStyle(.plain)
                .accessibilityHint("Opens the thread")
        } else {
            h
        }
    }

    // MARK: permission

    private var permissionBody: some View {
        VStack(alignment: .leading, spacing: 9) {
            HStack(spacing: 8) {
                RiskBadge(risk: a.risk)
                Text(a.displayTool).font(Typo.mono(12, .medium)).foregroundStyle(Tok.fg2).lineLimit(1)
            }
            if let diff = EditDiff.lines(tool: a.toolName, input: a.input) {
                EditDiffView(lines: diff)
            }
            AskPre(text: a.command ?? a.summary, lines: EditDiff.lines(tool: a.toolName, input: a.input) == nil ? 12 : 2)
                .contextMenu {
                    Button { UIPasteboard.general.string = a.command ?? a.summary } label: { Label("Copy", systemImage: "doc.on.doc") }
                }
            if !a.riskReason.isEmpty && a.risk != "low" {
                (Text("Why \(a.risk) risk: ").bold() + Text(a.riskReason))
                    .font(Typo.ui(12.5))
                    .foregroundStyle(a.risk == "high" ? Tok.err : Tok.fg2)
                    .padding(.horizontal, 10).padding(.vertical, 6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.row))
                    .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(a.risk == "high" ? Tok.err.opacity(0.5) : Tok.line, lineWidth: 1))
            }
            if !a.inputPairs.isEmpty {
                DisclosureGroup(isExpanded: $showInput) {
                    VStack(spacing: 4) {
                        ForEach(a.inputPairs.prefix(12), id: \.0) { p in MetaRow(label: p.0, value: p.1, mono: true) }
                    }
                    .padding(.top, 6)
                } label: {
                    Text("Full input").font(Typo.ui(12, .medium)).foregroundStyle(Tok.fg2)
                }
                .tint(Tok.muted)
            }
            if let opts = a.options, !opts.isEmpty {
                // ACP agents offer their own option list.
                VStack(spacing: 8) {
                    ForEach(opts) { o in
                        Button {
                            var b = DecideBody(behavior: o.allows ? "allow" : "deny")
                            b.optionId = o.optionId
                            send(b)
                        } label: { Text(o.name) }
                        .buttonStyle(.am(o.allows ? .allow : .deny, big: true, full: true))
                        .disabled(locked)
                    }
                }
            } else {
                ActionRow {
                    Button { send(.allow()) } label: { Label("Allow", systemImage: "checkmark") }
                    .buttonStyle(.am(.allow, big: true, full: true))
                    Button { withAnimation(.snappy) { denyOpen.toggle() } } label: { Label("Deny", systemImage: "xmark") }
                        .buttonStyle(.am(.deny, big: true, full: true))
                }
                .disabled(locked)
                if sameGroup.count > 1 { groupBox }
                if a.canAlways {
                    Button { send(DecideBody(behavior: "allow", always: true)) } label: {
                        Text("Always allow")
                    }
                    .buttonStyle(.am(.normal, big: true, full: true))
                    .disabled(locked)
                }
            }
        }
    }

    // MARK: question

    private var questionBody: some View {
        let qs = a.parsedQuestions
        let single = qs.count == 1 && !qs[0].multiSelect
        let typed = other.values.contains { !$0.trimmingCharacters(in: .whitespaces).isEmpty }
        return VStack(alignment: .leading, spacing: 12) {
            if qs.isEmpty { Text(a.summary).font(Typo.prose).foregroundStyle(Tok.fg) }
            ForEach(qs, id: \.question) { q in
                VStack(alignment: .leading, spacing: 7) {
                    QuestionTitle(q: q)
                    VStack(spacing: 5) {
                        ForEach(q.options, id: \.label) { o in
                            optionButton(q, o, single: single)
                        }
                    }
                    VoiceField(placeholder: "Other: type or say an answer", text: Binding(
                        get: { other[q.question] ?? "" },
                        set: { other[q.question] = $0; DraftStore.shared.set(draftKey(q.question), text: $0) }),
                        disabled: locked) { submitAnswers(qs) }
                }
            }
            ActionRow {
                if !single || typed {
                    Button { submitAnswers(qs) } label: { Text(qs.count > 1 ? "Send (\(answers(qs).count)/\(qs.count))" : "Send") }
                        .buttonStyle(.am(.pri, big: true, full: true))
                        .disabled(answers(qs).isEmpty)
                }
                Button { send(.deny("The user declined to answer.")) } label: { Text("Decline") } // the web's Decline
                    .buttonStyle(.am(.deny, big: true, full: true))
            }
            .disabled(locked)
        }
    }

    private func optionButton(_ q: ApprovalQuestion, _ o: ApprovalOption, single: Bool) -> some View {
        let on = (picked[q.question] ?? []).contains(o.label)
        return Button {
            var cur = picked[q.question] ?? []
            if q.multiSelect {
                if on { cur.removeAll { $0 == o.label } } else { cur.append(o.label) }
            } else {
                cur = [o.label]
            }
            picked[q.question] = cur
            if single { submitAnswers(a.parsedQuestions) }
        } label: {
            AskOption(label: o.label, detail: o.description, on: on, multi: q.multiSelect)
        }
        .buttonStyle(.plain)
        .disabled(locked)
    }

    /// Answers keyed by question text; picked labels and the typed "Other" text join with ", ".
    private func answers(_ qs: [ApprovalQuestion]) -> [String: String] {
        var out: [String: String] = [:]
        for q in qs {
            var p = picked[q.question] ?? []
            let t = (other[q.question] ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            if !t.isEmpty { p.append(t) }
            if !p.isEmpty { out[q.question] = p.joined(separator: ", ") }
        }
        return out
    }

    private func submitAnswers(_ qs: [ApprovalQuestion]) {
        let out = answers(qs)
        guard !out.isEmpty else { return }
        var b = DecideBody.allow()
        b.answers = out
        send(b)
    }

    // MARK: plan

    private var planBody: some View {
        VStack(alignment: .leading, spacing: 9) {
            PlanBox(text: a.planText)
            TextField("Optional: what should change in the plan?",
                      text: Binding(get: { denyMsg }, set: { denyMsg = $0; DraftStore.shared.set(draftKey("deny"), text: $0) }),
                      axis: .vertical)
                .lineLimit(1...4)
                .amField()
            ActionRow {
                Button {
                    var b = DecideBody.allow()
                    b.updatedInput = a.input ?? [:]
                    send(b)
                } label: { Text("Approve plan") }
                    .buttonStyle(.am(.allow, big: true, full: true))
                Button { send(.deny(trimmed(denyMsg) ?? "Keep planning. Do not start yet.")) } label: { Text("Keep planning") }
                    .buttonStyle(.am(.normal, big: true, full: true))
            }
            .disabled(locked)
        }
    }

    // MARK: deny

    private var denyBox: some View {
        VStack(alignment: .leading, spacing: 8) {
            TextField(a.kind == "plan" ? "What should change in the plan?" : "Tell the agent why (optional)",
                      text: Binding(get: { denyMsg }, set: { denyMsg = $0; DraftStore.shared.set(draftKey("deny"), text: $0) }),
                      axis: .vertical)
                .lineLimit(1...4)
                .amField()
            ActionRow {
                Button { send(.deny(trimmed(denyMsg))) } label: {
                    Text(a.kind == "plan" ? "Send" : "Deny")
                }
                .buttonStyle(.am(.deny, big: true, full: true))
                if a.agent == "claude" && a.kind == "permission" {
                    Button { send(.deny(trimmed(denyMsg), interrupt: true)) } label: { Text("Deny and stop") }
                        .buttonStyle(.am(.deny, big: true, full: true))
                }
            }
            .disabled(locked)
        }
    }

    private func draftKey(_ field: String) -> String { "ask:\(a.id):\(field)" }

    private func restoreDrafts() {
        for q in a.parsedQuestions where other[q.question] == nil {
            let t = DraftStore.shared.text(draftKey(q.question))
            if !t.isEmpty { other[q.question] = t }
        }
        if denyMsg.isEmpty {
            denyMsg = DraftStore.shared.text(draftKey("deny"))
            if !denyMsg.isEmpty { denyOpen = true }
        }
    }

    private func trimmed(_ s: String) -> String? {
        let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        return t.isEmpty ? nil : t
    }

    @ViewBuilder private var groupBox: some View {
        let list = sameGroup
        if groupOpen {
            VStack(alignment: .leading, spacing: 6) {
                Text("\(list.count) requests match \(a.groupPattern ?? a.toolName) in \(a.project). Each one is allowed once; no rule is added.")
                    .font(.footnote).foregroundStyle(Tok.fg2)
                ForEach(list, id: \.id) { x in
                    Text(x.summary).font(.caption.monospaced()).lineLimit(2).foregroundStyle(Tok.fg)
                }
                HStack {
                    Button("Allow all \(list.count)") { Task { await allowGroup(list) } }
                        .buttonStyle(.am(.allow))
                    Button("Cancel") { groupOpen = false }.buttonStyle(.am(.ghost))
                }
                .disabled(groupBusy)
            }
            .padding(10)
            .background(Tok.panel2, in: RoundedRectangle(cornerRadius: Radius.row))
        } else {
            Button("Allow these \(list.count)…") { groupOpen = true }
                .buttonStyle(.am(.normal, big: true, full: true))
                .disabled(locked)
        }
    }

    private func allowGroup(_ list: [Approval]) async {
        groupBusy = true
        defer { groupBusy = false; groupOpen = false }
        var failed = 0
        for x in list {
            do { try await store.decide(x, .allow()) } catch { failed += 1 }
        }
        ToastCenter.shared.show(failed == 0 ? "Allowed \(list.count) requests" : "\(list.count - failed) allowed, \(failed) not",
                                icon: failed == 0 ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
    }

    private func send(_ body: DecideBody, confirmed: Bool = false) {
        guard !locked, store.pendingDecisions[a.id] == nil else { return }
        if !confirmed, a.needsConfirm(body) { confirmBody = body; return }
        DraftStore.shared.clear(prefix: "ask:\(a.id):")
        error = nil
        denyOpen = false
        decided = body.behavior == "allow" ? "allow" : "deny"
        withAnimation(.snappy) { store.queueDecision(a, body, label: decisionLabel(body)) }
    }

    private func decisionLabel(_ b: DecideBody) -> String {
        if let ans = b.answers, !ans.isEmpty { return ans.values.sorted().joined(separator: ", ") }
        switch (a.kind, b.behavior) {
        case ("plan", "allow"): return "Approve plan"
        case ("plan", _): return "Keep planning"
        case ("question", _): return "Decline"
        case (_, "allow"): return b.always == true ? "Always allow" : "Allow"
        default: return "Deny"
        }
    }
}
