import Foundation
import Observation
import OSLog
import RepowireKit

/// App state. Owns the mesh service, mirrors the daemon's event log, and derives
/// everything the screens show from it, the same way the web dashboard does.
@Observable
final class AppModel {
    enum Phase: Equatable {
        case onboarding
        case connected(host: String)
    }

    enum Connection: Equatable {
        case connecting
        case live
        case reconnecting(retryIn: Int)
        case failed(String)
    }

    private(set) var phase: Phase = .onboarding
    private(set) var connection: Connection = .connecting
    private(set) var peers: [Peer] = []
    private(set) var events: [MeshEvent] = []
    private(set) var jobs: [Job] = []
    private(set) var answering: Set<String> = []
    var lastError: String?

    private var service: (any MeshService)?
    private var streamTask: Task<Void, Never>?
    private var pushToken: String?
    private let store: CredentialStore
    private let fixtures: Bool
    private let log = Logger(subsystem: "io.repowire.app", category: "mesh")
    private static let eventWindow = 500

    /// `fixtures` serves `FixtureMesh` instead of a relay (previews, UI tests,
    /// design screenshots); `onboarding` starts at the connect screen.
    init(store: CredentialStore = CredentialStore(), fixtures: Bool = false, onboarding: Bool = false) {
        self.store = store
        self.fixtures = fixtures
        if onboarding { return }
        #if DEBUG
        // `-relayURL <url> -relayKey <key>`: live end-to-end runs without the Keychain.
        let defaults = UserDefaults.standard
        if let raw = defaults.string(forKey: "relayURL"), let url = URL(string: raw), let key = defaults.string(forKey: "relayKey") {
            attach(RelayAPI(baseURL: url, key: key), host: url.host() ?? "relay")
            return
        }
        #endif
        if fixtures {
            attach(FixtureMesh(), host: "repowire.io")
        } else if let saved = store.load() {
            attach(RelayAPI(baseURL: saved.relayURL, key: saved.key), host: saved.relayURL.host() ?? "relay")
        }
    }

    // MARK: Derived state

    var pendingQuestions: [PendingQuestion] {
        PendingQuestion.derive(from: events).reversed()
    }

    /// Messages addressed to the human that are not open questions.
    var inbox: [MeshEvent] {
        let open = Set(pendingQuestions.map(\.correlationId))
        return events.reversed().filter { event in
            guard ["notification", "query", "ask"].contains(event.type), Self.isHuman(event.to) else { return false }
            return event.correlationId.map { !open.contains($0) } ?? true
        }
    }

    var listedPeers: [Peer] {
        peers.filter(\.isListed).sorted { ($0.status.rank, $0.label) < ($1.status.rank, $1.label) }
    }

    func peer(named name: String) -> Peer? {
        peers.first { $0.name == name || $0.label == name }
    }

    static func isHuman(_ address: String?) -> Bool {
        guard let address else { return false }
        return ["human", "dashboard"].contains(address.lowercased().trimmingCharacters(in: CharacterSet(charactersIn: "@")))
    }

    // MARK: Connection

    /// Validates credentials against the relay before keeping them.
    func connect(relay: URL, key: String) async throws {
        if fixtures {
            try await Task.sleep(for: .milliseconds(300))
            guard key.hasSuffix("demo") else { throw MeshError.unauthorized }
            attach(FixtureMesh(), host: relay.host() ?? "relay")
            return
        }
        let key = key.trimmingCharacters(in: .whitespacesAndNewlines)
        let api = RelayAPI(baseURL: relay, key: key)
        try await api.validate()
        store.save(Credentials(relayURL: relay, key: key))
        attach(api, host: relay.host() ?? "relay")
    }

    func signOut() {
        streamTask?.cancel()
        if !fixtures { store.clear() }
        service = nil
        peers = []
        events = []
        jobs = []
        phase = .onboarding
    }

    private func attach(_ service: any MeshService, host: String) {
        self.service = service
        phase = .connected(host: host)
        streamTask?.cancel()
        streamTask = Task { await run(service) }
        if let pushToken { Task { await register(token: pushToken) } }
    }

