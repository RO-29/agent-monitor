import SwiftUI

/// A list section whose header folds it; the folded state persists per key in UserDefaults.
struct FoldSection<Content: View, Accessory: View>: View {
    let title: String
    var tint: Color = Tok.muted
    var forceOpen = false
    @AppStorage private var folded: Bool
    private let accessory: Accessory
    private let content: Content

    init(_ title: String, key: String, foldedByDefault: Bool = false, tint: Color = Tok.muted, forceOpen: Bool = false,
         @ViewBuilder accessory: () -> Accessory = { EmptyView() }, @ViewBuilder content: () -> Content) {
        self.title = title
        self.tint = tint
        self.forceOpen = forceOpen
        _folded = AppStorage(wrappedValue: foldedByDefault, "fold." + key)
        self.accessory = accessory()
        self.content = content()
    }

    var body: some View {
        Section {
            if !folded || forceOpen { content }
        } header: {
            if forceOpen {
                FoldHeader(title: title, folded: .constant(false), tint: tint) { accessory }.allowsHitTesting(false)
            } else {
                FoldHeader(title: title, folded: $folded, tint: tint) { accessory }
            }
        }
    }
}

/// Tappable section header: title, optional trailing text, and a chevron that turns when open.
struct FoldHeader<Accessory: View>: View {
    let title: String
    @Binding var folded: Bool
    var tint: Color = Tok.muted
    @ViewBuilder var accessory: () -> Accessory

    var body: some View {
        Button {
            withAnimation(.snappy) { folded.toggle() }
        } label: {
            HStack(spacing: 6) {
                Text(title)
                Spacer(minLength: 8)
                accessory().monospacedDigit()
                Image(systemName: "chevron.right")
                    .font(.caption.weight(.semibold))
                    .rotationEffect(.degrees(folded ? 0 : 90))
                    .foregroundStyle(Tok.muted)
            }
            .font(.subheadline.weight(.semibold))
            .foregroundStyle(tint)
            .textCase(nil)
            .padding(.vertical, 4)
            .frame(minHeight: 32)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(.isHeader)
        .accessibilityValue(folded ? "Collapsed" : "Expanded")
        .accessibilityHint(folded ? "Shows this section" : "Hides this section")
    }
}
