import Foundation

/// A deterministic in-memory mesh for previews, UI tests, and design review.
/// Answering a question or sending a message mutates it like a real daemon would.
public actor FixtureMesh: MeshService {
    private var peerList: [Peer]
    private var log: [MeshEvent]
    private var transcripts: [String: [TranscriptTurn]]
    private var jobList: [Job]
    private var continuations: [UUID: AsyncThrowingStream<MeshEvent, Error>.Continuation] = [:]
    private var sequence = 0
    public private(set) var registeredDevices: [String] = []

    public init(now: Date = .now) {
        let stamp = { (minutesAgo: Double) in FixtureMesh.iso(now.addingTimeInterval(-minutesAgo * 60)) }
        peerList = [
            FixtureMesh.peer("p-backend", "backend", .busy, "repowire", "Migrating the session store to SQLite", backend: "claude-code", model: "opus", turn: "working"),
            FixtureMesh.peer("p-web", "web", .online, "repowire", "Dashboard empty states", backend: "codex", model: "gpt-5", turn: "idle"),
            FixtureMesh.peer("p-docs", "docs", .online, "repowire", "Relay push setup guide", backend: "claude-code", model: "sonnet", turn: "awaiting_input"),
            FixtureMesh.peer("p-infra", "infra", .online, "platform", "Terraform for the relay deploy", backend: "opencode", model: nil, turn: "idle"),
            FixtureMesh.peer("p-review", "reviewer", .offline, "repowire", nil, backend: "claude-code", model: "opus", turn: nil),
        ]
        log = [
            MeshEvent(id: "e1", type: "notification", timestamp: stamp(42), from: "infra", to: "dashboard", text: "Relay staging is up. Health check passes on all three regions."),
            MeshEvent(id: "e2", type: "ask", timestamp: stamp(9), from: "docs", to: "human", text: "Which relay host should the push guide use in examples?", correlationId: "ask-docs-1",
                      question: AskQuestion(kind: "choice", prompt: "Which relay host should the push guide use in examples?", options: [
                          QuestionOption(id: "hosted", title: "repowire.io"),
                          QuestionOption(id: "self", title: "relay.example.com"),
                      ], blocking: false, scope: "mesh_ask")),
            MeshEvent(id: "e3", type: "ask", timestamp: stamp(2), from: "backend", to: "human", text: "Run `go test -race ./...` and delete the stale state.db fixtures?", correlationId: "ask-backend-1",
                      question: AskQuestion(kind: "choice", prompt: "Bash: go test -race ./... && rm testdata/*.db", options: [
                          QuestionOption(id: "allow", title: "Allow"),
                          QuestionOption(id: "allow_always", title: "Always allow"),
                      ], blocking: true, scope: "tool_permission")),
        ]
        transcripts = [
            "backend": [
                FixtureMesh.turn("backend", 1, "user", "Move session bindings off the JSON file and into the SQLite store. Keep the legacy import.", stamp(30)),
                FixtureMesh.turn("backend", 2, "assistant", "Plan:\n1. Add a `session_bindings` table behind schema version 15.\n2. Import the JSON file once, then mark it imported.\n3. Route reads through the store.\n\nStarting with the migration.", stamp(29), tools: [.init(name: "Read", input: "state/migrate.go")]),
                FixtureMesh.turn("backend", 3, "user", "Sounds right. Keep the import idempotent.", stamp(12)),
                FixtureMesh.turn("backend", 4, "assistant", "Done with the migration and the import. Running the race tests next; I need approval to clear old fixtures.", stamp(2), tools: [.init(name: "Edit", input: "state/session_bindings.go"), .init(name: "Bash", input: "go test ./state/")]),
            ],
            "web": [
                FixtureMesh.turn("web", 1, "user", "The peer roster looks broken when nothing is connected.", stamp(55)),
                FixtureMesh.turn("web", 2, "assistant", "Added an empty state with the `repowire up` command and a link to the quickstart. Screenshots are in the PR.", stamp(50)),
            ],
        ]
        jobList = [
            Job(jobId: "job-1", title: "Nightly dependency audit", kind: "scheduled", state: "running", phase: "Scanning go.mod", assignedPeerId: "p-infra", updatedAt: stamp(4)),
            Job(jobId: "job-2", title: "Regenerate API reference", kind: "work", state: "queued", assignedPeerId: "p-docs", updatedAt: stamp(15)),
            Job(jobId: "job-3", title: "Flaky test triage", kind: "work", state: "completed", resultSummary: "Fixed a race in the demotion test.", assignedPeerId: "p-backend", updatedAt: stamp(180)),
        ]
    }

    public func health() async throws -> Bool { true }
    public func peers() async throws -> [Peer] { peerList }

    public func events(since: String?) async throws -> [MeshEvent] {
        guard let since, let index = log.firstIndex(where: { $0.id == since }) else { return log }
        return Array(log[(index + 1)...])
    }

    public nonisolated func eventStream() -> AsyncThrowingStream<MeshEvent, Error> {
        AsyncThrowingStream { continuation in
            let id = UUID()
            Task { await self.subscribe(id, continuation) }
            continuation.onTermination = { _ in Task { await self.unsubscribe(id) } }
        }
    }

    public func transcript(peer: String, limit: Int) async throws -> TranscriptPage {
        TranscriptPage(turns: Array((transcripts[peer] ?? []).suffix(limit)), nextBefore: nil, repowireSessionId: nil)
    }

    public func send(_ text: String, to peerId: String, mode: SendMode) async throws -> String? {
        guard let peer = peerList.first(where: { $0.peerId == peerId }) else { throw MeshError.server(status: 404, message: "Unknown peer") }
        let cid = mode == .ask ? "ask-\(nextID())" : nil
        var turns = transcripts[peer.name] ?? []
        turns.append(FixtureMesh.turn(peer.name, turns.count + 1, "user", text, FixtureMesh.iso(.now)))
        transcripts[peer.name] = turns
        append(MeshEvent(id: nextID(), type: mode == .ask ? "query" : "notification", timestamp: FixtureMesh.iso(.now), from: RelayAPI.surfaceName, to: peer.name, text: text, correlationId: cid))
        return cid
    }

    public func answer(correlationId: String, optionId: String?, outcome: String?, text: String?) async throws {
        append(MeshEvent(id: nextID(), type: "ack", timestamp: FixtureMesh.iso(.now), from: RelayAPI.surfaceName, text: text ?? optionId ?? outcome, correlationId: correlationId))
    }

    public func jobs() async throws -> [Job] { jobList }

    public func registerDevice(token: String, environment: String, name: String) async throws {
        registeredDevices.append(token)
    }

    private func subscribe(_ id: UUID, _ continuation: AsyncThrowingStream<MeshEvent, Error>.Continuation) {
        continuations[id] = continuation
    }

    private func unsubscribe(_ id: UUID) {
        continuations[id] = nil
    }

    private func append(_ event: MeshEvent) {
        log.append(event)
        for continuation in continuations.values { continuation.yield(event) }
    }

    private func nextID() -> String {
        sequence += 1
        return "fx-\(sequence)"
    }

    static func iso(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.string(from: date)
    }

    static func peer(_ id: String, _ name: String, _ status: Peer.Status, _ circle: String, _ description: String?, backend: String, model: String?, turn: String?) -> Peer {
        var peer = Peer(peerId: id, name: name, status: status, circle: circle, description: description)
        peer.backend = backend
        peer.model = model
        peer.turnState = turn
        peer.machine = "studio"
        peer.path = "~/src/repowire"
        peer.role = "agent"
        return peer
    }

    static func turn(_ peer: String, _ index: Int, _ role: String, _ text: String, _ timestamp: String, tools: [TranscriptTurn.ToolCall] = []) -> TranscriptTurn {
        TranscriptTurn(role: role, text: text, timestamp: timestamp, sessionId: "s-\(peer)", turnId: "t\(index)", toolCalls: tools)
    }
}
