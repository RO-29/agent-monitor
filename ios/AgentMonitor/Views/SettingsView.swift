import SwiftUI

struct SettingsView: View {
    @Environment(AppStore.self) private var store
    @State private var serverURL = Config.serverURL
    @State private var healthMsg: String?
    @State private var checking = false

    var body: some View {
        NavigationStack {
            Form {
                Section("Daemon") {
                    TextField("Server URL", text: $serverURL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                        .font(.callout.monospaced())
                    Button("Save & Reconnect") {
                        Config.serverURL = serverURL.trimmingCharacters(in: .whitespaces)
                        store.connect()
                    }
                    Button("Reset to Tailscale default") {
                        serverURL = Config.defaultServer
                        Config.serverURL = Config.defaultServer
                        store.connect()
                    }
                    .foregroundStyle(.secondary)
                }

                Section("Status") {
                    HStack { Text("Connection"); Spacer(); ConnectionChip() }
                    Button {
                        Task { await checkHealth() }
                    } label: {
                        HStack {
                            Text("Test connection")
                            Spacer()
                            if checking { ProgressView() }
                        }
                    }
                    if let healthMsg {
                        Text(healthMsg).font(.caption)
                            .foregroundStyle(healthMsg.hasPrefix("OK") ? .green : .red)
                    }
                    if let err = store.lastError {
                        Text(err).font(.caption).foregroundStyle(.red)
                    }
                }

                Section("Live counts") {
                    LabeledContent("Agents", value: "\(store.sessionsByID.count)")
                    LabeledContent("Needs attention", value: "\(store.attentionCount)")
                    LabeledContent("Pending approvals", value: "\(store.permissionsByID.count)")
                    LabeledContent("Pending talks", value: "\(store.pendingTalks.count)")
                    LabeledContent("Registered panes", value: "\(store.panesByID.count)")
                }

                Section {
                    Text("Agent Monitor mirrors the web dashboard over your tailnet. Bind the daemon to your Tailscale IP so only your devices can reach it.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .navigationTitle("Settings")
        }
    }

    private func checkHealth() async {
        checking = true; defer { checking = false }
        do {
            _ = try await APIClient.shared.health()
            healthMsg = "OK · daemon reachable"
        } catch {
            healthMsg = "Failed · \(error.localizedDescription)"
        }
    }
}
