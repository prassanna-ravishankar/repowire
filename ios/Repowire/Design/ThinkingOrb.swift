import RepowireKit
import SwiftUI

/// Indeterminate "agent is working" indicator: a ring of dots with a wave of
/// light travelling around it. Shown only while a peer reports `working`; it
/// is never progress. Under Reduce Motion it draws one still frame.
struct ThinkingOrb: View {
    enum Preset {
        case compact, large

        var side: CGFloat { self == .compact ? Theme.Size.orbCompact : Theme.Size.orbLarge }
        var dots: Int { self == .compact ? 7 : 10 }
        /// Dot radius as a fraction of the side; tuned per preset.
        var dotScale: CGFloat { self == .compact ? 0.11 : 0.075 }
        /// Seconds per revolution of the wave.
        var period: Double { self == .compact ? 1.1 : 1.4 }
    }

    var preset: Preset = .compact
    var tint: Color = Theme.Palette.warning
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        TimelineView(.animation(paused: reduceMotion)) { context in
            Canvas { canvas, size in
                let phase = reduceMotion ? 0.6 : context.date.timeIntervalSinceReferenceDate / preset.period
                draw(in: &canvas, size: size, phase: phase)
            }
        }
        .frame(width: preset.side, height: preset.side)
        .accessibilityHidden(true)
    }

    private func draw(in canvas: inout GraphicsContext, size: CGSize, phase: Double) {
        let center = CGPoint(x: size.width / 2, y: size.height / 2)
        let dot = size.width * preset.dotScale
        let breathe = 1 + 0.06 * sin(phase * .pi * 2 / 3)
        let orbit = (size.width / 2 - dot * 1.6) * breathe
        let head = phase.truncatingRemainder(dividingBy: 1) * .pi * 2
        for index in 0..<preset.dots {
            let angle = Double(index) / Double(preset.dots) * .pi * 2
            // 1 at the head of the wave, falling off behind it.
            let lag = (head - angle).truncatingRemainder(dividingBy: .pi * 2)
            let wave = pow((cos(lag < 0 ? lag + .pi * 2 : lag) + 1) / 2, 2)
            let radius = dot * (0.55 + 0.45 * wave)
            let point = CGPoint(x: center.x + orbit * cos(angle - .pi / 2), y: center.y + orbit * sin(angle - .pi / 2))
            canvas.fill(Path(ellipseIn: CGRect(x: point.x - radius, y: point.y - radius, width: radius * 2, height: radius * 2)),
                        with: .color(tint.opacity(0.25 + 0.75 * wave)))
        }
    }
}

extension Peer {
    /// The daemon reports `turn_state`; only `working` animates.
    var isWorking: Bool { turnState == "working" }
}

/// Status glyph for a peer: the orb while it works, the dot otherwise.
struct PeerActivity: View {
    let peer: Peer
    var preset: ThinkingOrb.Preset = .compact

    var body: some View {
        if peer.isWorking {
            ThinkingOrb(preset: preset)
        } else {
            StatusDot(status: peer.status)
                .frame(width: Theme.Size.orbCompact, height: Theme.Size.orbCompact)
        }
    }
}

#Preview {
    HStack(spacing: Theme.Space.xl) {
        ThinkingOrb()
        ThinkingOrb(preset: .large)
    }
    .padding(Theme.Space.xl)
}
