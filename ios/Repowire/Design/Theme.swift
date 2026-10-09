import SwiftUI
import UIKit

/// The Repowire Design System on iOS: warm-paper neutrals, one cobalt accent,
/// system type for prose and monospaced type for technical chrome. Views take
/// every color, space, radius, and font from here; `scripts/ios-design-lint.sh`
/// fails the build on raw values.
nonisolated enum Theme {
    enum Palette {
        static let page = dynamic(light: 0xFCFBFA, dark: 0x0E0E0D)
        static let surface = dynamic(light: 0xFFFFFF, dark: 0x161614)
        static let sunken = dynamic(light: 0xF7F6F3, dark: 0x0A0A09)
        static let foreground = dynamic(light: 0x141413, dark: 0xF3F1EC)
        static let muted = dynamic(light: 0x4E4B46, dark: 0xB5B1A8)
        static let faint = dynamic(light: 0x8A867E, dark: 0x7D7970)
        static let accent = dynamic(light: 0x2C54DD, dark: 0x5778E6)
        static let accentSoft = dynamic(light: 0xEEF2FF, dark: 0x1F2540)
        static let border = dynamic(light: 0xE8E5DF, dark: 0x2A2926)
        static let success = dynamic(light: 0x1F8A4C, dark: 0x3DBE74)
        static let warning = dynamic(light: 0xB86E00, dark: 0xE9A23B)
        static let danger = dynamic(light: 0xC2362B, dark: 0xF06A5E)
        /// Text and icons on a solid accent fill.
        static let onAccent = Color.white
        static let onAccentMuted = Color.white.opacity(0.75)

        private static func dynamic(light: UInt32, dark: UInt32) -> Color {
            Color(UIColor { $0.userInterfaceStyle == .dark ? UIColor(hex: dark) : UIColor(hex: light) })
        }
    }

    /// 4pt grid.
    enum Space {
        static let xxs: CGFloat = 2
        static let xs: CGFloat = 4
        static let s: CGFloat = 8
        static let m: CGFloat = 12
        static let l: CGFloat = 16
        static let xl: CGFloat = 24
        static let xxl: CGFloat = 32
    }

    enum Radius {
        static let control: CGFloat = 10
        static let card: CGFloat = 14
    }

    enum Stroke {
        static let hairline: CGFloat = 1
    }

    enum Size {
        static let statusDot: CGFloat = 8
        static let avatar: CGFloat = 36
        static let tapTarget: CGFloat = 44
        /// Thinking orb presets: inline with text, and header scale. Separate
        /// tunings, never one scaled into the other.
        static let orbCompact: CGFloat = 16
        static let orbLarge: CGFloat = 28
    }

    /// Critically damped by default; nothing here bounces without a gesture behind it.
    enum Motion {
        static let standard = Animation.spring(response: 0.35, dampingFraction: 1)
        static let quick = Animation.spring(response: 0.25, dampingFraction: 1)
    }

    enum Typeface {
        /// Technical chrome: peer names, ids, paths, timestamps, eyebrows.
        static func mono(_ style: Font.TextStyle, weight: Font.Weight = .regular) -> Font {
            .system(style, design: .monospaced, weight: weight)
        }

        static let eyebrow = Font.system(.caption2, design: .monospaced, weight: .semibold)
    }
}

nonisolated extension UIColor {
    convenience init(hex: UInt32) {
        self.init(
            red: CGFloat((hex >> 16) & 0xFF) / 255,
            green: CGFloat((hex >> 8) & 0xFF) / 255,
            blue: CGFloat(hex & 0xFF) / 255,
            alpha: 1
        )
    }
}
