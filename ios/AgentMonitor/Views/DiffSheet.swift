import SwiftUI

struct DiffTarget: Identifiable, Hashable {
    let cwd: String
    let path: String
    var id: String { cwd + "\u{0}" + path }
}

/// The work tree changes of one file, or of every changed file, as colored unified diff lines.
struct DiffSheet: View {
    let target: DiffTarget
    @Environment(\.dismiss) private var dismiss
    @State private var files: [ODiffFile]?
    @State private var error: String?
    @State private var wrap = true

    private static let maxLines = 3000

    var body: some View {
        NavigationStack {
            Group {
                if let files {
                    if files.isEmpty {
                        ContentUnavailableView("No changes", systemImage: "checkmark.circle", description: Text("The work tree matches HEAD."))
                    } else {
                        ScrollView(wrap ? .vertical : [.vertical, .horizontal]) {
                            LazyVStack(alignment: .leading, spacing: 0) {
                                ForEach(files, id: \.path) { f in fileBlock(f) }
                            }
                            .padding(.vertical, 8)
                        }
                    }
                } else if let error {
                    ContentUnavailableView("No diff", systemImage: "exclamationmark.triangle", description: Text(error))
                } else {
                    ProgressView()
                }
            }
            .background(Tok.bg)
            .navigationTitle(target.path.isEmpty ? "Changes" : (target.path as NSString).lastPathComponent)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Done") { dismiss() } }
                ToolbarItem(placement: .primaryAction) {
                    Button(wrap ? "No wrap" : "Wrap") { wrap.toggle() }
                }
            }
            .task { await load() }
        }
    }

    @ViewBuilder private func fileBlock(_ f: ODiffFile) -> some View {
        HStack(spacing: 8) {
            Text(f.path).font(.footnote.monospaced().weight(.semibold)).lineLimit(2).truncationMode(.head)
            Spacer(minLength: 6)
            if let a = f.add, a > 0 { Text("+\(a)").foregroundStyle(Tok.ok) }
            if let d = f.del, d > 0 { Text("\u{2212}\(d)").foregroundStyle(Tok.err) }
        }
        .font(.caption.monospacedDigit())
        .padding(.horizontal, 12).padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Tok.panel2)
        .accessibilityElement(children: .combine)
        if f.binary ?? false {
            note("Binary file: no text diff.")
        } else {
            let lines = bodyLines(f.patch ?? "")
            ForEach(Array(lines.prefix(Self.maxLines).enumerated()), id: \.offset) { _, l in line(String(l)) }
            if lines.count > Self.maxLines || (f.truncated ?? false) { note("The diff is longer than this view shows. Open it on the Mac for the rest.") }
        }
    }

    /// File headers show only before a file's first hunk, so a removed "-- comment" line stays.
    private func bodyLines(_ patch: String) -> [Substring] {
        var inHunk = false
        return patch.split(separator: "\n", omittingEmptySubsequences: false).filter { l in
            if l.hasPrefix("diff --git") { inHunk = false; return false }
            if l.hasPrefix("@@") { inHunk = true }
            return inHunk || !isHeader(l)
        }
    }

    private func isHeader(_ l: Substring) -> Bool {
        l.hasPrefix("diff --git") || l.hasPrefix("index ") || l.hasPrefix("--- ") || l.hasPrefix("+++ ") || l.hasPrefix("new file mode") || l.hasPrefix("deleted file mode")
    }

    private func line(_ l: String) -> some View {
        let add = l.hasPrefix("+"), del = l.hasPrefix("-"), hunk = l.hasPrefix("@@")
        return Text(l.isEmpty ? " " : l)
            .font(.caption.monospaced())
            .foregroundStyle(hunk ? Tok.muted : Tok.fg)
            .lineLimit(wrap ? nil : 1)
            .fixedSize(horizontal: !wrap, vertical: false)
            .padding(.horizontal, 12).padding(.vertical, 1)
            .frame(maxWidth: wrap ? .infinity : nil, alignment: .leading)
            .background(add ? Tok.okSoft : del ? Tok.err.opacity(0.12) : Color.clear)
            .accessibilityLabel(add ? "Added: \(l.dropFirst())" : del ? "Removed: \(l.dropFirst())" : l)
    }

    private func note(_ s: String) -> some View {
        Text(s).font(.footnote).foregroundStyle(Tok.muted).padding(.horizontal, 12).padding(.vertical, 6)
    }

    private func load() async {
        do {
            files = try await APIClient.shared.cwdDiff(target.cwd, path: target.path).files ?? []
        } catch {
            self.error = error.localizedDescription
        }
    }
}
