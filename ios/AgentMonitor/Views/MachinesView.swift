import SwiftUI

// Other machines' requests in Approvals: one card each, with the machine name; the owning daemon gets the decision.
struct RemoteNeedsSection: View {
    @Environment(AppStore.self) private var store

    var body: some View {
        ForEach(store.machines) { m in
            let items = store.remoteNeeds[m.id] ?? []
            if !items.isEmpty || store.remoteDown[m.id] != nil {
                VStack(alignment: .leading, spacing: 10) {
                    HStack(spacing: 6) {
                        Image(systemName: "desktopcomputer").font(.system(size: 12, weight: .semibold))
                        Text(m.name).font(Typo.meta.weight(.semibold))
                        if !items.isEmpty { Text("\(items.count)").font(Typo.sm).foregroundStyle(Tok.muted) }
                    }
                    .foregroundStyle(Tok.fg2)
                    if let e = store.remoteDown[m.id] {
                        Label("Cannot reach \(m.name): \(e)", systemImage: "wifi.exclamationmark")
                            .font(Typo.sm).foregroundStyle(Tok.err)
                    }
                    ForEach(items) { RemoteNeedCard(machine: m, item: $0) }
                }
            }
        }
    }
}

struct RemoteNeedCard: View {
    @Environment(AppStore.self) private var store
    let machine: Machine
    let item: NeedsYouItem
    @State private var working = false
    @State private var error: String?

    private var canDecide: Bool { item.answerable && ["approval", "mcp", "prompt"].contains(item.kind) }
    private var detail: String {
        if let a = item.approval { return a.summary.isEmpty ? a.toolName : "\(a.toolName): \(a.summary)" }
        if let m = item.mcp { return m.toolName }
        return item.why ?? item.message ?? ""
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 9) {
            HStack(spacing: 7) {
                AgentGlyph(agent: item.agent ?? "")
                Text(item.displayTitle).font(Typo.body.weight(.semibold)).lineLimit(1)
                Spacer(minLength: 4)
                Text(machine.name)
                    .font(Typo.xs.weight(.semibold)).foregroundStyle(Tok.accent)
                    .padding(.horizontal, 7).padding(.vertical, 3)
                    .background(Tok.accentSoft, in: Capsule())
            }
            if !detail.isEmpty {
                Text(detail).font(Typo.sm.monospaced()).foregroundStyle(Tok.fg2).lineLimit(4)
            }
            if canDecide {
                HStack(spacing: 10) {
                    Button { decide(true) } label: { Text("Allow").frame(maxWidth: .infinity) }
                        .buttonStyle(.borderedProminent).tint(Tok.ok)
                    Button { decide(false) } label: { Text("Deny").frame(maxWidth: .infinity) }
                        .buttonStyle(.bordered).tint(Tok.err)
                }
                .disabled(working)
            } else {
                Text("Answer this on \(machine.name).").font(Typo.sm).foregroundStyle(Tok.muted)
            }
            if let error { Text(error).font(Typo.ui(12.5)).foregroundStyle(Tok.err) }
        }
        .askCard(item.approval?.highRisk == true ? Tok.err : Tok.need)
    }

    private func decide(_ allow: Bool) {
        working = true
        Task {
            defer { working = false }
            do { try await store.decideRemote(machine, item, allow: allow); error = nil }
            catch let e as APIError where e.status == 403 {
                error = "\(machine.name) lets this phone view only. Raise its level in Settings, Machines on that Mac."
            } catch { self.error = error.localizedDescription }
        }
    }
}

// Settings, Machines: the other Macs this phone watches, each with its own token.
struct MachinesSettingsView: View {
    @Environment(AppStore.self) private var store
    @State private var name = ""
    @State private var link = ""
    @State private var adding = false
    @State private var msg: (String, Bool)?

    var body: some View {
        Form {
            Section {
                if store.machines.isEmpty {
                    Text("No other machines yet.").foregroundStyle(Tok.muted)
                }
                ForEach(store.machines) { m in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack {
                            Text(m.name).fontWeight(.semibold)
                            Spacer()
                            if let e = store.remoteDown[m.id] {
                                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(Tok.err).accessibilityLabel(e)
                            } else if store.remoteNeeds[m.id] != nil {
                                Image(systemName: "checkmark.circle.fill").foregroundStyle(Tok.ok).accessibilityLabel("Reachable")
                            }
                        }
                        Text(m.url).font(.footnote.monospaced()).foregroundStyle(Tok.muted)
                        Text(PairLink.mask(m.token ?? "")).font(.footnote.monospaced()).foregroundStyle(Tok.muted)
                    }
                }
                .onDelete { idx in
                    for i in idx { Machines.remove(store.machines[i].id) }
                    store.reloadMachines()
                }
            } header: {
                Text("Machines")
            } footer: {
                Text("Swipe left to remove a machine and delete its token from this phone. Its requests show in Approvals with its name.")
            }
            .listRowBackground(Tok.panel)

            Section {
                TextField("Name, for example studio", text: $name)
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                TextField("Pair link from that Mac", text: $link, axis: .vertical)
                    .font(.subheadline.monospaced())
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                    .lineLimit(1...3)
                Button {
                    if let s = UIPasteboard.general.string { link = s }
                } label: { Label("Paste from clipboard", systemImage: "doc.on.clipboard") }
                Button { add() } label: {
                    HStack {
                        Text(adding ? "Adding" : "Add machine").fontWeight(.semibold)
                        Spacer()
                        if adding { ProgressView() }
                    }
                }
                .disabled(adding || link.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                if let (m, ok) = msg {
                    Label(m, systemImage: ok ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                        .font(.footnote).foregroundStyle(ok ? Tok.ok : Tok.err)
                }
            } header: {
                Text("Add")
            } footer: {
                Text("On the other Mac, open Settings, Devices, add this phone and copy its pair link. To limit the phone there, set its level in Settings, Machines on that Mac.")
            }
            .listRowBackground(Tok.panel)
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Machines")
    }

    private func add() {
        adding = true
        Task {
            defer { adding = false }
            do {
                let m = try await Machines.add(name: name, link: link)
                store.reloadMachines()
                await store.loadRemote(m)
                if let e = store.remoteDown[m.id] {
                    msg = ("Added \(m.name), but it does not answer: \(e)", false)
                } else {
                    msg = ("Added \(m.name).", true)
                }
                name = ""; link = ""
            } catch {
                msg = (error.localizedDescription, false)
            }
        }
    }
}
