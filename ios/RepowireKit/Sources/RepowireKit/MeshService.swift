import Foundation

/// Everything the app needs from a Repowire mesh. `RelayAPI` talks to a real
/// daemon through the relay; `FixtureMesh` serves deterministic data for
/// previews, UI tests, and design screenshots.
public protocol MeshService: Sendable {
    /// Proves the key is accepted and a daemon is connected behind it.
    func validate() async throws
    func peers() async throws -> [Peer]
    func events(since: String?) async throws -> [MeshEvent]
    /// Live events. Ends when the connection drops; callers reconnect.
    func eventStream() -> AsyncThrowingStream<MeshEvent, Error>
    func transcript(peer: String, limit: Int) async throws -> TranscriptPage
    func send(_ text: String, to peerId: String, mode: SendMode) async throws -> String?
    func answer(correlationId: String, optionId: String?, outcome: String?, text: String?) async throws
    func jobs() async throws -> [Job]
    func registerDevice(token: String, environment: String, name: String) async throws
}

public enum MeshError: LocalizedError, Equatable {
    case unauthorized
    case noDaemon
    case server(status: Int, message: String)
    case invalidURL

    public var errorDescription: String? {
        switch self {
        case .unauthorized: "The relay key was not accepted."
        case .noDaemon: "No daemon is connected to the relay for this key."
        case let .server(status, message): message.isEmpty ? "Request failed (\(status))." : message
        case .invalidURL: "The relay URL is not valid."
        }
    }
}