    /// Initial load, then the live stream. On a drop it backs off and recovers
    /// the gap with `/events?since=`, mirroring the dashboard's `useEventStream`.
    private func run(_ service: any MeshService) async {
        var backoff: Double = 1
        connection = .connecting
        await refresh()
        while !Task.isCancelled {
            do {
                if let last = events.last?.id {
                    merge(try await service.events(since: last))
                }
                connection = .live
                backoff = 1
                for try await event in service.eventStream() {
                    merge([event])
                }
                connection = .reconnecting(retryIn: Int(backoff))
            } catch is CancellationError {
                return
            } catch {
                log.error("stream dropped: \(error.localizedDescription, privacy: .public)")
                if case MeshError.unauthorized = error {
                    connection = .failed(error.localizedDescription)
                    return
                }
                connection = .reconnecting(retryIn: Int(backoff))
            }
            try? await Task.sleep(for: .seconds(backoff))
            backoff = min(backoff * 2, 30)
        }
    }

    func refresh() async {
        guard let service else { return }
        do {
            async let peers = service.peers()
            async let events = service.events(since: nil)
            async let jobs = service.jobs()
            self.peers = try await peers
            self.events = Array(try await events.suffix(Self.eventWindow))
            self.jobs = (try? await jobs) ?? []
            lastError = nil
        } catch {
            lastError = error.localizedDescription
            if case MeshError.unauthorized = error { connection = .failed(error.localizedDescription) }
        }
    }

    private func merge(_ incoming: [MeshEvent]) {
        let known = Set(events.map(\.id))
        let fresh = incoming.filter { !known.contains($0.id) }
        guard !fresh.isEmpty else { return }
        events.append(contentsOf: fresh)
        if events.count > Self.eventWindow { events.removeFirst(events.count - Self.eventWindow) }
        if fresh.contains(where: \.isLifecycle) { Task { await reloadPeers() } }
        if fresh.contains(where: { $0.type == "job_state_changed" }) { Task { await reloadJobs() } }
    }

    private func reloadPeers() async {
        if let peers = try? await service?.peers() { self.peers = peers }
    }

    func reloadJobs() async {
        if let jobs = try? await service?.jobs() { self.jobs = jobs }
    }

    // MARK: Actions

    func answer(_ question: PendingQuestion, optionId: String? = nil, outcome: String? = nil, text: String? = nil) async {
        guard let service else { return }
        answering.insert(question.correlationId)
        defer { answering.remove(question.correlationId) }
        do {
            try await service.answer(correlationId: question.correlationId, optionId: optionId, outcome: outcome, text: text)
            // Optimistically close it; the ack event from the stream confirms.
            merge([MeshEvent(id: "local-ack-\(question.correlationId)", type: "ack", timestamp: ISO8601DateFormatter().string(from: .now), correlationId: question.correlationId)])
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Answers by correlation id, for notification actions that arrive before
    /// the event log has loaded.
    func answer(correlationId: String, optionId: String?, outcome: String?, text: String?) async {
        let question = pendingQuestions.first { $0.correlationId == correlationId }
            ?? PendingQuestion(correlationId: correlationId, from: "", text: "", question: AskQuestion(kind: "choice"))
        await answer(question, optionId: optionId, outcome: outcome, text: text)
    }

    func send(_ text: String, to peer: Peer, mode: SendMode) async throws {
        guard let service else { return }
        _ = try await service.send(text, to: peer.peerId, mode: mode)
    }

    func transcript(for peer: Peer) async throws -> [TranscriptTurn] {
        try await service?.transcript(peer: peer.name, limit: 50).turns ?? []
    }

    func register(token: String) async {
        pushToken = token
        guard let service else { return }
        #if DEBUG
        let environment = "sandbox"
        #else
        let environment = "production"
        #endif
        do {
            try await service.registerDevice(token: token, environment: environment, name: DeviceName.current)
        } catch {
            log.error("push registration failed: \(error.localizedDescription, privacy: .public)")
        }
    }
}

private extension Peer.Status {
    var rank: Int {
        switch self {
        case .busy: 0
        case .online: 1
        case .offline: 2
        }
    }
}
