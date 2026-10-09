import Foundation
import Testing

@testable import RepowireKit

/// Serves canned responses and records requests, keyed per test via a header.
final class StubProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handlers: [String: (URLRequest) -> (Int, Data)] = [:]
    nonisolated(unsafe) static var seen: [String: [URLRequest]] = [:]
    static let lock = NSLock()

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let id = request.url?.host() ?? ""
        var captured = request
        if let stream = request.httpBodyStream {
            stream.open()
            var body = Data()
            let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
            while stream.hasBytesAvailable {
                let n = stream.read(buffer, maxLength: 4096)
                if n <= 0 { break }
                body.append(buffer, count: n)
            }
            buffer.deallocate()
            stream.close()
            captured.httpBody = body
        }
        Self.lock.lock()
        Self.seen[id, default: []].append(captured)
        let handler = Self.handlers[id]
        Self.lock.unlock()
        let (status, data) = handler?(captured) ?? (404, Data())
        client?.urlProtocol(
            self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: nil)!,
            cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    static func api(_ host: String, _ handler: @escaping (URLRequest) -> (Int, Data)) -> RelayAPI {
        lock.lock()
        handlers[host] = handler
        lock.unlock()
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubProtocol.self]
        return RelayAPI(baseURL: URL(string: "https://\(host)")!, key: "rw_test_key_123", session: URLSession(configuration: config))
    }

    static func requests(_ host: String) -> [URLRequest] {
        lock.lock()
        defer { lock.unlock() }
        return seen[host] ?? []
    }
}

private func json(_ text: String) -> Data { Data(text.utf8) }

