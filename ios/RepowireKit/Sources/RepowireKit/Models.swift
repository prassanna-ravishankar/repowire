import Foundation

/// A mesh peer as returned by `GET /peers`.
public struct Peer: Codable, Identifiable, Hashable, Sendable {
    public enum Status: String, Codable, Sendable {
        case online, busy, offline
    }

    public var peerId: String
    public var name: String
    public var displayName: String?
    public var status: Status
    public var turnState: String?
    public var machine: String?
    public var path: String?
    public var backend: String?
    public var model: String?
    public var circle: String?
    public var role: String?
    public var description: String?
    public var addressable: Bool?
    public var initiator: String?
    public var parentRuntimeId: String?

    public var id: String { peerId }
    public var label: String { displayName?.isEmpty == false ? displayName! : name }

    /// Mirrors the dashboard's default roster rule: peers someone can send work to.
    public var isListed: Bool {
        addressable != false && initiator != "system" && role != "human" && role != "service"
    }

    public init(peerId: String, name: String, displayName: String? = nil, status: Status, circle: String? = nil, description: String? = nil) {
        self.peerId = peerId
        self.name = name
        self.displayName = displayName
        self.status = status
        self.circle = circle
        self.description = description
    }
}

public struct QuestionOption: Codable, Hashable, Sendable, Identifiable {
    public var id: String
    public var title: String
    public var description: String?

    public init(id: String, title: String, description: String? = nil) {
        self.id = id
        self.title = title
        self.description = description
    }
}

/// The structured question an `ask` event can carry.
public struct AskQuestion: Codable, Hashable, Sendable {
    public var kind: String
    public var prompt: String?
    public var options: [QuestionOption]?
    public var blocking: Bool?
    public var scope: String?

    public var isApproval: Bool { scope == "tool_permission" }

    public init(kind: String, prompt: String? = nil, options: [QuestionOption]? = nil, blocking: Bool? = nil, scope: String? = nil) {
        self.kind = kind
        self.prompt = prompt
        self.options = options
        self.blocking = blocking
        self.scope = scope
    }
}

/// One entry from the daemon's dashboard event log (`/events`, `/events/stream`).
public struct MeshEvent: Codable, Identifiable, Hashable, Sendable {
    public var id: String
    public var type: String
    public var timestamp: String
    public var from: String?
    public var to: String?
    public var fromPeerId: String?
    public var toPeerId: String?
    public var text: String?
    public var correlationId: String?
    public var question: AskQuestion?
    public var peer: String?
    public var peerId: String?
    public var peerName: String?
    public var role: String?
    public var sessionId: String?
    public var newStatus: String?

    public init(id: String, type: String, timestamp: String = "", from: String? = nil, to: String? = nil, text: String? = nil, correlationId: String? = nil, question: AskQuestion? = nil) {
        self.id = id
        self.type = type
        self.timestamp = timestamp
        self.from = from
        self.to = to
        self.text = text
        self.correlationId = correlationId
        self.question = question
    }

    /// Registry lifecycle events describe one peer rather than a routed message.
    public var isLifecycle: Bool {
        type.hasPrefix("peer_") || type == "status_change" || type == "offline_peer_still_has_runtime_evidence"
    }

    public var date: Date? { MeshEvent.parseDate(timestamp) }

    static func parseDate(_ value: String) -> Date? {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter.date(from: value) ?? ISO8601DateFormatter().date(from: value)
    }
}

/// An open question for the human, derived from the event log.
public struct PendingQuestion: Identifiable, Hashable, Sendable {
    public var correlationId: String
    public var from: String
    public var text: String
    public var question: AskQuestion
    public var askedAt: Date?

    public var id: String { correlationId }

    public init(correlationId: String, from: String, text: String, question: AskQuestion, askedAt: Date? = nil) {
        self.correlationId = correlationId
        self.from = from
        self.text = text
        self.question = question
        self.askedAt = askedAt
    }

    /// An `ask` carrying a question opens one; a later `ack` with the same
    /// correlation id closes it. Same rule as the web dashboard.
    public static func derive(from events: [MeshEvent]) -> [PendingQuestion] {
        var open: [String: PendingQuestion] = [:]
        var order: [String] = []
        for event in events {
            guard let cid = event.correlationId else { continue }
            if event.type == "ask", let question = event.question, question.kind != "acknowledge" {
                if open[cid] == nil { order.append(cid) }
                open[cid] = PendingQuestion(correlationId: cid, from: event.from ?? "?", text: event.text ?? "", question: question, askedAt: event.date)
            } else if event.type == "ack" {
                open[cid] = nil
            }
        }
        return order.compactMap { open[$0] }
    }
}

public struct TranscriptTurn: Codable, Hashable, Sendable, Identifiable {
    public struct ToolCall: Codable, Hashable, Sendable {
        public var name: String
        public var input: String
    }

    public var role: String
    public var text: String
    public var timestamp: String
    public var sessionId: String
    public var turnId: String
    public var toolCalls: [ToolCall]?

    public var id: String { "\(sessionId):\(turnId)" }
}

public struct TranscriptPage: Codable, Sendable {
    public var turns: [TranscriptTurn]
    public var nextBefore: String?
    public var repowireSessionId: String?
}

public struct Job: Codable, Hashable, Sendable, Identifiable {
    public var jobId: String?
    public var workId: String?
    public var title: String?
    public var kind: String?
    public var state: String?
    public var phase: String?
    public var resultSummary: String?
    public var assignedPeerId: String?
    public var createdAt: String?
    public var updatedAt: String?

    public var id: String { jobId ?? workId ?? UUID().uuidString }
    public var isActive: Bool {
        guard let state else { return false }
        return !["completed", "failed", "cancelled", "expired"].contains(state)
    }
}

public struct JobsPage: Codable, Sendable {
    public var work: [Job]
}

public struct AttachmentRef: Codable, Hashable, Sendable {
    public var id: String?
    public var path: String?
    public var filename: String?
    public var size: Int?
    public var contentType: String?
}

/// How a message to a peer is sent: `ask` expects an ack, `notify` does not.
public enum SendMode: String, Sendable, CaseIterable {
    case ask, notify
}

/// A decoding wrapper that keeps going when one element is malformed, so a new
/// event type with an unexpected field never blanks the whole feed.
struct Lossy<Element: Decodable>: Decodable {
    var elements: [Element]

    init(from decoder: Decoder) throws {
        var container = try decoder.unkeyedContainer()
        var elements: [Element] = []
        while !container.isAtEnd {
            if let element = try? container.decode(Element.self) {
                elements.append(element)
            } else {
                _ = try? container.decode(Discard.self)
            }
        }
        self.elements = elements
    }

    private struct Discard: Decodable {}
}
