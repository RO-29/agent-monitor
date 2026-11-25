import SwiftUI

// MARK: - Agent glyph (AgentIcon.tsx paths, 16x16 viewBox)

struct AgentGlyph: View {
    let agent: String
    var size: CGFloat = 13
    @ScaledMetric(relativeTo: .body) private var scale: CGFloat = 1

    var body: some View {
        let k = agentKey(agent)
        let c = Tok.agent(agent)
        Group {
            if ["claude", "codex", "cursor", "opencode", "gemini", "pi"].contains(k) {
                Canvas { ctx, sz in
                    let s = sz.width / 16
                    ctx.scaleBy(x: s, y: s)
                    for p in Self.parts(k) {
                        if p.fill > 0 { ctx.fill(p.path, with: .color(c.opacity(p.fill))) }
                        ctx.stroke(p.path, with: .color(c), style: StrokeStyle(lineWidth: p.width, lineCap: .round, lineJoin: .round))
                    }
                }
            } else {
                Text(String(k.first ?? "?").uppercased())
                    .font(.system(size: size * scale * 0.7, weight: .bold))
                    .foregroundStyle(c)
            }
        }
        .frame(width: size * scale, height: size * scale)
        .accessibilityLabel(agentName(agent))
    }

    private struct Part { var path: Path; var width: CGFloat; var fill: Double = 0 }

    private static func parts(_ k: String) -> [Part] {
        switch k {
        case "claude":
            return [Part(path: Path { p in
                p.move(to: .init(x: 8, y: 1.8)); p.addLine(to: .init(x: 8, y: 14.2))
                p.move(to: .init(x: 1.8, y: 8)); p.addLine(to: .init(x: 14.2, y: 8))
                p.move(to: .init(x: 3.6, y: 3.6)); p.addLine(to: .init(x: 12.4, y: 12.4))
                p.move(to: .init(x: 12.4, y: 3.6)); p.addLine(to: .init(x: 3.6, y: 12.4))
            }, width: 1.7)]
        case "codex":
            return [
                Part(path: Path { p in
                    p.addLines([.init(x: 8, y: 1.6), .init(x: 13.6, y: 4.8), .init(x: 13.6, y: 11.2),
                                .init(x: 8, y: 14.4), .init(x: 2.4, y: 11.2), .init(x: 2.4, y: 4.8)])
                    p.closeSubpath()
                }, width: 1.4),
                Part(path: Path { p in
                    p.addLines([.init(x: 5.8, y: 6.3), .init(x: 7.8, y: 8), .init(x: 5.8, y: 9.7)])
                    p.move(to: .init(x: 8.8, y: 10)); p.addLine(to: .init(x: 10.6, y: 10))
                }, width: 1.3),
            ]
        case "cursor":
            return [Part(path: Path { p in
                p.addLines([.init(x: 3, y: 2.2), .init(x: 13, y: 7.4), .init(x: 8.6, y: 8.6), .init(x: 7, y: 13.4)])
                p.closeSubpath()
            }, width: 1.4, fill: 0.25)]
        case "opencode":
            return [Part(path: Path { p in
                for m in [CGFloat(0), 16] {
                    let x = { (v: CGFloat) in m == 0 ? v : m - v }
                    p.move(to: .init(x: x(5.6), y: 2.6))
                    p.addCurve(to: .init(x: x(4), y: 5.2), control1: .init(x: x(4), y: 2.6), control2: .init(x: x(4), y: 4))
                    p.addCurve(to: .init(x: x(2.6), y: 8), control1: .init(x: x(4), y: 6.4), control2: .init(x: x(3.6), y: 7.6))
                    p.addCurve(to: .init(x: x(4), y: 10.8), control1: .init(x: x(3.6), y: 8.4), control2: .init(x: x(4), y: 9.4))
                    p.addCurve(to: .init(x: x(5.6), y: 13.4), control1: .init(x: x(4), y: 12.2), control2: .init(x: x(4), y: 13.4))
                }
            }, width: 1.4)]
        case "gemini":
            return [Part(path: Path { p in
                p.move(to: .init(x: 8, y: 1.6))
                p.addCurve(to: .init(x: 14.4, y: 8), control1: .init(x: 8.6, y: 5.6), control2: .init(x: 10.4, y: 7.4))
                p.addCurve(to: .init(x: 8, y: 14.4), control1: .init(x: 10.4, y: 8.6), control2: .init(x: 8.6, y: 10.4))
                p.addCurve(to: .init(x: 1.6, y: 8), control1: .init(x: 7.4, y: 10.4), control2: .init(x: 5.6, y: 8.6))
                p.addCurve(to: .init(x: 8, y: 1.6), control1: .init(x: 5.6, y: 7.4), control2: .init(x: 7.4, y: 5.6))
                p.closeSubpath()
            }, width: 1.2, fill: 0.3)]
        default:
            return [Part(path: Path { p in
                p.move(to: .init(x: 3, y: 4.4)); p.addLine(to: .init(x: 13, y: 4.4))
                p.move(to: .init(x: 6, y: 4.4)); p.addLine(to: .init(x: 6, y: 12.8))
                p.move(to: .init(x: 10.2, y: 4.4)); p.addLine(to: .init(x: 10.2, y: 11))
                p.addCurve(to: .init(x: 12, y: 12.8), control1: .init(x: 10.2, y: 12.2), control2: .init(x: 10.8, y: 12.8))
            }, width: 1.5)]
        }
    }
}

