import SwiftUI
import UIKit

// MARK: - Markdown to Slack mrkdwn (port of web/src/lib/slack.ts)

enum SlackFormat {
    private static func re(_ p: String) -> NSRegularExpression {
        // Patterns are literals; a bad one is a programming error.
        try! NSRegularExpression(pattern: p, options: [])
    }

    /// Replaces each match with f(groups); group 0 is the whole match, a missing group is "".
    static func sub(_ s: String, _ p: String, _ f: ([String]) -> String) -> String {
        let ns = s as NSString
        var out = ""
        var last = 0
        for m in re(p).matches(in: s, range: NSRange(location: 0, length: ns.length)) {
            out += ns.substring(with: NSRange(location: last, length: m.range.location - last))
            let g = (0..<m.numberOfRanges).map { i -> String in
                let r = m.range(at: i)
                return r.location == NSNotFound ? "" : ns.substring(with: r)
            }
            out += f(g)
            last = m.range.location + m.range.length
        }
        out += ns.substring(from: last)
        return out
    }

    static func first(_ s: String, _ p: String) -> [String]? {
        let ns = s as NSString
        guard let m = re(p).firstMatch(in: s, range: NSRange(location: 0, length: ns.length)) else { return nil }
        return (0..<m.numberOfRanges).map { i in
            let r = m.range(at: i)
            return r.location == NSNotFound ? "" : ns.substring(with: r)
        }
    }