@Suite struct RelayAPITests {
    @Test func `validates the key against the relay's daemon list`() async throws {
        let api = StubProtocol.api("auth.test") { _ in (200, json(#"[{"daemon_id":"studio"}]"#)) }
        try await api.validate()
        let request = try #require(StubProtocol.requests("auth.test").first)
        #expect(request.value(forHTTPHeaderField: "X-API-Key") == "rw_test_key_123")
        #expect(request.value(forHTTPHeaderField: "Cookie") == "rw_token=rw_test_key_123")
        #expect(request.url?.path() == "/api/v1/daemons")
        await #expect(throws: MeshError.noDaemon) {
            try await StubProtocol.api("nodaemon.test") { _ in (200, json("[]")) }.validate()
        }
    }

    @Test(arguments: [
        #"[{"peer_id":"p1","name":"backend","status":"online"}]"#,
        #"{"peers":[{"peer_id":"p1","name":"backend","status":"online"}]}"#,
    ])
    func `decodes peers bare or wrapped`(body: String) async throws {
        let host = "peers\(body.count).test"
        let peers = try await StubProtocol.api(host) { _ in (200, json(body)) }.peers()
        #expect(peers.map(\.name) == ["backend"])
    }

    @Test func `skips malformed events instead of failing the feed`() async throws {
        let api = StubProtocol.api("events.test") { _ in
            (
                200,
                json(
                    #"[{"id":"1","type":"ask","timestamp":"t","correlation_id":"c1"},{"bogus":true},{"id":"2","type":"ack","timestamp":"t","correlation_id":"c1"}]"#
                )
            )
        }
        let events = try await api.events(since: "0")
        #expect(events.map(\.id) == ["1", "2"])
        #expect(events.first?.correlationId == "c1")
        #expect(StubProtocol.requests("events.test").first?.url?.query() == "since=0")
    }

    @Test func `posts ask as the dashboard surface and returns the correlation id`() async throws {
        let api = StubProtocol.api("send.test") { _ in (200, json(#"{"correlation_id":"ask-9"}"#)) }
        let cid = try await api.send("ship it", to: "p1", mode: .ask)
        #expect(cid == "ask-9")
        let request = try #require(StubProtocol.requests("send.test").first)
        let body = try #require(try JSONSerialization.jsonObject(with: request.httpBody ?? Data()) as? [String: Any])
        #expect(request.httpMethod == "POST")
        #expect(request.url?.path() == "/ask")
        #expect(body["from_peer"] as? String == "dashboard")
        #expect(body["to_peer"] as? String == "p1")
    }

    @Test func `maps relay failures to readable errors`() async throws {
        await #expect(throws: MeshError.unauthorized) {
            try await StubProtocol.api("e401.test") { _ in (401, json(#"{"detail":"Invalid API key"}"#)) }.peers()
        }
        await #expect(throws: MeshError.noDaemon) {
            try await StubProtocol.api("e502.test") { _ in (502, json(#"{"detail":"No daemon connected"}"#)) }.peers()
        }
        await #expect(throws: MeshError.server(status: 500, message: "boom")) {
            try await StubProtocol.api("e500.test") { _ in (500, json(#"{"detail":"boom"}"#)) }.jobs()
        }
    }

    @Test func `registers push devices with the daemon`() async throws {
        let api = StubProtocol.api("push.test") { _ in (200, json(#"{"ok":true}"#)) }
        try await api.registerDevice(token: "abc", environment: "sandbox", name: "phone")
        let request = try #require(StubProtocol.requests("push.test").first)
        #expect(request.url?.path() == "/push/devices")
    }

    @Test(arguments: [("data: {\"id\":\"1\"}", "{\"id\":\"1\"}"), (": keepalive", nil), ("data:", nil)] as [(String, String?)])
    func `extracts sse data payloads`(line: String, expected: String?) {
        #expect(ServerSentEvents.payload(line) == expected)
    }
}

@Suite struct ModelTests {
    @Test func `derives open questions until acked`() {
        let question = AskQuestion(
            kind: "choice", prompt: "ok?", options: [QuestionOption(id: "y", title: "Yes")], blocking: true, scope: "tool_permission")
        let events = [
            MeshEvent(id: "1", type: "ask", from: "a", text: "first", correlationId: "c1", question: question),
            MeshEvent(id: "2", type: "ask", from: "b", text: "second", correlationId: "c2", question: question),
            MeshEvent(id: "3", type: "ask", from: "c", text: "no question", correlationId: "c3"),
            MeshEvent(id: "4", type: "ack", correlationId: "c1"),
        ]
        let pending = PendingQuestion.derive(from: events)
        #expect(pending.map(\.correlationId) == ["c2"])
        #expect(pending.first?.question.isApproval == true)
    }

    @Test(arguments: [
        ("repowire.io", "https://repowire.io"),
        ("https://relay.example.com/", "https://relay.example.com"),
        ("http://localhost:8080", "http://localhost:8080"),
    ])
    func `normalizes relay urls`(raw: String, expected: String) {
        #expect(Credentials.normalizedRelayURL(raw)?.absoluteString == expected)
    }

    @Test func `rejects implausible relay keys`() {
        #expect(Credentials.isPlausibleKey("rw_abcdefgh123"))
        #expect(!Credentials.isPlausibleKey("abc"))
        #expect(!Credentials.isPlausibleKey("rw_ has space"))
    }
}

@Suite struct FixtureMeshTests {
    @Test func `answering closes the question and streams the ack`() async throws {
        let mesh = FixtureMesh()
        let stream = mesh.eventStream()
        try await Task.sleep(for: .milliseconds(20))
        let before = PendingQuestion.derive(from: try await mesh.events(since: nil))
        #expect(before.count == 2)
        try await mesh.answer(correlationId: "ask-backend-1", optionId: "allow", outcome: nil, text: nil)
        var iterator = stream.makeAsyncIterator()
        let streamed = try await iterator.next()
        #expect(streamed?.type == "ack")
        let after = PendingQuestion.derive(from: try await mesh.events(since: nil))
        #expect(after.map(\.correlationId) == ["ask-docs-1"])
    }
}