// MARK: - Atoms

enum Tone {
    case need, run, ok, err, sub, mute, acc

    var fg: Color {
        switch self {
        case .need: return Tok.need
        case .run: return Tok.run
        case .ok: return Tok.ok
        case .err: return Tok.err
        case .sub: return Tok.sub
        case .mute: return Tok.fg2
        case .acc: return Tok.accent
        }
    }

    var soft: Color {
        switch self {
        case .need: return Tok.needSoft
        case .run: return Tok.runSoft
        case .ok: return Tok.okSoft
        case .err: return Tok.errSoft
        case .sub: return Tok.subSoft
        case .mute: return Tok.panel3
        case .acc: return Tok.accentSoft
        }
    }
}

/// Status dot (`.wsd`): need, run, err; others use the idle token.
struct StatusDot: View {
    var color: Color
    var size: CGFloat = 7
    @ScaledMetric(relativeTo: .body) private var scale: CGFloat = 1
    var body: some View { Circle().fill(color).frame(width: size * scale, height: size * scale) }
}

/// Side-by-side action buttons that stack vertically at accessibility text sizes.
struct ActionRow<Content: View>: View {
    @Environment(\.dynamicTypeSize) private var dts
    @ViewBuilder let content: () -> Content
    var body: some View {
        let layout = dts.isAccessibilitySize ? AnyLayout(VStackLayout(spacing: 8)) : AnyLayout(HStackLayout(spacing: 8))
        layout { content() }
    }
}

/// The web `.pill`: 11 pt semibold on a soft tint.
struct Pill: View {
    let text: String
    var tone: Tone = .mute

    init(text: String, tone: Tone = .mute) { self.text = text; self.tone = tone }
    init(text: String, color: Color) {
        self.text = text
        self.tone = .mute
        self.custom = color
    }
    private var custom: Color?

    var body: some View {
        Text(text)
            .font(Typo.ui(11, .semibold))
            .lineLimit(1)
            .padding(.horizontal, 8).padding(.vertical, 1.5)
            .background(custom.map { $0.opacity(0.13) } ?? tone.soft, in: Capsule())
            .foregroundStyle(custom ?? tone.fg)
    }
}

/// Small pill showing a session's state.
struct StateBadge: View {
    let state: AgentState
    var body: some View {
        HStack(spacing: 5) {
            StatusDot(color: Theme.color(for: state), size: 6)
            Text(Theme.label(for: state))
        }
        .font(Typo.ui(11, .semibold))
        .padding(.horizontal, 8).padding(.vertical, 1.5)
        .background(Theme.color(for: state).opacity(0.12), in: Capsule())
        .foregroundStyle(Theme.color(for: state))
    }
}

/// Risk badge (`.risk`): uppercase, 10.5 pt bold.
struct RiskBadge: View {
    let risk: String
    var body: some View {
        let t: Tone = risk == "low" ? .ok : risk == "high" ? .err : .need
        Text("\(risk) risk".uppercased())
            .font(Typo.ui(10.5, .bold)).tracking(0.4)
            .padding(.horizontal, 6).padding(.vertical, 1)
            .background(t.soft, in: RoundedRectangle(cornerRadius: 4))
            .foregroundStyle(t.fg)
    }
}