    /// Inline spans: code stays, links become "text (url)", ** and __ become *, single * becomes _.
    static func inline(_ raw: String) -> String {
        var keep: [String] = []
        func hold(_ s: String) -> String { keep.append(s); return "\u{0}\(keep.count - 1)\u{0}" }
        var s = sub(raw, #"(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)"#) { hold($0[0]) }
        s = sub(s, #"!?\[([^\]]*)\]\(<?([^\s)>]+)>?(?:\s+"[^"]*")?\)"#) { g in
            hold(!g[1].isEmpty && g[1] != g[2] ? "\(g[1]) (\(g[2]))" : g[2])
        }
        s = sub(s, #"<(https?://[^\s>]+)>"#) { hold($0[1]) }
        s = sub(s, #"\*\*(?=\S)([\s\S]*?\S)\*\*"#) { "\u{1}\($0[1])\u{1}" }
        s = sub(s, #"(^|[^\w])__(?=\S)([\s\S]*?\S)__(?!\w)"#) { "\($0[1])\u{1}\($0[2])\u{1}" }
        s = sub(s, #"(^|[\s(])\*(?=[^\s*])([^*\n]*?[^\s*])\*(?![\w*])"#) { "\($0[1])_\($0[2])_" }
        s = sub(s, #"~~(?=\S)([\s\S]*?\S)~~"#) { "~\($0[1])~" }
        s = s.replacingOccurrences(of: "\u{1}", with: "*")
        return sub(s, "\u{0}(\\d+)\u{0}") { g in Int(g[1]).flatMap { $0 < keep.count ? keep[$0] : nil } ?? g[0] }
    }

    private static func strip(_ s: String) -> String {
        s.replacingOccurrences(of: "**", with: "").replacingOccurrences(of: "__", with: "").replacingOccurrences(of: "`", with: "")
    }

    private static func cells(_ l: String) -> [String] {
        var t = l.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("|") { t.removeFirst() }
        if t.hasSuffix("|") { t.removeLast() }
        return t.replacingOccurrences(of: "\\|", with: "\u{2}").components(separatedBy: "|")
            .map { $0.trimmingCharacters(in: .whitespaces).replacingOccurrences(of: "\u{2}", with: "|") }
    }

    private static func table(_ rows: [[String]]) -> String {
        let n = rows.map(\.count).max() ?? 0
        let w = (0..<n).map { i in rows.map { i < $0.count ? $0[i].count : 0 }.max() ?? 0 }
        func line(_ r: [String]) -> String {
            w.enumerated().map { i, x in (i < r.count ? r[i] : "").padding(toLength: x, withPad: " ", startingAt: 0) }
                .joined(separator: "  ").replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression)
        }
        let out = [line(rows[0]), w.map { String(repeating: "-", count: $0) }.joined(separator: "  ")] + rows.dropFirst().map(line)
        return "```\n" + out.joined(separator: "\n") + "\n```"
    }

    static let tableSep = #"^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$"#

    /// Converts one Markdown document to Slack mrkdwn.
    static func toSlack(_ md: String) -> String {
        let src = md.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n")
        var out: [String] = []
        var i = 0
        while i < src.count {
            let l = src[i]
            if let f = first(l, #"^\s*(`{3,}|~{3,})"#) {
                var body: [String] = []
                var j = i + 1
                while j < src.count && !src[j].trimmingCharacters(in: .whitespaces).hasPrefix(f[1]) { body.append(src[j]); j += 1 }
                out.append("```\n" + body.joined(separator: "\n") + "\n```")
                i = j + 1
                continue
            }
            if l.contains("|"), i + 1 < src.count, first(src[i + 1], tableSep) != nil, src[i + 1].contains("-") {
                var rows = [cells(l).map(strip)]
                var j = i + 2
                while j < src.count && src[j].contains("|") && !src[j].trimmingCharacters(in: .whitespaces).isEmpty {
                    rows.append(cells(src[j]).map(strip)); j += 1
                }
                out.append(table(rows))
                i = j
                continue
            }
            if let h = first(l, #"^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$"#) {
                out.append("*\(inline(strip(h[1])))*")
            } else if first(l, #"^\s{0,3}([-*_])(\s*\1){2,}\s*$"#) != nil {
                out.append("──────────")
            } else if let li = first(l, #"^(\s*)([-*+])\s+(?:\[([ xX])\]\s+)?(.*)$"#) {
                let pad = String(repeating: " ", count: (li[1].replacingOccurrences(of: "\t", with: "  ").count / 2) * 4)
                let box = li[3].isEmpty ? "" : (li[3] == " " ? "☐ " : "☑ ")
                out.append("\(pad)• \(box)\(inline(li[4]))")
            } else if let ol = first(l, #"^(\s*)(\d+)[.)]\s+(.*)$"#) {
                let pad = String(repeating: " ", count: (ol[1].count / 2) * 4)
                out.append("\(pad)\(ol[2]). \(inline(ol[3]))")
            } else if let q = first(l, #"^\s*>\s?(.*)$"#) {
                out.append("> \(inline(q[1]))".replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression))
            } else {
                out.append(inline(l))
            }
            i += 1
        }
        return out.joined(separator: "\n")
            .replacingOccurrences(of: "\n{3,}", with: "\n\n", options: .regularExpression)
            .trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Markdown without its markup, for a plain Copy: links as "text (url)", no emphasis, no heading marks.
    static func plain(_ md: String) -> String {
        var s = sub(md, #"!?\[([^\]]*)\]\(<?([^\s)>]+)>?\)"#) { g in !g[1].isEmpty && g[1] != g[2] ? "\(g[1]) (\(g[2]))" : g[2] }
        s = sub(s, #"\*\*(?=\S)([\s\S]*?\S)\*\*"#) { $0[1] }
        s = sub(s, #"(?m)^\s{0,3}#{1,6}\s+"#) { _ in "" }
        s = sub(s, #"(?m)^\s*(`{3,}|~{3,}).*$\n?"#) { _ in "" }
        s = sub(s, #"(?<!`)`([^`\n]+)`(?!`)"#) { $0[1] }
        return s.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

// MARK: - Copy and share

enum Share {
    /// Presents the system share sheet over whatever is on screen.
    @MainActor static func present(_ items: [Any]) {
        guard let scene = UIApplication.shared.connectedScenes.compactMap({ $0 as? UIWindowScene }).first(where: { $0.activationState == .foregroundActive })
                ?? UIApplication.shared.connectedScenes.compactMap({ $0 as? UIWindowScene }).first,
              var top = scene.windows.first(where: \.isKeyWindow)?.rootViewController else { return }
        while let p = top.presentedViewController { top = p }
        let vc = UIActivityViewController(activityItems: items, applicationActivities: nil)
        vc.popoverPresentationController?.sourceView = top.view
        vc.popoverPresentationController?.sourceRect = CGRect(x: top.view.bounds.midX, y: top.view.bounds.maxY - 40, width: 1, height: 1)
        top.present(vc, animated: true)
    }

    /// Writes markdown to a temporary .md file and shares the file, so Files and Mail get a real document.
    @MainActor static func markdownFile(_ md: String, name: String) {
        let safe = name.replacingOccurrences(of: "[^A-Za-z0-9 _-]", with: "", options: .regularExpression)
            .trimmingCharacters(in: .whitespaces).prefix(60)
        let url = FileManager.default.temporaryDirectory.appendingPathComponent((safe.isEmpty ? "thread" : String(safe)) + ".md")
        do {
            try md.write(to: url, atomically: true, encoding: .utf8)
            present([url])
        } catch {
            ToastCenter.shared.show("Could not write the file", icon: "exclamationmark.circle.fill")
        }
    }
}

/// Copy, Copy as Markdown, Copy for Slack and Share for one piece of markdown text.
struct CopyMenuItems: View {
    let markdown: String
    var label = "Copy"
    var isCode = false

    var body: some View {
        Button { copyToPasteboard(isCode ? markdown : SlackFormat.plain(markdown)) } label: { Label(label, systemImage: "doc.on.doc") }
        if !isCode {
            Button { copyToPasteboard(markdown, label: "Markdown copied") } label: { Label("Copy as Markdown", systemImage: "number") }
        }
        Button {
            copyToPasteboard(isCode ? "```\n\(markdown)\n```" : SlackFormat.toSlack(markdown), label: "Copied for Slack")
        } label: { Label("Copy for Slack", systemImage: "number.square") }
        Button { Share.present([markdown]) } label: { Label("Share…", systemImage: "square.and.arrow.up") }
    }
}

/// A small visible copy control; long-press shows the other formats.
struct CopyButton: View {
    let markdown: String
    var isCode = false
    var body: some View {
        Menu {
            CopyMenuItems(markdown: markdown, isCode: isCode)
        } label: {
            Image(systemName: "doc.on.doc")
                .font(.footnote.weight(.medium))
                .foregroundStyle(Tok.muted)
                .frame(width: 32, height: 32)
                .contentShape(Rectangle())
        } primaryAction: {
            copyToPasteboard(isCode ? markdown : SlackFormat.plain(markdown))
        }
        .accessibilityLabel("Copy")
        .accessibilityHint("Long-press for Markdown, Slack and Share")
    }
}
