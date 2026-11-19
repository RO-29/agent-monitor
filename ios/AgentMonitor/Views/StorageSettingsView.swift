import SwiftUI

/// Settings, Storage: how long the daemon keeps attached files, with a clean-up now and a delete-all.
struct StorageSettingsView: View {
    struct Stats: Decodable {
        var files: Int
        var bytes: Int64
        var deleted: Int?
        var oldest: Int64?
        var maxAgeHours: Int
        var quotaBytes: Int64
    }

    static let ages: [(h: Int, l: String)] = [(1, "1 hour"), (24, "1 day"), (168, "7 days"), (0, "30 days"), (2160, "90 days"), (-1, "Keep")]

    @State private var stats: Stats?
    @State private var error: String?
    @State private var note: String?
    @State private var busy = false
    @State private var confirmAll = false

    var body: some View {
        Form {
            Section {
                Picker("Delete after", selection: Binding(get: { stats?.maxAgeHours ?? 0 }, set: { h in Task { await setAge(h) } })) {
                    ForEach(Self.ages, id: \.h) { Text($0.l).tag($0.h) }
                }
                .disabled(busy || stats == nil)
            } header: {
                Text("Attached files")
            } footer: {
                Text(stats?.maxAgeHours == -1
                     ? "Files stay until the \(size(stats?.quotaBytes ?? 500 << 20)) limit is full."
                     : "The Mac checks every hour. This setting is the same on the web, the Mac app and this phone.")
            }
            .listRowBackground(Tok.panel)

            Section {
                if let s = stats {
                    LabeledContent("Files", value: "\(s.files)")
                    LabeledContent("Space", value: "\(size(s.bytes)) of \(size(s.quotaBytes))")
                    if let o = s.oldest {
                        LabeledContent("Oldest", value: Date(timeIntervalSince1970: Double(o) / 1000).formatted(date: .abbreviated, time: .shortened))
                    }
                } else if error == nil {
                    ProgressView()
                }
                Button("Clean Up Now") { Task { await prune(all: false) } }
                    .disabled(busy || stats == nil || stats?.maxAgeHours == -1)
                Button("Delete All Files", role: .destructive) { confirmAll = true }
                    .disabled(busy || (stats?.files ?? 0) == 0)
            } footer: {
                if let error { Text(error).foregroundStyle(Tok.err) } else if let note { Text(note) }
            }
            .listRowBackground(Tok.panel)
        }
        .scrollContentBackground(.hidden)
        .background(Tok.bg)
        .navigationTitle("Storage")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("Delete all attached files?", isPresented: $confirmAll, titleVisibility: .visible) {
            Button("Delete All", role: .destructive) { Task { await prune(all: true) } }
        } message: {
            Text("Agents can no longer open files you attached before.")
        }
        .task { await load() }
        .refreshable { await load() }
    }

    private func size(_ n: Int64) -> String { ByteCountFormatter.string(fromByteCount: n, countStyle: .file) }

    private func load() async {
        do {
            stats = try await APIClient.shared.get("/api/orch/uploads/stats", as: Stats.self)
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func setAge(_ h: Int) async {
        busy = true
        defer { busy = false }
        do {
            _ = try await APIClient.shared.post("/api/settings", body: ["uploadMaxAgeHours": h])
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func prune(all: Bool) async {
        busy = true
        defer { busy = false }
        do {
            let data = try await APIClient.shared.post("/api/orch/uploads/prune", body: all ? ["all": true] : [:])
            let s = try JSONDecoder().decode(Stats.self, from: data)
            stats = s
            let n = s.deleted ?? 0
            note = n == 0 ? "No files to delete." : "Deleted \(n) file\(n == 1 ? "" : "s")."
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}
