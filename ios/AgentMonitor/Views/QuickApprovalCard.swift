import SwiftUI

// A hook approval on the home list, answerable in one tap: Allow or Deny, an option, or Approve plan.
// Anything that needs typing opens the thread, where the full card lives.
struct QuickApprovalCard: View {
    @Environment(AppStore.self) private var store
    let a: Approval

    @State private var confirmBody: (body: DecideBody, label: String)?

    private var tint: Color { a.highRisk ? Tok.err : Tok.need }
    private var pending: PendingDecision? { store.pendingDecisions[a.id] }
    private var locked: Bool { pending != nil || a.sentToTerminal }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Button(action: open) {
                ApprovalHeading(a: a, title: store.approvalTitle(a), tint: tint).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityHint("Opens the thread")

            if let p = pending {
                DecisionBar(p: p) { withAnimation(.snappy) { store.undoDecision(a.id) } }
            } else {
                switch a.kind {
                case "question": question
                case "plan": plan
                default: permission
                }
            }
            if let error = store.decisionErrors[a.id] {
                Text(error).font(.footnote).foregroundStyle(Tok.err)
            }
        }
        .askCard(tint)
        .disabled(a.sentToTerminal)
        .onDisappear { store.flushDecision(a.id) }
        .contextMenu {
            Button(action: open) { Label("Open Thread", systemImage: "bubble.left") }
            if let c = a.command {
                Button { copyToPasteboard(cleanCommand(c), label: "Command copied") } label: { Label("Copy Command", systemImage: "terminal") }
            }
            if a.canAlways, a.options == nil, !locked {
                Button { send(DecideBody(behavior: "allow", always: true), label: "Always allow") } label: { Label("Always Allow \(a.displayTool)", systemImage: "checkmark.seal") }
            }
        }
        .confirmationDialog("High-risk request", isPresented: Binding(get: { confirmBody != nil }, set: { if !$0 { confirmBody = nil } }),
                            titleVisibility: .visible, presenting: confirmBody) { c in
            Button(c.label, role: .destructive) {
                store.queueDecision(a, c.body, label: c.label)
                store.flushDecision(a.id)
            }
        } message: { _ in
            Text(a.riskReason.isEmpty ? "Allow this high-risk call?" : a.riskReason)
        }
    }

    // MARK: kinds

    private var permission: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(a.command ?? a.summary)
                .font(.subheadline.monospaced())
                .foregroundStyle(Tok.fg)
                .lineLimit(4)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 12).padding(.vertical, 10)
                .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
            if a.highRisk, !a.riskReason.isEmpty {
                Label(a.riskReason, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote).foregroundStyle(Tok.err)
            }
            if let opts = a.options, !opts.isEmpty {
                // Every option the agent offers, as on the Approvals tab; two fit on one row.
                let buttons = ForEach(opts) { o in
                    Button {
                        var b = DecideBody(behavior: o.allows ? "allow" : "deny")
                        b.optionId = o.optionId
                        send(b, label: o.name)
                    } label: { Text(o.name) }
                    .buttonStyle(.am(o.allows ? .allow : .deny, big: true, full: true))
                }
                if opts.count <= 2 { ActionRow { buttons } } else { VStack(spacing: 8) { buttons } }
            } else {
                ActionRow {
                    Button { send(.deny(), label: "Deny") } label: { Text("Deny") }
                        .buttonStyle(.am(.deny, big: true, full: true))
                    Button { send(.allow(), label: "Allow") } label: { Text("Allow") }
                        .buttonStyle(.am(.allow, big: true, full: true))
                }
            }
        }
    }

    /// The full card's question layout cut to two options; "Answer" opens the full card.
    @ViewBuilder private var question: some View {
        let qs = a.parsedQuestions
        if let q = qs.first {
            let oneTap = qs.count == 1 && !q.multiSelect
            VStack(alignment: .leading, spacing: 7) {
                QuestionTitle(q: q)
                ForEach(q.options.prefix(2), id: \.label) { o in
                    Button {
                        if oneTap {
                            var b = DecideBody.allow()
                            b.answers = [q.question: o.label]
                            send(b, label: o.label)
                        } else {
                            store.approvalSheet = a.id
                        }
                    } label: {
                        AskOption(label: o.label, detail: o.description, on: false, multi: q.multiSelect)
                    }
                    .buttonStyle(.plain)
                }
                Button { store.approvalSheet = a.id } label: {
                    Text(answerLabel(qs))
                }
                .buttonStyle(.am(.pri, big: true, full: true))
                .padding(.top, 5)
            }
        } else {
            Text(a.summary).font(.body).foregroundStyle(Tok.fg)
            Button { store.approvalSheet = a.id } label: { Text("Answer") }
                .buttonStyle(.am(.pri, big: true, full: true))
        }
    }

    private func answerLabel(_ qs: [ApprovalQuestion]) -> String {
        let more = (qs.first?.options.count ?? 0) - 2
        if qs.count > 1 { return "Answer \(qs.count) questions" }
        return more > 0 ? "Answer: \(more) more option\(more == 1 ? "" : "s")" : "Answer"
    }

    private var plan: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(planPreview)
                .font(.subheadline).foregroundStyle(Tok.fg2)
                .lineLimit(5)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 12).padding(.vertical, 10)
                .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
            ActionRow {
                Button(action: open) { Text("Read Plan") }
                    .buttonStyle(.am(.normal, big: true, full: true))
                Button {
                    var b = DecideBody.allow()
                    b.updatedInput = a.input ?? [:]
                    send(b, label: "Approve plan")
                } label: { Text("Approve") }
                    .buttonStyle(.am(.allow, big: true, full: true))
            }
        }
    }

    private var planPreview: String {
        a.planText.components(separatedBy: "\n")
            .map { $0.trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: "#")).trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
            .joined(separator: "\n")
    }

    // MARK: actions

    private func open() { store.openThread(store.threadID(of: a)) }

    private func send(_ body: DecideBody, label: String) {
        guard !locked else { return }
        if a.needsConfirm(body) { confirmBody = (body, label); return }
        withAnimation(.snappy) { store.queueDecision(a, body, label: label) }
    }
}