/// Section label (`.eyebrow`): 11 pt semibold uppercase muted.
struct Eyebrow: View {
    let text: String
    var color: Color = Tok.muted
    var body: some View {
        Text(text.uppercased()).font(Typo.ui(11, .semibold)).tracking(0.6).foregroundStyle(color)
    }
}

// MARK: - Buttons (`.btn`)

struct AMButton: ButtonStyle {
    enum Kind { case normal, pri, allow, deny, ghost, accent }
    var kind: Kind = .normal
    /// Phone approval buttons are 12 pt padded and 14 pt, like the web mobile rule.
    var big = false
    var full = false
    @Environment(\.isEnabled) private var enabled

    func makeBody(configuration: Configuration) -> some View {
        let r: CGFloat = big ? 12 : 9
        configuration.label
            .font(big ? .body.weight(.semibold) : .subheadline.weight(.medium))
            .lineLimit(2)
            .multilineTextAlignment(.center)
            .padding(.horizontal, big ? 14 : 12)
            .padding(.vertical, big ? 12 : 7)
            .frame(maxWidth: full ? .infinity : nil, minHeight: big ? 48 : 36)
            .foregroundStyle(fg)
            .background(bg, in: RoundedRectangle(cornerRadius: r))
            .overlay(RoundedRectangle(cornerRadius: r).stroke(border, lineWidth: kind == .ghost || kind == .allow || kind == .pri ? 0 : 1))
            .opacity(enabled ? (configuration.isPressed ? 0.75 : 1) : 0.45)
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .animation(.snappy(duration: 0.15), value: configuration.isPressed)
            // A small button keeps its look; its tap area grows to 44 pt.
            .contentShape(Rectangle().inset(by: big ? 0 : -4))
    }

    private var fg: Color {
        switch kind {
        case .pri: return Tok.panel
        case .allow: return Tok.onOk
        case .deny: return Tok.err
        case .accent: return Tok.onAccent
        case .ghost: return Tok.fg2
        case .normal: return Tok.fg
        }
    }
    private var bg: Color {
        switch kind {
        case .pri: return Tok.fg
        case .allow: return Tok.ok
        case .accent: return Tok.accent
        case .ghost: return .clear
        case .deny: return Tok.errSoft
        default: return Tok.panel
        }
    }
    private var border: Color {
        switch kind {
        case .pri: return Tok.fg
        case .allow: return Tok.ok
        case .accent: return Tok.accent
        case .deny: return Tok.err.opacity(0.35)
        default: return Tok.line2
        }
    }
}

extension ButtonStyle where Self == AMButton {
    static func am(_ kind: AMButton.Kind = .normal, big: Bool = false, full: Bool = false) -> AMButton {
        AMButton(kind: kind, big: big, full: full)
    }
}

// MARK: - Surfaces

extension View {
    /// `.card` / `.wbox`: panel, hairline border, 11 pt radius.
    func amCard(padding: CGFloat = 12, radius: CGFloat = Radius.card, border: Color = Tok.line) -> some View {
        self.padding(padding)
            .background(Tok.panel, in: RoundedRectangle(cornerRadius: radius))
            .overlay(RoundedRectangle(cornerRadius: radius).stroke(border, lineWidth: 1))
    }

    /// `.wask`: the need-tinted card used for approvals, questions and plans.
    func askCard(_ color: Color = Tok.need) -> some View {
        self.padding(16)
            .background {
                ZStack {
                    RoundedRectangle(cornerRadius: 18, style: .continuous).fill(Tok.panel)
                    RoundedRectangle(cornerRadius: 18, style: .continuous).fill(color.opacity(0.06))
                }
            }
            .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).stroke(color.opacity(0.4), lineWidth: 1))
    }

    /// `.inp`: input field on panel-2 with a hairline that turns accent on focus.
    func amField(focused: Bool = false) -> some View {
        self.font(.body)
            .padding(.horizontal, 12).padding(.vertical, 10)
            .frame(minHeight: 44)
            .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).stroke(focused ? Tok.accent : Tok.line, lineWidth: 1))
    }

    /// Page background for a scroll view or list.
    func amPage(_ color: Color = Tok.bg) -> some View {
        self.scrollContentBackground(.hidden).background(color)
    }
}

