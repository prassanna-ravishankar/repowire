import SwiftUI

/// The Repowire R as dots, shared with the relay landing page
/// (`daemon-go/relayserver/assets/landing.html`). On appear the dots orbit as a
/// tilted sphere, then settle into the mark once and hold it. Decorative: it
/// does not reflect connection state. Under Reduce Motion it is the static R.
struct RelayMark: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var start = Date.now
    var width: CGFloat = Theme.Size.relayMark
    var tint: Color = Theme.Palette.foreground

    var body: some View {
        TimelineView(.animation(paused: reduceMotion || isSettled)) { context in
            let progress = reduceMotion ? 1 : min(context.date.timeIntervalSince(start) / Reveal.duration, 1)
            Canvas { canvas, size in
                draw(in: &canvas, size: size, progress: progress)
            }
        }
        // Canvas has no intrinsic size; derive height from the landing viewBox.
        .frame(width: width, height: width * RelayMarkDots.viewBox.height / RelayMarkDots.viewBox.width)
        .accessibilityHidden(true)
    }

    private var isSettled: Bool { Date.now.timeIntervalSince(start) >= Reveal.duration }

    private func draw(in canvas: inout GraphicsContext, size: CGSize, progress: Double) {
        let scale = size.width / RelayMarkDots.viewBox.width
        let radius = RelayMarkDots.radius * scale
        let count = RelayMarkDots.points.count
        for (index, target) in RelayMarkDots.points.enumerated() {
            let (point, opacity) = Reveal.position(of: index, count: count, target: target, progress: progress)
            let center = CGPoint(x: point.x * scale, y: point.y * scale)
            let dot = CGRect(x: center.x - radius, y: center.y - radius, width: radius * 2, height: radius * 2)
            canvas.fill(Path(ellipseIn: dot), with: .color(tint.opacity(opacity)))
        }
    }
}

/// The landing page's reveal, step for step: 48% of the time orbiting, the
/// rest easing each dot into its place in the R.
nonisolated enum Reveal {
    static let duration: TimeInterval = 3.2
    private static let orbitShare = 0.48
    private static let orbitTravel = 0.85
    private static let orbitRadius: CGFloat = 74
    private static let tilt = (cos: 0.966, sin: 0.259)

    static func position(of index: Int, count: Int, target: CGPoint, progress: Double) -> (CGPoint, Double) {
        guard progress < 1 else { return (target, 1) }
        // A Fibonacci sphere gives each dot its own orbit, with no random jumps.
        let latitude = 1 - 2 * (Double(index) + 0.5) / Double(count)
        let ring = (1 - latitude * latitude).squareRoot()
        let longitude = Double(index) * .pi * (3 - 5.0.squareRoot())
        func orbit(_ phase: Double) -> (CGPoint, Double) {
            let angle = longitude + phase * orbitTravel
            let x = ring * cos(angle)
            let depth = ring * sin(angle)
            let center = CGPoint(x: RelayMarkDots.viewBox.width / 2, y: RelayMarkDots.viewBox.height / 2)
            let point = CGPoint(
                x: center.x + orbitRadius * (x * tilt.cos + latitude * tilt.sin),
                y: center.y + orbitRadius * (x * tilt.sin - latitude * tilt.cos)
            )
            return (point, 0.35 + 0.65 * (depth + 1) / 2)
        }
        if progress <= orbitShare { return orbit(progress / orbitShare) }
        let (from, fromOpacity) = orbit(1)
        let eased = easeInOut((progress - orbitShare) / (1 - orbitShare))
        let point = CGPoint(x: from.x + (target.x - from.x) * eased, y: from.y + (target.y - from.y) * eased)
        return (point, fromOpacity + (1 - fromOpacity) * eased)
    }

    /// `cubic-bezier(0.77, 0, 0.175, 1)`, the landing page's `--ease-in-out`.
    static func easeInOut(_ x: Double) -> Double {
        let (x1, y1, x2, y2) = (0.77, 0.0, 0.175, 1.0)
        func bezier(_ t: Double, _ a: Double, _ b: Double) -> Double {
            3 * (1 - t) * (1 - t) * t * a + 3 * (1 - t) * t * t * b + t * t * t
        }
        var t = x
        for _ in 0..<8 {
            let slope = 3 * (1 - t) * (1 - t) * x1 + 6 * (1 - t) * t * (x2 - x1) + 3 * t * t * (1 - x2)
            guard abs(slope) > 1e-6 else { break }
            t -= (bezier(t, x1, x2) - x) / slope
            t = min(max(t, 0), 1)
        }
        return bezier(t, y1, y2)
    }
}

#Preview {
    RelayMark()
        .padding(Theme.Space.xl)
}
