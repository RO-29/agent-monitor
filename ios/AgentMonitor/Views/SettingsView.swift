import SwiftUI

struct SettingsView: View {
    @Environment(AppStore.self) private var store
    @State private var pairLink = ""
    @State private var pairing = false
    @State private var pairMsg: (String, Bool)?
    @State private var healthMsg: (String, Bool)?
    @State private var checking = false
    @State private var savedToken = PairLink.mask(Config.password)
    @State private var pairedHaptic = 0
    @AppStorage("voice.cleanup") private var voiceCleanup = VoiceCleanup.always.rawValue

    var body: some View {
        @Bindable var store = store
        NavigationStack(path: $store.settingsPath) {
            Form {
                Section {
                    HStack {
                        Text("Status")
                        Spacer()
                        ConnectionChip(showLabel: true)
                    }
                    NavigationLink(value: SettingsRoute.server) {
                        LabeledContent("Server") {
                            Text(hostLabel).font(.subheadline.monospaced()).lineLimit(1)
                        }
                    }
                    LabeledContent("Token") {
                        Text(savedToken).font(.subheadline.monospaced())
                    }
                    NavigationLink(value: SettingsRoute.machines) {
                        LabeledContent("Other machines") {
                            Text(store.machines.isEmpty ? "None" : "\(store.machines.count)").font(.subheadline)
                        }
                    }
                    Button { Task { await checkHealth() } } label: {
                        HStack {
                            Text("Test connection")
                            Spacer()
                            if checking { ProgressView() }
                        }
                    }
                    .disabled(checking)
                    if let (m, ok) = healthMsg {
                        Label(m, systemImage: ok ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                            .font(.footnote).foregroundStyle(ok ? Tok.ok : Tok.err)
                    }
                    if let err = store.lastError {
                        Label(err, systemImage: "exclamationmark.triangle.fill").font(.footnote).foregroundStyle(Tok.err)
                    }
                } header: {
                    header("Connection")
                }
                .listRowBackground(Tok.panel)

                Section {
                    TextField("Pair link or amd_ token", text: $pairLink, axis: .vertical)
                        .font(.subheadline.monospaced())
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .lineLimit(1...3)
                    Button {
                        if let s = UIPasteboard.general.string { pairLink = s }
                    } label: { Label("Paste from clipboard", systemImage: "doc.on.clipboard") }
                    Button { Task { await pair() } } label: {
                        HStack {
                            Text(pairing ? "Pairing" : "Pair this phone").fontWeight(.semibold)
                            Spacer()
                            if pairing { ProgressView() }
                        }
                    }
                    .disabled(pairing || pairLink.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    if let (m, ok) = pairMsg {
                        Label(m, systemImage: ok ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                            .font(.footnote).foregroundStyle(ok ? Tok.ok : Tok.err)
                    }
                } header: {
                    header("Pair")
                } footer: {
                    Text("On the Mac, open Settings, Devices, add this phone and copy its pair link. The link works once, for 5 minutes.")
                }
                .listRowBackground(Tok.panel)

                Section {
                    UsageStrip()
                } header: {
                    header("Usage")
                } footer: {
                    Text("Plan windows come from each agent. Tokens per model are counted from this Mac's transcripts.")
                }
                .listRowBackground(Tok.panel)

                Section {
                    NavigationLink(value: SettingsRoute.alerts) {
                        row("Alerts", "bell.badge", Tok.err, badge: 0, total: nil)
                    }
                    NavigationLink(value: SettingsRoute.commands) {
                        row("History", "clock.arrow.circlepath", Tok.need, badge: 0, total: nil)
                    }
                    NavigationLink(value: SettingsRoute.storage) {
                        row("Storage", "externaldrive", Tok.codex, badge: 0, total: nil)
                    }
                    Picker(selection: $voiceCleanup) {
                        ForEach(VoiceCleanup.allCases) { Text($0.label).tag($0.rawValue) }
                    } label: {
                        Label { Text("Clean up dictation") } icon: {
                            Image(systemName: "wand.and.stars").font(.footnote.weight(.semibold)).foregroundStyle(.white)
                                .frame(width: 28, height: 28).background(Tok.accent, in: RoundedRectangle(cornerRadius: 7))
                        }
                    }
                } header: {
                    header("Phone")
                } footer: {
                    Text("Clean up sends dictated text to the Mac, which removes filler words and fixes punctuation. You see the result before it goes anywhere.")
                }
                .listRowBackground(Tok.panel)

                Section {
                    NavigationLink(value: SettingsRoute.agents) {
                        row("Agents", "cpu", Tok.run, badge: store.attentionCount, total: store.sessionsByID.count)
                    }
                    NavigationLink(value: SettingsRoute.talks) {
                        row("Talks", "bubble.left.and.bubble.right", Tok.sub, badge: store.pendingTalks.count, total: store.talksByID.count)
                    }
                    NavigationLink(value: SettingsRoute.panes) {
                        row("Panes", "rectangle.split.3x1", Tok.codex, badge: 0, total: store.panesByID.count)
                    }
                } header: {
                    header("Advanced")
                } footer: {
                    Text("Raw agent sessions, agent to agent talks and the tmux panes the daemon can drive.")
                }
                .listRowBackground(Tok.panel)

                Section {
                    LabeledContent("Version", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "-")
                    LabeledContent("Build", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "-")
                } header: {
                    header("About")
                }
                .listRowBackground(Tok.panel)
            }
            .scrollContentBackground(.hidden)
            .background(Tok.bg)
            .navigationTitle("Settings")
            .navigationDestination(for: SettingsRoute.self) { r in
                switch r {
                case .server: ServerSettingsView(savedToken: $savedToken)
                case .agents: SessionsView()
                case .talks: TalksView()
                case .panes: PanesView()
                case .alerts: AlertSettingsView()
                case .storage: StorageSettingsView()
                case .commands: HistoryView()
                case .machines: MachinesSettingsView()
                }
            }
            .sensoryFeedback(.success, trigger: pairedHaptic)
        }
    }

    private var hostLabel: String {
        URL(string: Config.serverURL)?.host.map { h in
            URL(string: Config.serverURL)?.port.map { "\(h):\($0)" } ?? h
        } ?? Config.serverURL
    }

    private func header(_ s: String) -> some View {
        Text(s).font(.subheadline.weight(.semibold)).foregroundStyle(Tok.muted).textCase(nil)
    }

    private func row(_ title: String, _ icon: String, _ color: Color, badge: Int, total: Int?) -> some View {
        HStack(spacing: 12) {
            Image(systemName: icon)
                .font(.footnote.weight(.semibold))
                .foregroundStyle(.white)
                .frame(width: 28, height: 28)
                .background(color, in: RoundedRectangle(cornerRadius: 7))
            Text(title)
            Spacer()
            if badge > 0 {
                Text("\(badge)").font(.footnote.weight(.semibold)).monospacedDigit()
                    .foregroundStyle(Tok.need)
                    .padding(.horizontal, 7).padding(.vertical, 2)
                    .background(Tok.needSoft, in: Capsule())
            }
            if let total { Text("\(total)").monospacedDigit().foregroundStyle(Tok.muted) }
        }
    }

    private func pair() async {
        pairing = true
        defer { pairing = false }
        do {
            let base: String
            let token: String
            switch try PairLink.parse(pairLink) {
            case .token(let t, let b):
                token = t
                base = b ?? Config.serverURL.trimmingCharacters(in: .whitespaces)
            case .code(let code, let b):
                guard let u = URL(string: b) else { throw PairLink.Failure(message: "Bad server address in the link.") }
                token = try await APIClient.redeemPairCode(code, base: u)
                base = b
            }
            Config.serverURL = base
            Config.password = token
            savedToken = PairLink.mask(token)
            pairLink = ""
            pairMsg = ("Paired with \(base). Token \(savedToken) saved.", true)
            pairedHaptic += 1
            store.connect()
        } catch {
            pairMsg = (error.localizedDescription, false)
        }
    }

    private func checkHealth() async {
        checking = true; defer { checking = false }
        do {
            _ = try await APIClient.shared.health()
            healthMsg = ("The daemon answers.", true)
        } catch {
            healthMsg = ("Failed: \(error.localizedDescription)", false)
        }
    }
}

/// Manual server address and token, for setups without a pair link.
struct ServerSettingsView: View {
    @Environment(AppStore.self) private var store
    @Binding var savedToken: String
    @State private var serverURL = Config.serverURL
    @State private var tokenDraft = ""
    @State private var confirmForget = false
    @State private var pendingHost: String?

    var body: some View {
        Form {
            Section {
                TextField("http://host:7777", text: $serverURL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.URL)
                    .font(.body.monospaced())
            } header: {
                Text("Server URL")
            } footer: {
                Text("The daemon address on the tailnet. The default is \(Config.defaultServer).")
            }
            .listRowBackground(Tok.panel)
            Section {
                LabeledContent("Saved", value: savedToken)
                SecureField("New device token or password", text: $tokenDraft)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
            } header: {
                Text("Token")
            }
            .listRowBackground(Tok.panel)
            Section {
                Button("Save and reconnect") { save(serverURL) }
                    .fontWeight(.semibold)
                Button("Reset to the Tailscale default") {
                    serverURL = Config.defaultServer
                    save(Config.defaultServer)
                }
                Button("Forget the token", role: .destructive) { confirmForget = true }
                    .disabled(Config.password.isEmpty)
            }
            .listRowBackground(Tok.panel)
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Server")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("Forget the saved token?", isPresented: $confirmForget, titleVisibility: .visible) {
            Button("Forget", role: .destructive) {
                Config.password = ""
                savedToken = PairLink.mask("")
                store.connect()
            }
        } message: {
            Text("The phone stops reaching a daemon that needs a token until you pair again.")
        }
        .confirmationDialog("New server address", isPresented: Binding(get: { pendingHost != nil }, set: { if !$0 { pendingHost = nil } }),
                            titleVisibility: .visible, presenting: pendingHost) { url in
            Button("Switch and forget the token", role: .destructive) { apply(url, keepToken: false) }
            Button("Keep the token") { apply(url, keepToken: true) }
            Button("Cancel", role: .cancel) { serverURL = Config.serverURL }
        } message: { url in
            Text("The saved token belongs to \(Config.hostKey(Config.serverURL)). Keep it only if \(Config.hostKey(url)) is the same Mac.")
        }
    }

    private func save(_ raw: String) {
        let url = raw.trimmingCharacters(in: .whitespaces)
        let newToken = !tokenDraft.trimmingCharacters(in: .whitespaces).isEmpty
        if !newToken, !Config.password.isEmpty, Config.hostKey(url) != Config.hostKey(Config.serverURL) {
            pendingHost = url
            return
        }
        apply(url, keepToken: true)
    }

    // A token goes only to the host it was saved for, unless the person keeps it on purpose.
    private func apply(_ url: String, keepToken: Bool) {
        Config.serverURL = url
        if !keepToken {
            Config.password = ""
            savedToken = PairLink.mask("")
        }
        let t = tokenDraft.trimmingCharacters(in: .whitespaces)
        if !t.isEmpty {
            Config.password = t
            tokenDraft = ""
            savedToken = PairLink.mask(t)
        }
        store.connect()
    }
}