/// Key/value line used across detail screens.
struct MetaRow: View {
    let label: String
    let value: String
    var mono: Bool = false
    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Text(label)
                .font(Typo.sm)
                .foregroundStyle(Tok.muted)
                .frame(width: 92, alignment: .leading)
            Text(value)
                .font(mono ? Typo.mono(12) : Typo.sm)
                .foregroundStyle(Tok.fg)
                .textSelection(.enabled)
            Spacer(minLength: 0)
        }
    }
}

/// `pre.out`: monospaced scrollable block for captures and tool output.
struct CodeBlock: View {
    let text: String
    var maxHeight: CGFloat? = nil
    var body: some View {
        ScrollView([.horizontal, .vertical]) {
            Text(text.isEmpty ? "-" : text)
                .font(Typo.mono(11.5))
                .foregroundStyle(Tok.fg)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 10).padding(.vertical, 8)
        }
        .frame(maxHeight: maxHeight)
        .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.btn))
        .overlay(RoundedRectangle(cornerRadius: Radius.btn).stroke(Tok.line, lineWidth: 1))
    }
}

/// `.state`: centred empty state with a bold line and a muted hint.
struct EmptyHint: View {
    let icon: String
    let title: String
    let subtitle: String
    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: icon).font(.system(size: 26, weight: .light)).foregroundStyle(Tok.line2)
            Text(title).font(Typo.ui(13, .semibold)).foregroundStyle(Tok.fg2)
            Text(subtitle).font(Typo.meta).foregroundStyle(Tok.muted)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.horizontal, 28)
        .padding(.top, 60)
    }
}

/// Inline error line on an err-soft tint (`.werr`).
struct ErrorBanner: View {
    let message: String
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text("✗").font(Typo.ui(12, .bold))
            Text(message).font(Typo.meta)
            Spacer(minLength: 0)
        }
        .foregroundStyle(Tok.err)
        .padding(.horizontal, 12).padding(.vertical, 9)
        .background(Tok.errSoft, in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Tok.err.opacity(0.4), lineWidth: 1))
        .padding(.horizontal, Space.gutter)
    }
}

// MARK: - Markdown

