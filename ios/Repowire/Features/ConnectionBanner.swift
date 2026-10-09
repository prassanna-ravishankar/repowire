import SwiftUI

/// Visible only when the live stream is not live. Fail loud, but calmly.
struct ConnectionBanner: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        if let text {
            HStack(spacing: Theme.Space.s) {
                Image(systemName: icon)
                Text(text)
                    .lineLimit(2)
                Spacer(minLength: 0)
            }
            .font(.footnote.weight(.medium))
            .foregroundStyle(tint)
            .padding(Theme.Space.m)
            .background(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).fill(tint.opacity(0.1)))
            .accessibilityIdentifier("connection.banner")
        }
    }

    private var text: String? {
        switch model.connection {
        case .live: model.lastError
        case .connecting: "Connecting to the relay"
        case let .reconnecting(seconds): "Connection lost. Retrying in \(seconds)s."
        case let .failed(message): message
        }
    }

    private var icon: String {
        switch model.connection {
        case .live, .failed: "exclamationmark.triangle"
        case .connecting, .reconnecting: "arrow.triangle.2.circlepath"
        }
    }

    private var tint: Color {
        switch model.connection {
        case .live, .failed: Theme.Palette.danger
        case .connecting, .reconnecting: Theme.Palette.warning
        }
    }
}
