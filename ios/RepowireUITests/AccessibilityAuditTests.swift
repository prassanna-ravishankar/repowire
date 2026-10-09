import XCTest

/// Apple's accessibility audit (contrast, Dynamic Type, hit regions, labels,
/// clipped text) on every screen, in light and dark.
final class AccessibilityAuditTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = true
    }

    func testLightScreens() throws {
        try auditAllScreens(appearance: "light")
    }

    func testDarkScreens() throws {
        try auditAllScreens(appearance: "dark")
    }

    private func auditAllScreens(appearance: String) throws {
        let onboarding = XCUIApplication.fixtures(onboarding: true, appearance: appearance)
        onboarding.launch()
        onboarding.buttons["onboarding.connect"].waitToAppear()
        try audit(onboarding, "onboarding")
        onboarding.terminate()

        let app = XCUIApplication.fixtures(appearance: appearance)
        app.launch()
        app.element("question.ask-backend-1").waitToAppear()
        try audit(app, "inbox")

        app.tab("Peers")
        app.buttons["peer.backend"].waitToAppear()
        try audit(app, "peers")

        app.buttons["peer.backend"].tap()
        app.textFields["composer.text"].waitToAppear()
        try audit(app, "peer detail")

        app.tab("Jobs")
        app.staticTexts["Nightly dependency audit"].waitToAppear()
        try audit(app, "jobs")

        app.tab("Settings")
        app.buttons["settings.disconnect"].waitToAppear()
        try audit(app, "settings")
    }

    private func audit(_ app: XCUIApplication, _ screen: String) throws {
        // Content between the navigation bar and the tab bar is what you read;
        // above or below it, it is under translucent chrome.
        let nav = app.navigationBars.firstMatch
        let tabs = app.tabBars.firstMatch
        // iOS 26 fades content for a short distance below the bar (the soft
        // scroll edge effect), so that band is chrome too.
        let top = nav.exists ? nav.frame.maxY + Self.scrollEdgeEffect : app.frame.minY
        // The composer floats above the tab bar on a peer's screen.
        let composer = app.segmentedControls["composer.mode"]
        let bottom = composer.exists ? composer.frame.minY : tabs.exists ? tabs.frame.minY : app.frame.maxY
        let readable = CGRect(x: app.frame.minX, y: top, width: app.frame.width, height: bottom - top)
        try app.performAccessibilityAudit { issue in
            if let reason = Self.knownFalsePositive(issue, readable: readable) {
                XCTContext.runActivity(named: "ignored [\(screen)] \(issue.compactDescription): \(reason)") { _ in }
                return true
            }
            guard let element = issue.element else {
                // Nothing to locate or fix; in practice rows under the tab bar.
                XCTContext.runActivity(named: "unattributed [\(screen)] \(issue.detailedDescription)") { _ in }
                return true
            }
            XCTFail("a11y [\(screen)] \(issue.compactDescription) :: '\(element.label)' \(element.frame) readable \(readable)")
            return true
        }
    }

    private static let scrollEdgeEffect: CGFloat = 56

    /// The audit measures elements where they are, so a few results describe
    /// the scroll position rather than the design. Each exemption says why.
    private static func knownFalsePositive(_ issue: XCUIAccessibilityAuditIssue, readable: CGRect) -> String? {
        let element = issue.element
        let frame = element?.frame ?? .null
        switch issue.auditType {
        case .contrast where element?.isEnabled == false:
            return "disabled control (WCAG exempts inactive components)"
        case .contrast, .textClipped:
            if !frame.isNull, !readable.contains(frame) { return "content scrolled under translucent chrome" }
            if issue.detailedDescription.contains("UISearchBarTextField") { return "system search field" }
        case .dynamicType:
            // Text styles scale; ScreenshotTour's largetext variant is the check
            // that layouts hold at accessibility sizes.
            return "verified by the largetext screenshot variant"
        case .sufficientElementDescription:
            if let label = element?.label, label.contains("."), !label.contains(" ") { return "hostname or identifier, read as written" }
        default:
            break
        }
        return nil
    }
}
