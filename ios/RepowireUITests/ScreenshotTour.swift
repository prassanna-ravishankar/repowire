import XCTest

/// Captures every screen in each design variant for the design review sheet
/// (`scripts/ios-check.sh` exports these attachments). Names sort by variant,
/// then by screen, so the sheet lines variants up side by side.
final class ScreenshotTour: XCTestCase {
    private static let variants: [(name: String, appearance: String, textSize: String?)] = [
        ("light", "light", nil),
        ("dark", "dark", nil),
        ("largetext", "light", "UICTContentSizeCategoryAccessibilityL"),
    ]

    func testTour() {
        for variant in Self.variants {
            let onboarding = XCUIApplication.fixtures(onboarding: true, appearance: variant.appearance, textSize: variant.textSize)
            onboarding.launch()
            onboarding.buttons["onboarding.connect"].waitToAppear()
            capture(onboarding, variant.name, "1-onboarding")
            onboarding.terminate()

            let app = XCUIApplication.fixtures(appearance: variant.appearance, textSize: variant.textSize)
            app.launch()
            app.element("question.ask-backend-1").waitToAppear()
            capture(app, variant.name, "2-inbox")

            app.tab("Peers")
            app.buttons["peer.backend"].waitToAppear()
            capture(app, variant.name, "3-peers")

            app.buttons["peer.backend"].tap()
            app.textFields["composer.text"].waitToAppear()
            capture(app, variant.name, "4-peer-detail")

            app.tab("Jobs")
            app.staticTexts["Nightly dependency audit"].waitToAppear()
            capture(app, variant.name, "5-jobs")

            app.tab("Settings")
            app.buttons["settings.disconnect"].waitToAppear()
            capture(app, variant.name, "6-settings")
            app.terminate()
        }
    }

    private func capture(_ app: XCUIApplication, _ variant: String, _ screen: String) {
        let attachment = XCTAttachment(screenshot: app.screenshot())
        attachment.name = "\(variant)--\(screen)"
        attachment.lifetime = .keepAlways
        add(attachment)
    }
}
