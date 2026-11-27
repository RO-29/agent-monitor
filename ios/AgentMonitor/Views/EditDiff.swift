import SwiftUI

/// One line of an edit preview: " " context, "-" removed, "+" added, "@" a file or hunk header.
struct DiffLine: Hashable {
    let mark: Character
    let text: String
}

/// The diff an edit approval would apply, built from the tool input (Edit, MultiEdit, Write, apply_patch).
enum EditDiff {
    static func lines(tool: String, input: [String: JSONValue]?) -> [DiffLine]? {
        guard let input else { return nil }
        func s(_ k: String) -> String? { if case .string(let v)? = input[k] { return v }; return nil }
        let file = s("file_path") ?? s("path")
        var out: [DiffLine] = []
        if let f = file { out.append(DiffLine(mark: "@", text: (f as NSString).lastPathComponent)) }
        switch tool {
        case "Edit":
            guard let old = s("old_string"), let new = s("new_string") else { return nil }
            out += hunk(old, new)
        case "MultiEdit":
            guard case .array(let edits)? = input["edits"] else { return nil }
            for case .object(let e) in edits {
                if case .string(let old)? = e["old_string"], case .string(let new)? = e["new_string"] { out += hunk(old, new) }
            }
        case "Write":
            guard let c = s("content") else { return nil }
            out += c.split(separator: "\n", omittingEmptySubsequences: false).map { DiffLine(mark: "+", text: String($0)) }
        default:
            guard let p = s("patch") ?? s("input") ?? s("command"), p.contains("*** Begin Patch") else { return nil }
            for l in p.split(separator: "\n", omittingEmptySubsequences: false) {
                if l.hasPrefix("*** ") && l.contains("File:") { out.append(DiffLine(mark: "@", text: String(l.dropFirst(4)))) }
                else if let c = l.first, c == "+" || c == "-" || c == " " { out.append(DiffLine(mark: c, text: String(l.dropFirst()))) }
            }
        }
        return out.count > (file == nil ? 0 : 1) ? out : nil
    }

    /// Removed and added lines with up to 2 shared lines on each side.
    static func hunk(_ old: String, _ new: String) -> [DiffLine] {
        let a = old.components(separatedBy: "\n"), b = new.components(separatedBy: "\n")
        var p = 0
        while p < a.count, p < b.count, a[p] == b[p] { p += 1 }
        var q = 0
        while q < a.count - p, q < b.count - p, a[a.count - 1 - q] == b[b.count - 1 - q] { q += 1 }
        var out = a[max(0, p - 2)..<p].map { DiffLine(mark: " ", text: $0) }
        out += a[p..<(a.count - q)].map { DiffLine(mark: "-", text: $0) }
        out += b[p..<(b.count - q)].map { DiffLine(mark: "+", text: $0) }
        out += a[(a.count - q)..<min(a.count, a.count - q + 2)].map { DiffLine(mark: " ", text: $0) }
        return out
    }
}

struct EditDiffView: View {
    let lines: [DiffLine]
    var limit = 40
    @State private var all = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array((all ? lines : Array(lines.prefix(limit))).enumerated()), id: \.offset) { _, l in
                row(l)
            }
            if lines.count > limit && !all {
                Button("Show all \(lines.count) lines") { all = true }
                    .font(Typo.ui(12, .medium)).padding(.top, 6)
            }
        }
        .padding(8)
        .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 8))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Edit preview, \(lines.filter { $0.mark == "-" }.count) lines removed, \(lines.filter { $0.mark == "+" }.count) lines added")
    }

    @ViewBuilder private func row(_ l: DiffLine) -> some View {
        switch l.mark {
        case "@":
            Text(l.text).font(Typo.mono(11.5, .semibold)).foregroundStyle(Tok.fg2).padding(.vertical, 3)
        default:
            Text("\(String(l.mark)) \(l.text)")
                .font(Typo.mono(11.5))
                .foregroundStyle(l.mark == "-" ? Tok.err : l.mark == "+" ? Tok.ok : Tok.muted)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(l.mark == "-" ? Tok.errSoft : l.mark == "+" ? Tok.okSoft : .clear)
                .lineLimit(3)
        }
    }
}
