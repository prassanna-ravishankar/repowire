import XCTest

/// Functional coverage of the human's core jobs, run against FixtureMesh.
final class FlowTests: XCTestCase {
    override func setUp() {
        continueAfterFailure = false
    }

    func testOnboardingRejectsABadKeyThenConnects() {
        let app = XCUIApplication.fixtures(onboarding: true)
        app.launch()

        let connect = app.buttons["onboarding.connect"].waitToAppear()
        XCTAssertFalse(connect.isEnabled, "Connect must wait for a plausible key")

        let key = app.secureTextFields["onboarding.key"]
        key.tap()
        key.typeText("rw_wrong_key")
        XCTAssertTrue(connect.isEnabled)
        connect.tap()
        app.element("onboarding.error").waitToAppear()

        key.tap()
        key.typeText(XCUIKeyboardKey.delete.rawValue.repeated(12) + "rw_test_demo")
        connect.tap()
        app.navigationBars["Inbox"].waitToAppear()
    }

    func testApprovingAToolCallClearsIt() {
        let app = XCUIApplication.fixtures()
        app.launch()

        let card = app.element("question.ask-backend-1").waitToAppear()
        XCTAssertEqual(app.tabBars.buttons["Inbox"].value as? String, "2 items")
        card.buttons["question.allow"].tap()
        card.waitToDisappear()
        XCTAssertTrue(app.element("question.ask-docs-1").exists, "Only the answered question closes")
    }

    func testDenyingAToolCallClearsIt() {
        let app = XCUIApplication.fixtures()
        app.launch()

        let card = app.element("question.ask-backend-1").waitToAppear()
        card.buttons["question.deny"].tap()
        card.waitToDisappear()
    }

    func testChoosingAnOptionAnswersTheQuestion() {
        let app = XCUIApplication.fixtures()
        app.launch()

        let card = app.element("question.ask-docs-1").waitToAppear()
        card.buttons["question.option.hosted"].tap()
        card.waitToDisappear()
    }

    func testMessagingAPeerShowsInItsConversation() {
        let app = XCUIApplication.fixtures()
        app.launch()
        app.tab("Peers")
        app.buttons["peer.web"].waitToAppear().tap()

        let field = app.textFields["composer.text"].waitToAppear()
        XCTAssertFalse(app.buttons["composer.send"].isEnabled, "Send waits for text")
        field.tap()
        field.typeText("Ship the empty states")
        app.buttons["composer.send"].tap()
        app.staticTexts["Ship the empty states"].waitToAppear()
        XCTAssertEqual(field.value as? String, "Message @web", "Composer clears after a successful send")
    }

    func testOfflinePeerCannotBeMessaged() {
        let app = XCUIApplication.fixtures()
        app.launch()
        app.tab("Peers")
        app.swipeUp()
        app.buttons["peer.reviewer"].waitToAppear().tap()
        XCTAssertFalse(app.buttons["composer.send"].waitToAppear().isEnabled)
    }

    func testCollapsingACircleHidesItsPeers() {
        let app = XCUIApplication.fixtures()
        app.launch()
        app.tab("Peers")
        let backend = app.buttons["peer.backend"].waitToAppear()
        let circle = app.buttons["circle.repowire"]
        XCTAssertEqual(circle.value as? String, "Expanded")

        circle.tap()
        backend.waitToDisappear()
        XCTAssertEqual(circle.value as? String, "Collapsed")
        XCTAssertTrue(app.buttons["peer.infra"].exists, "Other circles stay open")

        circle.tap()
        backend.waitToAppear()
    }

    func testJobsAreGroupedByState() {
        let app = XCUIApplication.fixtures()
        app.launch()
        app.tab("Jobs")
        app.staticTexts["Nightly dependency audit"].waitToAppear()
        XCTAssertTrue(app.staticTexts["Flaky test triage"].exists)
    }

    func testDisconnectReturnsToOnboarding() {
        let app = XCUIApplication.fixtures()
        app.launch()
        app.tab("Settings")
        app.buttons["settings.disconnect"].waitToAppear().tap()
        app.sheets.buttons["Disconnect"].firstMatch.waitToAppear().tap()
        app.buttons["onboarding.connect"].waitToAppear()
    }
}

private extension String {
    func repeated(_ count: Int) -> String { String(repeating: self, count: count) }
}
