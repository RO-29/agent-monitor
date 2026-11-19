import SwiftUI
import UIKit

/// Settings, Alerts, Lock screen: the daemon pushes encrypted alerts through the Bark app when this app is closed.
struct PushSettingsView: View {
    struct Bark: Decodable {
        var enabled: Bool
        var server: String
        var deviceKeySet: Bool
        var deviceKeyHint: String?
        var key: String
        var iv: String
        var algorithm: String
        var mode: String
        var padding: String
        var sound: String?
    }

    @State private var cfg: Bark?
    @State private var keyField = ""
    @State private var error: String?
    @State private var note: String?
    @State private var busy = false
    @State private var copied: String?

    var body: some View {
        Form {
            Section {
                Text("This app alerts only while it runs. For alerts on a locked phone, the Mac sends each alert through the free Bark app. The Mac encrypts the alert, so Bark's server sees no text. A tap on the alert opens the thread here.")
                    .font(.footnote).foregroundStyle(Tok.fg2)
                Link(destination: URL(string: "https://apps.apple.com/app/id1403753865")!) {
                    Label("1. Get Bark from the App Store", systemImage: "arrow.down.app")
                }
            }
            .listRowBackground(Tok.panel)

            Section {
                TextField(cfg?.deviceKeySet == true ? "Set (\(cfg?.deviceKeyHint ?? "")). Paste to change." : "https://api.day.app/your-key/", text: $keyField)
                    .textInputAutocapitalization(.never).autocorrectionDisabled().font(.body.monospaced())
                Button("Save Bark URL") { Task { await save(["deviceKey": keyField]) } }
                    .disabled(busy || keyField.trimmingCharacters(in: .whitespaces).isEmpty)
            } header: {
                Text("2. Bark URL")
            } footer: {
                Text("Open Bark and copy the server URL on its home screen.")
            }
            .listRowBackground(Tok.panel)

            if let c = cfg {
                Section {
                    LabeledContent("Algorithm", value: c.algorithm)
                    LabeledContent("Mode", value: c.mode)
                    LabeledContent("Padding", value: c.padding)
                    copyRow("Key", c.key)
                    copyRow("IV", c.iv)
                } header: {
                    Text("3. Bark encryption settings")
                } footer: {
                    Text("In Bark, open Push Encryption and enter these values. Keep the key private.")
                }
                .listRowBackground(Tok.panel)

                Section {
                    Toggle("Lock screen alerts", isOn: Binding(get: { c.enabled }, set: { v in Task { await save(["enabled": v]) } }))
                        .disabled(busy || !c.deviceKeySet)
                    Button("Send Test Alert") { Task { await test() } }
                        .disabled(busy || !c.deviceKeySet)
                    Button("Make a New Key", role: .destructive) { Task { await save(["newKey": true]) } }
                        .disabled(busy)
                } header: {
                    Text("4. Turn on")
                } footer: {
                    if let error { Text(error).foregroundStyle(Tok.err) } else if let note { Text(note) } else {
                        Text("Lock the phone, then send a test. Alerts are not pushed while this app is open, because it alerts by itself.")
                    }
                }
                .listRowBackground(Tok.panel)
            } else if let error {
                Section { Text(error).foregroundStyle(Tok.err) }.listRowBackground(Tok.panel)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Lock Screen")
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
    }

    private func copyRow(_ label: String, _ value: String) -> some View {
        Button {
            UIPasteboard.general.string = value
            copied = label
        } label: {
            LabeledContent {
                HStack(spacing: 6) {
                    Text(value).font(.footnote.monospaced()).lineLimit(1).truncationMode(.middle)
                    Image(systemName: copied == label ? "checkmark" : "doc.on.doc").foregroundStyle(Tok.accent)
                }
            } label: { Text(label).foregroundStyle(Tok.fg) }
        }
        .accessibilityLabel("Copy \(label)")
    }

    private func load() async {
        do {
            cfg = try await APIClient.shared.get("/api/push/bark", as: Bark.self)
            error = nil
        } catch let e as APIError where e.status == 404 {
            error = "Update agent-monitor on the Mac to use lock screen alerts."
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func save(_ body: [String: Any]) async {
        busy = true
        defer { busy = false }
        do {
            let data = try await APIClient.shared.post("/api/push/bark", body: body)
            cfg = try JSONDecoder().decode(Bark.self, from: data)
            if body["deviceKey"] != nil { keyField = ""; note = "Bark URL saved." }
            if body["newKey"] != nil { note = "New key made. Enter it in Bark again."; copied = nil }
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func test() async {
        busy = true
        defer { busy = false }
        do {
            _ = try await APIClient.shared.post("/api/push/bark/test", body: [:])
            note = "Test sent. It shows on the lock screen, or in Bark when the phone is unlocked."
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}