/// Inline markdown with code spans on a panel-3 tint and tappable links; bare URLs become links too.
func markdown(_ s: String) -> AttributedString {
    let opts = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
    let src = s.contains("`") ? s : SlackFormat.sub(s, #"(?<![\(<\[])\b(https?://[^\s)<>\]]+[^\s)<>\].,;:!?])"#) { "<\($0[1])>" }
    var a = (try? AttributedString(markdown: src, options: opts)) ?? AttributedString(s)
    for run in a.runs where run.inlinePresentationIntent?.contains(.code) == true {
        a[run.range].backgroundColor = Tok.panel3
        a[run.range].font = .system(.callout, design: .monospaced)
    }
    for run in a.runs where run.link != nil {
        a[run.range].foregroundColor = Tok.accent
        a[run.range].underlineStyle = .single
    }
    return a
}

/// Block markdown for agent prose (`.wagent`): headings, nested lists, quotes, tables and code fences.
struct Prose: View {
    let text: String
    var size: CGFloat = 14
    var color: Color = Tok.fg

    enum Block: Hashable {
        case para(String), heading(String, Int), bullet(String, String, Int), code(String, String), rule, quote(String), table([[String]])
    }

    var body: some View {
        VStack(alignment: .leading, spacing: size * 0.6) {
            ForEach(Array(Self.blocks(text).enumerated()), id: \.offset) { _, b in
                switch b {
                case .para(let s):
                    Text(markdown(s)).font(Typo.ui(size)).lineSpacing(size * 0.3)
                case .heading(let s, let level):
                    Text(markdown(s))
                        .font(Typo.ui(level == 1 ? size + 5 : level == 2 ? size + 3 : size + 1, level <= 2 ? .bold : .semibold))
                        .padding(.top, size * 0.4)
                        .accessibilityAddTraits(.isHeader)
                case .bullet(let mark, let s, let level):
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        Text(mark).font(Typo.ui(size, mark.count > 1 ? .medium : .regular)).foregroundStyle(Tok.muted)
                            .frame(minWidth: 16, alignment: .trailing)
                        Text(markdown(s)).font(Typo.ui(size)).lineSpacing(size * 0.3)
                    }
                    .padding(.leading, CGFloat(level) * 18)
                case .quote(let s):
                    Text(markdown(s)).font(Typo.ui(size)).foregroundStyle(Tok.fg2).lineSpacing(size * 0.3)
                        .padding(.leading, 12)
                        .overlay(alignment: .leading) { Rectangle().fill(Tok.line2).frame(width: 3) }
                case .code(let lang, let s):
                    CodeFence(lang: lang, code: s)
                case .table(let rows):
                    MarkdownTable(rows: rows, size: size)
                case .rule:
                    Rectangle().fill(Tok.line).frame(height: 1).padding(.vertical, 4)
                }
            }
        }
        .foregroundStyle(color)
        .textSelection(.enabled)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    static func blocks(_ text: String) -> [Block] {
        var out: [Block] = []
        var para: [String] = []
        var code: [String]? = nil
        var lang = ""
        var fence = "```"
        func flush() {
            if !para.isEmpty { out.append(.para(para.joined(separator: "\n"))); para = [] }
        }
        let lines = text.components(separatedBy: "\n")
        var i = 0
        while i < lines.count {
            let raw = lines[i]
            let line = raw.trimmingCharacters(in: .whitespaces)
            i += 1
            if code != nil {
                if line.hasPrefix(fence) { out.append(.code(lang, code!.joined(separator: "\n"))); code = nil } else { code?.append(raw) }
                continue
            }
            if line.hasPrefix("```") || line.hasPrefix("~~~") {
                flush()
                fence = String(line.prefix(3))
                lang = String(line.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                code = []
                continue
            }
            if line.isEmpty { flush(); continue }
            if line.contains("|"), i < lines.count, SlackFormat.first(lines[i], SlackFormat.tableSep) != nil, lines[i].contains("-") {
                flush()
                var rows = [tableCells(line)]
                i += 1
                while i < lines.count, lines[i].contains("|"), !lines[i].trimmingCharacters(in: .whitespaces).isEmpty {
                    rows.append(tableCells(lines[i])); i += 1
                }
                out.append(.table(rows))
                continue
            }
            if line == "---" || line == "***" || line == "___" { flush(); out.append(.rule); continue }
            if let h = line.firstIndex(where: { $0 != "#" }), line.hasPrefix("#"), line[h] == " " {
                flush()
                out.append(.heading(String(line[h...]).trimmingCharacters(in: .whitespaces), line.distance(from: line.startIndex, to: h)))
                continue
            }
            if line.hasPrefix(">") {
                flush()
                var q = [String(line.dropFirst()).trimmingCharacters(in: .whitespaces)]
                while i < lines.count, lines[i].trimmingCharacters(in: .whitespaces).hasPrefix(">") {
                    q.append(String(lines[i].trimmingCharacters(in: .whitespaces).dropFirst()).trimmingCharacters(in: .whitespaces)); i += 1
                }
                out.append(.quote(q.joined(separator: "\n")))
                continue
            }
            let indent = raw.prefix { $0 == " " || $0 == "\t" }.reduce(0) { $0 + ($1 == "\t" ? 4 : 1) }
            let level = min(indent / 2, 4)
            if line.hasPrefix("- ") || line.hasPrefix("* ") || line.hasPrefix("+ ") {
                flush()
                var body = String(line.dropFirst(2))
                var mark = level == 0 ? "•" : level == 1 ? "◦" : "▪"
                if body.hasPrefix("[ ] ") { mark = "☐"; body = String(body.dropFirst(4)) }
                if body.lowercased().hasPrefix("[x] ") { mark = "☑"; body = String(body.dropFirst(4)) }
                out.append(.bullet(mark, body, level))
                continue
            }
            if let dot = line.firstIndex(where: { $0 == "." || $0 == ")" }), dot > line.startIndex,
               line[..<dot].allSatisfy(\.isNumber), line[line.index(after: dot)...].hasPrefix(" ") {
                flush()
                out.append(.bullet(String(line[...dot]), String(line[line.index(dot, offsetBy: 2)...]), level))
                continue
            }
            para.append(raw)
        }
        if let c = code { out.append(.code(lang, c.joined(separator: "\n"))) }
        flush()
        return out
    }

    private static func tableCells(_ l: String) -> [String] {
        var t = l.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("|") { t.removeFirst() }
        if t.hasSuffix("|") { t.removeLast() }
        return t.components(separatedBy: "|").map { $0.trimmingCharacters(in: .whitespaces) }
    }
}

extension Notification.Name {
    /// Insert on a code block: userInfo "code" and "lang"; the chat's composer puts it in its box.
    static let amFillComposer = Notification.Name("amFillComposer")
}

private struct CanFillComposerKey: EnvironmentKey { static let defaultValue = false }
extension EnvironmentValues {
    /// True inside a chat that has a composer, so code blocks offer Insert.
    var canFillComposer: Bool {
        get { self[CanFillComposerKey.self] }
        set { self[CanFillComposerKey.self] = newValue }
    }
}



/// A fenced code block: a header row with the language and a copy control, then code that scrolls sideways.
struct CodeFence: View {
    let lang: String
    let code: String
    @Environment(\.canFillComposer) private var canFill
    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 6) {
                Text(lang.isEmpty ? "code" : lang).font(Typo.mono(11.5, .medium)).foregroundStyle(Tok.muted)
                Spacer()
                if canFill {
                    Button("Insert") {
                        NotificationCenter.default.post(name: .amFillComposer, object: nil, userInfo: ["code": code, "lang": lang])
                    }
                    .font(Typo.ui(12.5, .medium)).buttonStyle(.am(.ghost))
                    .accessibilityHint("Puts this in the message box. It is not sent.")
                }
                CopyButton(markdown: cleanCommand(code), isCode: true)
            }
            .padding(.leading, 12).padding(.trailing, 4)
            .frame(minHeight: 34)
            .background(Tok.panel3.opacity(0.6))
            Rectangle().fill(Tok.line).frame(height: 1)
            ScrollView(.horizontal, showsIndicators: false) {
                Text(code).font(Typo.mono(13)).foregroundStyle(Tok.fg)
                    .padding(.horizontal, 12).padding(.vertical, 10)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Tok.panel2, in: RoundedRectangle(cornerRadius: Radius.box))
        .clipShape(RoundedRectangle(cornerRadius: Radius.box))
        .overlay(RoundedRectangle(cornerRadius: Radius.box).stroke(Tok.line, lineWidth: 1))
        .contextMenu { CopyMenuItems(markdown: cleanCommand(code), isCode: true) }
    }
}

