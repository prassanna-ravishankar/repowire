import Foundation

/// Talks to a daemon through the relay tunnel, authenticated with the relay key.
public struct RelayAPI: MeshService {
    /// The human surface the daemon already frames as direct user instructions.
    public static let surfaceName = "dashboard"

    public let baseURL: URL
    let key: String
    let session: URLSession

    public init(baseURL: URL, key: String, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.key = key
        self.session = session
    }

    static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return decoder
    }()

    func request(_ path: String, method: String = "GET", query: [URLQueryItem] = [], body: [String: Any]? = nil) throws -> URLRequest {
        guard var components = URLComponents(url: baseURL.appending(path: path), resolvingAgainstBaseURL: false) else {
            throw MeshError.invalidURL
        }
        if !query.isEmpty { components.queryItems = query }
        guard let url = components.url else { throw MeshError.invalidURL }
        var request = URLRequest(url: url)
        request.httpMethod = method
        // X-API-Key covers the relay's API routes; the rw_token cookie covers
        // tunnel routes on relays that predate header auth there. Both are
        // stripped before the request reaches the daemon on current relays.
        request.setValue(key, forHTTPHeaderField: "X-API-Key")
        request.setValue("rw_token=\(key)", forHTTPHeaderField: "Cookie")
        request.httpShouldHandleCookies = false
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        return request
    }

    func data(for request: URLRequest) async throws -> Data {
        let (data, response) = try await session.data(for: request)
        try Self.check(response, data: data)
        return data
    }

    static func check(_ response: URLResponse, data: Data) throws {
        guard let http = response as? HTTPURLResponse else { return }
        switch http.statusCode {
        case 200..<300: return
        case 401, 403: throw MeshError.unauthorized
        case 502 where String(decoding: data, as: UTF8.self).contains("No daemon"): throw MeshError.noDaemon
        default:
            let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            let message = (object?["detail"] ?? object?["error"]) as? String ?? ""
            throw MeshError.server(status: http.statusCode, message: message)
        }
    }

    /// The relay answers `/health` itself, so key and daemon checks use its
    /// own daemon listing: 401 for a bad key, an empty list for no daemon.
    public func validate() async throws {
        let data = try await data(for: request("api/v1/daemons"))
        let daemons = try JSONSerialization.jsonObject(with: data) as? [Any] ?? []
        if daemons.isEmpty { throw MeshError.noDaemon }
    }

    public func peers() async throws -> [Peer] {
        let data = try await data(for: request("peers"))
        if let wrapped = try? Self.decoder.decode(PeersEnvelope.self, from: data) {
            return wrapped.peers.elements
        }
        return try Self.decoder.decode(Lossy<Peer>.self, from: data).elements
    }

    public func events(since: String?) async throws -> [MeshEvent] {
        let query = since.map { [URLQueryItem(name: "since", value: $0)] } ?? []
        return try Self.decoder.decode(Lossy<MeshEvent>.self, from: await data(for: request("events", query: query))).elements
    }

    public func eventStream() -> AsyncThrowingStream<MeshEvent, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    var request = try request("events/stream")
                    request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    request.timeoutInterval = .infinity
                    let (bytes, response) = try await session.bytes(for: request)
                    try Self.check(response, data: Data())
                    for try await line in bytes.lines {
                        guard let payload = ServerSentEvents.payload(line),
                            let event = try? Self.decoder.decode(MeshEvent.self, from: Data(payload.utf8))
                        else { continue }
                        continuation.yield(event)
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    public func transcript(peer: String, limit: Int) async throws -> TranscriptPage {
        let data = try await data(for: request("peers/\(peer)/transcript", query: [URLQueryItem(name: "limit", value: String(limit))]))
        return try Self.decoder.decode(TranscriptPage.self, from: data)
    }

    public func send(_ text: String, to peerId: String, mode: SendMode) async throws -> String? {
        let body: [String: Any] = ["from_peer": Self.surfaceName, "to_peer": peerId, "text": text, "bypass_circle": true]
        let data = try await data(for: request(mode.rawValue, method: "POST", body: body))
        let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        if let error = object?["error"] as? String { throw MeshError.server(status: 200, message: error) }
        return object?["correlation_id"] as? String
    }

    public func answer(correlationId: String, optionId: String?, outcome: String?, text: String?) async throws {
        var body: [String: Any] = ["correlation_id": correlationId]
        if let optionId { body["option_id"] = optionId }
        if let outcome { body["outcome"] = outcome }
        if let text { body["text"] = text }
        _ = try await data(for: request("answer", method: "POST", body: body))
    }

    public func jobs() async throws -> [Job] {
        try Self.decoder.decode(JobsPage.self, from: await data(for: request("jobs"))).work
    }

    public func registerDevice(token: String, environment: String, name: String) async throws {
        _ = try await data(for: request("push/devices", method: "POST", body: ["token": token, "environment": environment, "name": name]))
    }

    private struct PeersEnvelope: Decodable {
        var peers: Lossy<Peer>
    }
}

/// `text/event-stream` framing: the daemon writes one JSON object per `data:` line.
enum ServerSentEvents {
    static func payload(_ line: String) -> String? {
        guard line.hasPrefix("data:") else { return nil }
        let payload = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
        return payload.isEmpty ? nil : payload
    }
}