/// A question's title row, shared by the home card and the full card: header pill, question, "pick any".
struct QuestionTitle: View {
    let q: ApprovalQuestion

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            if let h = q.header, !h.isEmpty { Pill(text: h) }
            Text(q.question).font(.body.weight(.semibold)).foregroundStyle(Tok.fg)
            if q.multiSelect { Text("pick any").font(Typo.xs).foregroundStyle(Tok.muted) }
        }
    }
}

/// The full approval card in a sheet, opened by "Answer" on a home card; it closes once the approval is answered.
struct ApprovalSheet: View {
    @Environment(AppStore.self) private var store
    @Environment(\.dismiss) private var dismiss
    let id: String

    var body: some View {
        NavigationStack {
            ScrollView {
                if let a = store.approvalsByID[id] {
                    HookApprovalCard(a: a, onOpen: {
                        dismiss()
                        store.openThread(store.threadID(of: a))
                    })
                    .padding(16)
                }
            }
            .background(Tok.bg)
            .navigationTitle("Answer")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Close") { dismiss() } } }
            .onChange(of: store.approvalsByID[id] == nil) { _, gone in if gone { dismiss() } }
        }
    }
}

extension AppStore {
    /// Heading of an approval card: the thread title, else the folder, else the agent.
    func approvalTitle(_ a: Approval) -> String {
        thread(threadID(of: a))?.name ?? (a.project.isEmpty ? agentName(a.agent) : a.project)
    }
}

/// The heading both approval cards share: agent glyph, thread title on up to 2 lines, then "what it wants · age".
struct ApprovalHeading: View {
    let a: Approval
    let title: String
    let tint: Color

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            AgentGlyph(agent: a.agent, size: 18)
                .frame(width: 32, height: 32)
                .background(Tok.panel3, in: Circle())
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.headline).foregroundStyle(Tok.fg).lineLimit(2).multilineTextAlignment(.leading)
                (Text(a.headline).foregroundColor(tint) + Text(" · " + relativeTime(a.createdAt)).foregroundColor(Tok.muted))
                    .font(.subheadline.weight(.medium)).lineLimit(2)
            }
            .layoutPriority(1)
            Spacer(minLength: 0)
        }
    }
}

/// "Sending: <answer> · Undo" during the undo window, then "Sending" while the POST runs, then "Sent".
struct DecisionBar: View {
    let p: PendingDecision
    let undo: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 10) {
                switch p.phase {
                case .sent: Image(systemName: "checkmark.circle.fill").foregroundStyle(Tok.ok)
                case .sending: ProgressView().controlSize(.small)
                case .waiting: Image(systemName: "clock").foregroundStyle(Tok.muted)
                }
                Text("\(p.phase == .sent ? "Sent" : "Sending"): \(p.label)")
                    .font(.body.weight(.medium)).foregroundStyle(Tok.fg).lineLimit(2)
                Spacer(minLength: 4)
                if p.phase == .waiting {
                    Button("Undo") { undo() }
                        .buttonStyle(.am(.normal, big: true))
                }
            }
            if p.phase == .waiting {
                ProgressView(timerInterval: p.start...p.start.addingTimeInterval(AppStore.undoWindow), countsDown: true) {
                    EmptyView()
                } currentValueLabel: { EmptyView() }
                .tint(Tok.muted)
            }
        }
        .accessibilityElement(children: .contain)
    }
}
