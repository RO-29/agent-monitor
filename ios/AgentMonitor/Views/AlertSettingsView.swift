import SwiftUI
import UserNotifications

/// Settings, Alerts: a master switch, then per kind on/off, sound and a test; optional quiet hours.
struct AlertSettingsView: View {
    @Environment(AppStore.self) private var store
    @AppStorage("notify.all") private var allOn = true
    @AppStorage("notify.quiet") private var quiet = false
    @AppStorage("notify.quietFrom") private var quietFrom = 22
    @AppStorage("notify.quietTo") private var quietTo = 7
    @State private var osStatus: UNAuthorizationStatus?
    @Environment(\.scenePhase) private var phase

    var body: some View {
        Form {
            if let st = osStatus, st == .denied {
                Section {
                    Label("Notifications are off in iOS Settings", systemImage: "bell.slash").foregroundStyle(Tok.err)
                    Button("Open Settings") {
                        if let u = URL(string: UIApplication.openNotificationSettingsURLString) { UIApplication.shared.open(u) }
                    }
                } footer: {
                    Text("The switches below do nothing until iOS allows alerts for Agent Monitor.")
                }
                .listRowBackground(Tok.panel)
            }
            Section {
                Toggle("Alerts", isOn: $allOn)
                NavigationLink {
                    PushSettingsView()
                } label: {
                    Label("Lock screen alerts", systemImage: "lock.iphone")
                }
            } footer: {
                Text("The app alerts while it keeps its connection to the daemon, in the foreground or shortly after. Lock screen alerts come through the Bark app.")
            }
            .listRowBackground(Tok.panel)

            if allOn {
                ForEach(NotifyKind.allCases) { k in
                    KindSection(kind: k, threadID: store.workspacePath.first ?? store.threads.first?.id)
                }
                Section {
                    Toggle("Quiet hours", isOn: $quiet)
                    if quiet {
                        Picker("From", selection: $quietFrom) { ForEach(0..<24, id: \.self) { Text(hour($0)).tag($0) } }
                        Picker("To", selection: $quietTo) { ForEach(0..<24, id: \.self) { Text(hour($0)).tag($0) } }
                    }
                } footer: {
                    Text("During quiet hours alerts still arrive, with no sound.")
                }
                .listRowBackground(Tok.panel)
            }
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Alerts")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: phase) { osStatus = await Notifier.shared.refreshAuthorization() }
    }

    private func hour(_ h: Int) -> String { String(format: "%02d:00", h) }
}

private struct KindSection: View {
    let kind: NotifyKind
    let threadID: String?
    @AppStorage private var on: Bool
    @AppStorage private var sound: String

    init(kind: NotifyKind, threadID: String?) {
        self.kind = kind
        self.threadID = threadID
        _on = AppStorage(wrappedValue: kind.defaultOn, NotifyPrefs.enabledKey(kind))
        _sound = AppStorage(wrappedValue: kind.defaultSound.rawValue, NotifyPrefs.soundKey(kind))
    }

    var body: some View {
        Section {
            Toggle(isOn: $on) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(kind.label)
                    Text(kind.detail).font(.footnote).foregroundStyle(Tok.muted)
                }
            }
            if on {
                Picker("Sound", selection: $sound) {
                    ForEach(NotifySound.allCases) { s in Text(s.label).tag(s.rawValue) }
                }
                Button {
                    Task {
                        if await Notifier.shared.test(kind, threadId: threadID) {
                            ToastCenter.shared.show("Test alert sent", icon: "bell.badge")
                        } else {
                            ToastCenter.shared.show("iOS blocks alerts for this app", icon: "exclamationmark.triangle.fill")
                        }
                    }
                } label: { Label("Send a test", systemImage: "bell.badge") }
            }
        }
        .listRowBackground(Tok.panel)
    }
}
