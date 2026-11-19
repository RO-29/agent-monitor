import Foundation

// Copy and Insert text for code blocks; web/src/lib/insert.ts is the same, both checked by testdata/insert_cases.json.

private let shellLangs: Set<String> = ["sh", "bash", "zsh", "fish", "shell", "console", "terminal", "shellsession", "shell-session"]
private let promptRE = #"^\s*[$!] "#

private func isPrompt(_ l: String) -> Bool { l.range(of: promptRE, options: .regularExpression) != nil }

/// The text to paste into a shell: a console transcript keeps only its commands, and "$ "/"! " prompts come off.
func copyableCode(_ code: String, lang: String = "") -> String {
    let body = code.replacingOccurrences(of: #"\s+$"#, with: "", options: .regularExpression)
    let lines = body.components(separatedBy: "\n")
    if !lang.isEmpty && !shellLangs.contains(lang.lowercased()) { return body }
    let prompted = lines.filter(isPrompt)
    if prompted.isEmpty { return body }
    let mixed = prompted.count < lines.filter { !$0.trimmingCharacters(in: .whitespaces).isEmpty }.count
    return (mixed ? prompted : lines).map { $0.replacingOccurrences(of: promptRE, with: "", options: .regularExpression) }.joined(separator: "\n")
}

/// A shell command as the user would paste it (Copy buttons with no fence language).
func cleanCommand(_ s: String) -> String { copyableCode(s.trimmingCharacters(in: .whitespacesAndNewlines)) }

/// A block with no language that starts with "! " or "$ " is shell.
func blockLang(_ raw: String, _ lang: String) -> String { !lang.isEmpty ? lang : (isPrompt(raw) ? "sh" : "") }

/// Box text for Insert: a one-line shell command becomes "!cmd" where the box has bash mode; longer blocks stay as they are.
func fillText(_ code: String, lang: String, bash: Bool) -> String {
    let c = code.trimmingCharacters(in: .whitespacesAndNewlines)
    guard bash, shellLangs.contains(lang.lowercased()) else { return c }
    let one = c.replacingOccurrences(of: #"\s*\\\n\s*"#, with: " ", options: .regularExpression)
    return one.contains("\n") ? c : "!" + one
}

/// Insert adds to a draft; a "!" command only counts at the start of the box.
func withInsert(_ cur: String, _ add: String) -> String {
    let head = cur.replacingOccurrences(of: #"\s+$"#, with: "", options: .regularExpression)
    guard !head.isEmpty else { return add }
    return head + "\n" + (add.hasPrefix("!") && !cur.hasPrefix("!") ? String(add.dropFirst()) : add)
}

/// The whole Insert step: raw block text and its fence language to the new box text.
func insertFor(_ raw: String, lang: String, bash: Bool, cur: String) -> String {
    let l = blockLang(raw, lang)
    return withInsert(cur, fillText(copyableCode(raw, lang: l), lang: l, bash: bash))
}
