import XCTest

extension XCUIApplication {
    /// The app on `FixtureMesh`: deterministic data, no network.
    static func fixtures(onboarding: Bool = false, appearance: String = "light", textSize: String? = nil) -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-fixtures", "-appearance", appearance]
        if onboarding { app.launchArguments.append("-onboarding") }
        if let textSize { app.launchArguments += ["-UIPreferredContentSizeCategoryName", textSize] }
        return app
    }

    func element(_ identifier: String) -> XCUIElement {
        descendants(matching: .any)[identifier]
    }

    func tab(_ name: String) {
        tabBars.buttons[name].tap()
    }
}

extension XCUIElement {
    @discardableResult
    func waitToAppear(_ timeout: TimeInterval = 5, file: StaticString = #filePath, line: UInt = #line) -> XCUIElement {
        XCTAssertTrue(waitForExistence(timeout: timeout), "\(self) did not appear", file: file, line: line)
        return self
    }

    func waitToDisappear(_ timeout: TimeInterval = 5, file: StaticString = #filePath, line: UInt = #line) {
        let gone = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: self)
        XCTAssertEqual(XCTWaiter().wait(for: [gone], timeout: timeout), .completed, "\(self) did not disappear", file: file, line: line)
    }
}
