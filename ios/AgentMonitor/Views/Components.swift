import SwiftUI

/// Small pill showing a session's state with its colour.
struct StateBadge: View {
    let state: AgentState
    var body: some View {
        HStack(spacing: 4) {
            Circle().fill(Theme.color(for: state)).frame(width: 7, height: 7)
            Text(Theme.label(for: state))
                .font(.caption2.weight(.medium))
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(Theme.color(for: state).opacity(0.15), in: Capsule())
        .foregroundStyle(Theme.color(for: state))
    }
}

/// Key/value line used across detail screens.
struct MetaRow: View {
    let label: String
    let value: String
    var mono: Bool = false
    var body: some View {
        HStack(alignment: .top) {
            Text(label)
                .font(.caption)
                .foregroundStyle(.secondary)
                .frame(width: 92, alignment: .leading)
            Text(value)
                .font(mono ? .caption.monospaced() : .caption)
                .textSelection(.enabled)
            Spacer(minLength: 0)
        }
    }
}

/// Monospaced scrollable block for terminal captures / tool output.
struct CodeBlock: View {
    let text: String
    var body: some View {
        ScrollView([.horizontal, .vertical]) {
            Text(text.isEmpty ? "—" : text)
                .font(.system(.caption2, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(10)
        }
        .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
    }
}

/// Empty-state placeholder.
struct EmptyHint: View {
    let icon: String
    let title: String
    let subtitle: String
    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: icon).font(.largeTitle).foregroundStyle(.tertiary)
            Text(title).font(.headline)
            Text(subtitle).font(.caption).foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.top, 60)
    }
}

/// Inline error banner used at the top of tabs.
struct ErrorBanner: View {
    let message: String
    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
            Text(message).font(.caption)
            Spacer(minLength: 0)
        }
        .padding(8)
        .background(Color.red.opacity(0.12), in: RoundedRectangle(cornerRadius: 8))
        .foregroundStyle(.red)
        .padding(.horizontal)
    }
}
