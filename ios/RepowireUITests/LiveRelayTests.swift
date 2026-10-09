import XCTest

/// End to end through a real relay and daemon: a question raised on the daemon
/// reaches the app over the relay's SSE tunnel, and tapping Allow in the app
/// resolves it on the daemon. Run by `scripts/live-relay-e2e.sh`, which starts
/// an isolated relay and daemon; skipped otherwise.
final class LiveRelayTests: XCTestCase {
    private var relay: URL!
    private var key: String!

    override func setUpWithError() throws {
        let env = ProcessInfo.processInfo.environment
        guard let raw = env["REPOWIRE_RELAY_URL"], let url = URL(string: raw), let key = env["REPOWIRE_RELAY_KEY"] else {
            throw XCTSkip("Set by scripts/live-relay-e2e.sh")
        }
        relay = url
        self.key = key
        continueAfterFailure = false
    }

    @MainActor
    func testQuestionRoundTripsThroughTheRelay() async throws {
        let app = launchLive()
        print("live-e2e: launched")
        app.tab("Peers")
        let peer = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH 'peer.'")).firstMatch
        XCTAssertTrue(peer.waitForExistence(timeout: 10), "The daemon's peer should list through the relay")
        print("live-e2e: peer listed")
        app.tab("Inbox")

        let cid = "ask-e2e-\(Int.random(in: 1000...9999))"
        let (relay, key) = (relay!, key!)
        let outcome = Task.detached { try await Self.askBlocking(cid, relay: relay, key: key) }
        let card = app.element("question.\(cid)")
        XCTAssertTrue(card.waitForExistence(timeout: 15), "The question should stream to the app over SSE")
        print("live-e2e: card shown")
        card.buttons["question.allow"].tap()
        print("live-e2e: tapped allow")

        let chosen = try await outcome.value
        XCTAssertEqual(chosen, "allow", "The daemon should receive the app's answer")
    }

    @MainActor
    private func launchLive() -> XCUIApplication {
        let app = XCUIApplication()
        app.launchArguments = ["-relayURL", relay.absoluteString, "-relayKey", key, "-appearance", "light"]
        app.launch()
        return app
    }

    /// The daemon's blocking human question, exactly what a tool-permission hook raises.
    /// Returns the option id the daemon resolved the question with.
    private nonisolated static func askBlocking(_ cid: String, relay: URL, key: String) async throws -> String? {
        var request = URLRequest(url: relay.appending(path: "questions/ask-blocking"))
        request.httpMethod = "POST"
        request.timeoutInterval = 60
        request.setValue("Bearer \(key)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: [
            "prompt": "Bash: rm -rf build/ (live relay e2e)", "scope": "tool_permission", "from_peer": "e2e-claude-code",
            "correlation_id": cid, "timeout_seconds": 45,
            "options": [["id": "allow", "title": "Allow"]],
        ])
        let (data, _) = try await URLSession.shared.data(for: request)
        let object = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        return object?["option_id"] as? String
    }
}
