import SwiftUI
import UIKit

// Desk tokens copied from web/src/v3/styles/v3.css (light = :root[data-theme=light], dark = .v3).
enum Tok {
    static let bg = Color(light: 0xf4f3ef, dark: 0x121210)
    static let panel = Color(light: 0xfffefb, dark: 0x1a1a17)
    static let panel2 = Color(light: 0xf8f7f3, dark: 0x1e1e1a)
    static let panel3 = Color(light: 0xefede6, dark: 0x282823)
    static let line = Color(light: 0xe3e0d8, dark: 0x33322c)
    static let line2 = Color(light: 0xd2cec4, dark: 0x45443d)
    static let fg = Color(light: 0x1d1c19, dark: 0xecebe6)
    static let fg2 = Color(light: 0x57534b, dark: 0xb0ada3)
    static let muted = Color(light: 0x716c62, dark: 0x918d82)
    static let accent = Color(light: 0x2f5bd3, dark: 0x86a3ff)
    static let accentSoft = Color(light: 0x2f5bd3, dark: 0x86a3ff, la: 0.09, da: 0.12)
    static let need = Color(light: 0x9a5a08, dark: 0xf0ae4a)
    static let needSoft = Color(light: 0xc47814, dark: 0xf0ae4a, la: 0.11, da: 0.13)
    static let run = Color(light: 0x0d7f95, dark: 0x4cc3d8)
    static let runSoft = Color(light: 0x0d7f95, dark: 0x4cc3d8, la: 0.10, da: 0.12)
    static let ok = Color(light: 0x2c7d4f, dark: 0x5dc48a)
    static let okSoft = Color(light: 0x2c7d4f, dark: 0x5dc48a, la: 0.10, da: 0.12)
    static let err = Color(light: 0xc23b33, dark: 0xf07068)
    static let errSoft = Color(light: 0xc23b33, dark: 0xf07068, la: 0.09, da: 0.12)
    static let claude = Color(light: 0xb3552f, dark: 0xe08562)
    static let codex = Color(light: 0x1f7f68, dark: 0x4cbf9c)
    static let sub = Color(light: 0x7a58c4, dark: 0xa88cf0)
    static let subSoft = Color(light: 0x7a58c4, dark: 0xa88cf0, la: 0.10, da: 0.14)
    static let opencode = Color(light: 0xb3447a, dark: 0xe57aa8)
    static let gemini = Color(light: 0x5d7f1c, dark: 0xa9cc5c)
    static let cursor = Color(light: 0x4b5260, dark: 0xb9bfca)
    static let idle = Color(light: 0xcfcac0, dark: 0x4a4943)
    static let read = Color(light: 0x4f7fd6, dark: 0x86a3ff)
    static let onOk = Color(light: 0xffffff, dark: 0x0d2316)
    /// Text and icons on the accent or a coloured tile: white in light mode, near black on the light dark-mode colours.
    static let onAccent = Color(light: 0xffffff, dark: 0x14161c)

    /// The agent colour token (--claude, --codex, ...); unknown agents use --muted.
    static func agent(_ a: String) -> Color {
        switch agentKey(a) {
        case "claude": return claude
        case "codex": return codex
        case "opencode": return opencode
        case "gemini": return gemini
        case "cursor": return cursor
        default: return muted
        }
    }
}

// Type scale on iOS text styles so every size follows Dynamic Type; New York for titles, SF Mono for code.
enum Typo {
    static func ui(_ size: CGFloat, _ w: Font.Weight = .regular) -> Font { .system(style(size), weight: w) }
    static func mono(_ size: CGFloat, _ w: Font.Weight = .regular) -> Font { .system(style(size), design: .monospaced, weight: w) }
    static func serif(_ size: CGFloat, _ w: Font.Weight = .semibold) -> Font { .system(style(size), design: .serif, weight: w) }
    static let xs = ui(11)
    static let sm = ui(12)
    static let meta = ui(13)
    static let body = ui(15)
    static let prose = ui(17)
    static let h3 = serif(20)
    static let h2 = serif(28)

    /// Nearest iOS text style for a web pixel size.
    static func style(_ size: CGFloat) -> Font.TextStyle {
        switch size {
        case ..<11.5: return .caption2
        case ..<12.5: return .caption
        case ..<14: return .footnote
        case ..<15.5: return .subheadline
        case ..<16.5: return .callout
        case ..<19: return .body
        case ..<21: return .title3
        case ..<25: return .title2
        case ..<31: return .title
        default: return .largeTitle
        }
    }
}

enum Space {
    static let xs: CGFloat = 4, s: CGFloat = 6, m: CGFloat = 8, l: CGFloat = 12, xl: CGFloat = 16, xxl: CGFloat = 20
    static let gutter: CGFloat = 16
}

enum Radius {
    static let chip: CGFloat = 6, btn: CGFloat = 7, row: CGFloat = 8, box: CGFloat = 9
    static let card: CGFloat = 11, ask: CGFloat = 12, bubble: CGFloat = 14, composer: CGFloat = 14
}

