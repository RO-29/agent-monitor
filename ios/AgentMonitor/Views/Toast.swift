import SwiftUI
import UIKit

/// A short confirmation shown at the top of the screen, with a haptic.
@MainActor
@Observable
final class ToastCenter {
    static let shared = ToastCenter()
    var text: String?
    var icon = "checkmark.circle.fill"
    var seq = 0
    var isError = false

    func show(_ text: String, icon: String = "checkmark.circle.fill") {
        self.text = text
        self.icon = icon
        // An error icon means a failure: error haptic and longer on screen. VoiceOver reads every toast.
        isError = icon.hasPrefix("exclamationmark") || icon.hasPrefix("xmark")
        AccessibilityNotification.Announcement(text).post()
        seq += 1
        let mine = seq
        let wait: UInt64 = isError ? 4_500_000_000 : 1_600_000_000
        Task {
            try? await Task.sleep(nanoseconds: wait)
            if self.seq == mine { withAnimation(.snappy) { self.text = nil } }
        }
    }
}

/// Copies text and confirms with a haptic and a "Copied" toast.
@MainActor
func copyToPasteboard(_ text: String, label: String = "Copied") {
    UIPasteboard.general.string = text
    ToastCenter.shared.show(label)
}


private struct ToastHost: ViewModifier {
    @State private var center = ToastCenter.shared
    func body(content: Content) -> some View {
        content
            .overlay(alignment: .top) {
                if let t = center.text {
                    Label(t, systemImage: center.icon)
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(Tok.fg)
                        .padding(.horizontal, 16).padding(.vertical, 10)
                        .background(.regularMaterial, in: Capsule())
                        .overlay(Capsule().stroke(Tok.line, lineWidth: 0.5))
                        .shadow(color: .black.opacity(0.12), radius: 10, y: 3)
                        .padding(.top, 6)
                        .transition(.move(edge: .top).combined(with: .opacity))
                        .accessibilityAddTraits(.isStaticText)
                        .allowsHitTesting(false)
                }
            }
            .animation(.snappy, value: center.text)
            .sensoryFeedback(trigger: center.seq) { _, _ in center.isError ? .error : .success }
    }
}

extension View {
    /// Shows ToastCenter messages over this view; apply at the root and on each sheet.
    func toastHost() -> some View { modifier(ToastHost()) }
}