/// A markdown table in a grid that scrolls sideways; the first row is the header.
struct MarkdownTable: View {
    let rows: [[String]]
    var size: CGFloat = 15
    /// One fixed width per column from its longest cell, so wrapped rows are measured at the width they are drawn.
    private func widths(_ n: Int) -> [CGFloat] {
        let ch = (size - 2) * 0.55
        return (0..<n).map { c in
            let longest = rows.map { c < $0.count ? $0[c].count : 0 }.max() ?? 0
            return min(260, max(70, CGFloat(longest) * ch + 20))
        }
    }

    var body: some View {
        let n = rows.map(\.count).max() ?? 0
        let w = widths(n)
        ScrollView(.horizontal, showsIndicators: true) {
            Grid(alignment: .topLeading, horizontalSpacing: 0, verticalSpacing: 0) {
                ForEach(Array(rows.enumerated()), id: \.offset) { r, row in
                    GridRow {
                        ForEach(0..<n, id: \.self) { c in
                            Text(markdown(c < row.count ? row[c] : ""))
                                .font(Typo.ui(size - 2, r == 0 ? .semibold : .regular))
                                .fixedSize(horizontal: false, vertical: true)
                                .frame(width: w[c] - 20, alignment: .topLeading)
                                .padding(.horizontal, 10).padding(.vertical, 7)
                                .frame(maxHeight: .infinity, alignment: .topLeading)
                                .background(r == 0 ? Tok.panel3 : r % 2 == 0 ? Tok.panel2 : Color.clear)
                        }
                    }
                    if r == 0 { Rectangle().fill(Tok.line2).frame(height: 1).gridCellColumns(n) }
                }
            }
            .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
            .clipShape(RoundedRectangle(cornerRadius: Radius.row))
        }
        .contextMenu {
            let md = rows.map { "| " + $0.joined(separator: " | ") + " |" }
            let all = ([md[0], "|" + String(repeating: " --- |", count: n)] + md.dropFirst()).joined(separator: "\n")
            CopyMenuItems(markdown: all, label: "Copy Table")
        }
    }
}