extension Color {
    /// A light/dark pair resolved per trait collection, like the web's two token sets.
    init(light: UInt32, dark: UInt32, la: CGFloat = 1, da: CGFloat = 1) {
        self.init(uiColor: UIColor { t in
            t.userInterfaceStyle == .dark ? UIColor(hex: dark, alpha: da) : UIColor(hex: light, alpha: la)
        })
    }
}

extension UIColor {
    convenience init(hex: UInt32, alpha: CGFloat = 1) {
        self.init(red: CGFloat((hex >> 16) & 0xff) / 255, green: CGFloat((hex >> 8) & 0xff) / 255,
                  blue: CGFloat(hex & 0xff) / 255, alpha: alpha)
    }
}

/// Normalises "cursor-agent" or "claude:<uuid>" to an icon key, as AgentIcon.tsx does.
func agentKey(_ agent: String) -> String {
    let a = (agent.lowercased().split(separator: ":").first.map(String.init)) ?? ""
    for k in ["cursor", "claude", "codex", "opencode", "gemini"] where a.hasPrefix(k) { return k }
    if a == "pi" || a.hasPrefix("pi-") { return "pi" }
    return a
}

func agentName(_ agent: String) -> String {
    switch agentKey(agent) {
    case "claude": return "Claude Code"
    case "codex": return "Codex"
    case "opencode": return "OpenCode"
    case "gemini": return "Gemini"
    case "cursor": return "Cursor"
    case "pi": return "Pi"
    case "": return "Agent"
    default: return agent.capitalized
    }
}

enum Theme {
    static func color(for state: AgentState) -> Color {
        switch state {
        case .running: return Tok.run
        case .awaitingPermission: return Tok.need
        case .awaitingInput, .idle: return Tok.ok
        case .completed: return Tok.muted
        case .abandoned, .unknown: return Tok.idle
        }
    }

    static func label(for state: AgentState) -> String {
        switch state {
        case .running: return "Working"
        case .awaitingPermission: return "Needs you"
        case .awaitingInput, .idle: return "Ready"
        case .completed, .abandoned: return "Done"
        case .unknown: return "Unknown"
        }
    }

    static func icon(for tool: String) -> String {
        switch agentKey(tool) {
        case "claude": return "sparkles"
        case "codex": return "chevron.left.forwardslash.chevron.right"
        case "opencode": return "curlybraces"
        case "cursor": return "cursorarrow.rays"
        default: return "terminal"
        }
    }

    /// Nav bars: paper behind large serif titles, system blur once content scrolls under; tab bar stays system.
    static func applyAppearance() {
        let ui = { (c: Color) in UIColor(c) }
        let serif = UIFont.systemFont(ofSize: 34, weight: .bold).fontDescriptor.withDesign(.serif)
            .map { UIFont(descriptor: $0, size: 34) } ?? UIFont.boldSystemFont(ofSize: 34)
        let large = UIFontMetrics(forTextStyle: .largeTitle).scaledFont(for: serif)
        let inline = UIFontMetrics(forTextStyle: .headline).scaledFont(for: .systemFont(ofSize: 17, weight: .semibold))
        func style(_ a: UINavigationBarAppearance) {
            a.largeTitleTextAttributes = [.foregroundColor: ui(Tok.fg), .font: large]
            a.titleTextAttributes = [.foregroundColor: ui(Tok.fg), .font: inline]
        }
        let std = UINavigationBarAppearance()
        std.configureWithDefaultBackground()
        style(std)
        let edge = UINavigationBarAppearance()
        edge.configureWithTransparentBackground()
        style(edge)
        UINavigationBar.appearance().standardAppearance = std
        UINavigationBar.appearance().compactAppearance = std
        UINavigationBar.appearance().scrollEdgeAppearance = edge
    }
}

/// Compact relative time from a unix-millis timestamp.
func relativeTime(_ millis: Int64) -> String {
    guard millis > 0 else { return "-" }
    let secs = Int((Double(Date().timeIntervalSince1970) - Double(millis) / 1000.0))
    if secs < 5 { return "now" }
    if secs < 60 { return "\(secs)s" }
    let mins = secs / 60
    if mins < 60 { return "\(mins)m" }
    let hrs = mins / 60
    if hrs < 24 { return "\(hrs)h" }
    return "\(hrs / 24)d"
}

/// Human token count: 12345 -> "12.3k".
func compactCount(_ n: Int64) -> String {
    if n < 1000 { return "\(n)" }
    if n < 1_000_000 { return String(format: "%.1fk", Double(n) / 1000) }
    return String(format: "%.1fM", Double(n) / 1_000_000)
}

/// Duration in ms as "820ms", "4.2s", "3m 10s".
func fmtDuration(_ ms: Int64) -> String {
    if ms < 1000 { return "\(ms)ms" }
    let s = Double(ms) / 1000
    if s < 60 { return String(format: "%.1fs", s) }
    let m = Int(s) / 60
    return "\(m)m \(Int(s) % 60)s"
}
