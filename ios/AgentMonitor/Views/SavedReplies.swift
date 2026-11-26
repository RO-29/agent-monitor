import SwiftUI

/// Saved replies: one tap puts a common message in the box; long press edits the list.
struct SavedRepliesBar: View {
    @Binding var text: String
    @AppStorage("chat.savedReplies") private var raw = "continue\nRun the tests and fix what fails\nCommit and open a draft PR\nSummarize what changed"
    @State private var editing = false

    private var list: [String] { raw.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty } }

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 6) {
                ForEach(list, id: \.self) { r in
                    Button { text = r } label: {
                        Text(r).font(.footnote).lineLimit(1)
                            .padding(.horizontal, 10).padding(.vertical, 6)
                            .background(Tok.panel, in: Capsule())
                            .overlay(Capsule().stroke(Tok.line, lineWidth: 1))
                            .frame(minHeight: 44)
                    }
                    .buttonStyle(.plain)
                    .accessibilityHint("Puts this text in the message box")
                }
                Button { editing = true } label: {
                    Image(systemName: "pencil").font(.footnote).frame(width: 44, height: 44)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Edit saved replies")
            }
            .padding(.horizontal, 6)
        }
        .sheet(isPresented: $editing) { SavedRepliesEditor(raw: $raw) }
    }
}

private struct SavedRepliesEditor: View {
    @Binding var raw: String
    @Environment(\.dismiss) private var dismiss
    @State private var draft = ""

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $draft).font(.body).frame(minHeight: 200)
                } footer: {
                    Text("One reply on each line.")
                }
            }
            .navigationTitle("Saved replies")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { raw = draft; dismiss() }
                }
            }
            .onAppear { draft = raw }
        }
    }
}
