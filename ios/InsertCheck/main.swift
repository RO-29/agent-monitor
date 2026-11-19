import Foundation

// Runs testdata/insert_cases.json against AgentMonitor/Insert.swift; built by ios/check-insert.sh.
struct Case: Decodable { let name, raw, lang, cur, want: String; let bash: Bool }

let path = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "testdata/insert_cases.json"
let cases = try JSONDecoder().decode([Case].self, from: Data(contentsOf: URL(fileURLWithPath: path)))
var bad = 0
for c in cases {
    let got = insertFor(c.raw, lang: c.lang, bash: c.bash, cur: c.cur)
    if got != c.want {
        bad += 1
        print("FAIL \(c.name)\n  got  \(got.debugDescription)\n  want \(c.want.debugDescription)")
    }
}
print(bad > 0 ? "\(bad) of \(cases.count) failed" : "all \(cases.count) insert cases passed")
exit(bad > 0 ? 1 : 0)
