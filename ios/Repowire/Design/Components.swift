import RepowireKit
import SwiftUI

extension Peer.Status {
    var color: Color {
        switch self {
        case .online: Theme.Palette.success
        case .busy: Theme.Palette.warning
        case .offline: Theme.Palette.faint
        }
    }

    var label: String {
        switch self {
        case .online: "Online"
        case .busy: "Busy"
        case .offline: "Offline"
        }
    }
}

/// Status is never color alone: the dot pairs with a label for VoiceOver.
struct StatusDot: View {
    let status: Peer.Status

    var body: some View {
        Circle()
            .fill(status.color)
            .frame(width: Theme.Size.statusDot, height: Theme.Size.statusDot)
            .accessibilityLabel(status.label)
    }
}

/// Uppercase mono label for section eyebrows and badges.
struct Eyebrow: View {
    let text: String
    var color: Color = Theme.Palette.muted

    var body: some View {
        Text(text.uppercased())
            .font(Theme.Typeface.eyebrow)
            .tracking(0.8)
            .foregroundStyle(color)
    }
}

struct Badge: View {
    let text: String
    var tint: Color = Theme.Palette.muted

    var body: some View {
        Text(text)
            .font(Theme.Typeface.mono(.caption2, weight: .medium))
            .foregroundStyle(tint)
            .padding(.horizontal, Theme.Space.s)
            .padding(.vertical, Theme.Space.xxs)
            .background(Capsule().fill(tint.opacity(0.12)))
            .lineLimit(1)
            .fixedSize()
    }
}

/// A row header that sits on one line at regular sizes and stacks at
/// accessibility text sizes, so names and badges never hyphenate or squeeze.
struct HeaderRow<Leading: View, Trailing: View>: View {
    @Environment(\.dynamicTypeSize) private var size
    @ViewBuilder var leading: Leading
    @ViewBuilder var trailing: Trailing

    var body: some View {
        if size.isAccessibilitySize {
            VStack(alignment: .leading, spacing: Theme.Space.xs) {
                leading
                trailing
            }
        } else {
            HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                leading
                Spacer(minLength: 0)
                trailing
            }
        }
    }
}

/// White card on warm paper with a hairline border, the system's base container.
struct Card<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.m) { content }
            .padding(Theme.Space.l)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).fill(Theme.Palette.surface))
            .overlay(RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).strokeBorder(Theme.Palette.border, lineWidth: Theme.Stroke.hairline))
    }
}

/// Primary is solid cobalt; secondary is a quiet bordered control. Feedback
/// lands on press, not release.
struct RWButtonStyle: ButtonStyle {
    enum Kind { case primary, secondary, destructive }
    var kind: Kind = .primary

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.semibold))
            .frame(maxWidth: .infinity, minHeight: Theme.Size.tapTarget)
            .padding(.horizontal, Theme.Space.m)
            .foregroundStyle(foreground)
            .background(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).fill(background))
            .overlay(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).strokeBorder(border, lineWidth: Theme.Stroke.hairline))
            .opacity(configuration.isPressed ? 0.75 : 1)
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .animation(Theme.Motion.quick, value: configuration.isPressed)
    }

    private var foreground: Color {
        switch kind {
        case .primary: Theme.Palette.onAccent
        case .secondary: Theme.Palette.foreground
        case .destructive: Theme.Palette.danger
        }
    }

    private var background: Color {
        kind == .primary ? Theme.Palette.accent : Theme.Palette.surface
    }

    private var border: Color {
        kind == .primary ? .clear : Theme.Palette.border
    }
}

extension ButtonStyle where Self == RWButtonStyle {
    static var rwPrimary: RWButtonStyle { RWButtonStyle(kind: .primary) }
    static var rwSecondary: RWButtonStyle { RWButtonStyle(kind: .secondary) }
    static var rwDestructive: RWButtonStyle { RWButtonStyle(kind: .destructive) }
}

/// "2m ago" in mono, relative to now, refreshed by the system.
struct Timestamp: View {
    let date: Date?
    var tint: Color = Theme.Palette.faint

    var body: some View {
        if let date {
            Text(date, format: .relative(presentation: .numeric, unitsStyle: .abbreviated))
                .font(Theme.Typeface.mono(.caption2))
                .foregroundStyle(tint)
                .monospacedDigit()
        }
    }
}

extension View {
    /// Warm-paper page background behind lists and scroll views.
    func pageBackground() -> some View {
        scrollContentBackground(.hidden).background(Theme.Palette.page)
    }
}