/// Long prose folds to a fixed height with Show more; short prose renders as is.
struct FoldedProse: View {
    let text: String
    var size: CGFloat = 17
    @State private var open = false
    private var long: Bool { text.count > 1800 || text.components(separatedBy: "\n").count > 40 }

    var body: some View {
        if !long {
            Prose(text: text, size: size)
        } else {
            VStack(alignment: .leading, spacing: 6) {
                Prose(text: text, size: size)
                    .frame(maxHeight: open ? nil : 460, alignment: .top)
                    .clipped()
                    .overlay(alignment: .bottom) {
                        if !open {
                            LinearGradient(colors: [Tok.panel.opacity(0), Tok.panel], startPoint: .top, endPoint: .bottom)
                                .frame(height: 70).allowsHitTesting(false)
                        }
                    }
                Button { withAnimation(.snappy) { open.toggle() } } label: {
                    Label(open ? "Show less" : "Show more", systemImage: open ? "chevron.up" : "chevron.down")
                        .font(.subheadline.weight(.semibold))
                        .frame(minHeight: 36)
                }
                .buttonStyle(.borderless)
                .tint(Tok.accent)
            }
        }
    }
}

/// Colour for a hook approval's risk level.
func riskColor(_ risk: String) -> Color {
    switch risk {
    case "low": return Tok.ok
    case "high": return Tok.err
    default: return Tok.need
    }
}

// MARK: - Ask card parts

/// One answer option (`.wq-o`): radio or square box, label and description; accent ring when picked.
struct AskOption: View {
    let label: String
    var detail: String?
    var on: Bool
    var multi: Bool

    var body: some View {
        HStack(alignment: .top, spacing: 9) {
            ZStack {
                if multi {
                    RoundedRectangle(cornerRadius: 4).fill(on ? Tok.accent : .clear)
                    RoundedRectangle(cornerRadius: 4).stroke(on ? Tok.accent : Tok.line2, lineWidth: 1.5)
                    if on { Image(systemName: "checkmark").font(.system(size: 10, weight: .bold)).foregroundStyle(Tok.panel) }
                } else {
                    Circle().stroke(on ? Tok.accent : Tok.line2, lineWidth: 1.5)
                    if on { Circle().fill(Tok.accent).padding(4) }
                }
            }
            .frame(width: 18, height: 18)
            .padding(.top, 2)
            VStack(alignment: .leading, spacing: 2) {
                Text(label).font(.body.weight(.medium)).foregroundStyle(Tok.fg)
                if let d = detail, !d.isEmpty {
                    Text(d).font(.subheadline).foregroundStyle(Tok.fg2).lineSpacing(1)
                }
            }
            Spacer(minLength: 0)
        }
        .multilineTextAlignment(.leading)
        .padding(.horizontal, 12).padding(.vertical, 11)
        .frame(minHeight: 44)
        .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.box))
        .overlay(RoundedRectangle(cornerRadius: Radius.box).stroke(on ? Tok.accent : Tok.line, lineWidth: 1))
        .background(RoundedRectangle(cornerRadius: Radius.box + 2).fill(on ? Tok.accentSoft : .clear).padding(-2))
        .contentShape(Rectangle())
    }
}

/// `.wplan`: a plan in prose on a panel box, scrolling past 420 pt.
struct PlanBox: View {
    let text: String
    var body: some View {
        ScrollView {
            Prose(text: text, size: 13.5).padding(.horizontal, 14).padding(.vertical, 10)
        }
        .frame(maxHeight: 420)
        .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.row))
        .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
    }
}

/// `pre` inside an ask card: monospaced on a panel box.
struct AskPre: View {
    let text: String
    var lines: Int? = 10
    var body: some View {
        Text(text)
            .font(Typo.mono(12.5))
            .foregroundStyle(Tok.fg)
            .lineLimit(lines)
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 10).padding(.vertical, 8)
            .background(Tok.panel, in: RoundedRectangle(cornerRadius: Radius.row))
            .overlay(RoundedRectangle(cornerRadius: Radius.row).stroke(Tok.line, lineWidth: 1))
    }
}

/// Press tint for custom tappable rows outside a List.
struct RowPress: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .background(configuration.isPressed ? Tok.fg.opacity(0.06) : .clear, in: RoundedRectangle(cornerRadius: Radius.row))
            .contentShape(Rectangle())
    }
}
