import RepowireKit
import SwiftUI

/// One peer: what it is doing, its recent conversation, and a composer that
/// sends as the human (`@dashboard`).
struct PeerDetailView: View {
    @Environment(AppModel.self) private var model
    let peer: Peer
    @State private var turns: [TranscriptTurn] = []
    @State private var loading = true
    @State private var loadError: String?

    private var live: Peer { model.peers.first { $0.peerId == peer.peerId } ?? peer }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: Theme.Space.m) {
                    header
                    if loading {
                        ProgressView().frame(maxWidth: .infinity).padding(Theme.Space.xl)
                    } else if let loadError {
                        Label(loadError, systemImage: "exclamationmark.triangle")
                            .font(.footnote)
                            .foregroundStyle(Theme.Palette.danger)
                    } else if turns.isEmpty {
                        Text("No conversation yet. Send the first message below.")
                            .font(.subheadline)
                            .foregroundStyle(Theme.Palette.muted)
                            .padding(.vertical, Theme.Space.l)
                    }
                    ForEach(turns) { turn in
                        TurnBubble(turn: turn).id(turn.id)
                    }
                }
                .padding(Theme.Space.l)
            }
            .defaultScrollAnchor(.bottom)
            .hardTopScrollEdge()
            .background(Theme.Palette.page)
            .safeAreaInset(edge: .bottom) {
                Composer(peer: live) { await load() }
            }
            .onChange(of: turns.last?.id) { _, id in
                guard let id else { return }
                withAnimation(Theme.Motion.standard) { proxy.scrollTo(id, anchor: .bottom) }
            }
        }
        .navigationTitle("@\(live.label)")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            // Status lives in the bar: the conversation opens at its newest turn,
            // so a header in the scroll view would start off screen.
            ToolbarItem(placement: .principal) {
                VStack(spacing: Theme.Space.xxs) {
                    Text("@\(live.label)")
                        .font(Theme.Typeface.mono(.subheadline, weight: .semibold))
                        .foregroundStyle(Theme.Palette.foreground)
                    HStack(spacing: Theme.Space.xs) {
                        PeerActivity(peer: live)
                        Text(live.isWorking ? "Working" : live.status.label)
                            .font(.caption)
                            .foregroundStyle(Theme.Palette.muted)
                    }
                }
                .accessibilityElement(children: .combine)
                .accessibilityIdentifier("peer.header")
            }
        }
        .task { await load() }
        .refreshable { await load() }
        .onChange(of: model.events.count) { Task { await load() } }
    }

    private var header: some View {
        Card {
            if let backend = live.backend {
                Badge(text: [backend, live.model].compactMap(\.self).joined(separator: " · "))
            }
            if let description = live.description {
                Text(description)
                    .font(.body)
                    .foregroundStyle(Theme.Palette.foreground)
            }
            if let path = live.path {
                Text([live.machine, path].compactMap(\.self).joined(separator: ":"))
                    .font(Theme.Typeface.mono(.caption))
                    .foregroundStyle(Theme.Palette.faint)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
        }
    }

    private func load() async {
        do {
            turns = try await model.transcript(for: live)
            loadError = nil
        } catch {
            loadError = error.localizedDescription
        }
        loading = false
    }
}

struct TurnBubble: View {
    let turn: TranscriptTurn
    private var mine: Bool { turn.role == "user" }

    var body: some View {
        HStack {
            if mine { Spacer(minLength: Theme.Space.xxl) }
            VStack(alignment: .leading, spacing: Theme.Space.s) {
                Text(markdown: turn.text)
                    .font(.body)
                    .foregroundStyle(mine ? Theme.Palette.onAccent : Theme.Palette.foreground)
                    .textSelection(.enabled)
                if let tools = turn.toolCalls, !tools.isEmpty {
                    FlowTools(tools: tools)
                }
                Timestamp(
                    date: MeshEvent(id: "", type: "", timestamp: turn.timestamp).date,
                    tint: mine ? Theme.Palette.onAccentMuted : Theme.Palette.faint)
            }
            .padding(Theme.Space.m)
            .background(
                RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous)
                    .fill(mine ? Theme.Palette.accentFill : Theme.Palette.surface)
            )
            .overlay(
                RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous)
                    .strokeBorder(mine ? Color.clear : Theme.Palette.border, lineWidth: Theme.Stroke.hairline)
            )
            if !mine { Spacer(minLength: Theme.Space.xxl) }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(mine ? "You" : "Agent"): \(turn.text)")
    }
}

/// Tool calls as quiet mono chips: what the agent touched, not the full input.
struct FlowTools: View {
    let tools: [TranscriptTurn.ToolCall]

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: Theme.Space.xs) { chips }
            VStack(alignment: .leading, spacing: Theme.Space.xs) { chips }
        }
    }

    private var chips: some View {
        ForEach(Array(tools.prefix(4).enumerated()), id: \.offset) { _, tool in
            Text(tool.name)
                .font(Theme.Typeface.mono(.caption2, weight: .medium))
                .foregroundStyle(Theme.Palette.muted)
                .padding(.horizontal, Theme.Space.s)
                .padding(.vertical, Theme.Space.xxs)
                .background(Capsule().fill(Theme.Palette.sunken))
        }
    }
}

/// Ask expects an ack back; notify is fire-and-forget. Drafts survive failures.
struct Composer: View {
    @Environment(AppModel.self) private var model
    let peer: Peer
    let onSent: () async -> Void
    @State private var text = ""
    @State private var mode: SendMode = .ask
    @State private var sending = false
    @State private var error: String?
    @FocusState private var focused: Bool

    private var canSend: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !sending && peer.status != .offline }

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.s) {
            if let error {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(Theme.Palette.danger)
            }
            Picker("Mode", selection: $mode) {
                Text("Ask").tag(SendMode.ask)
                Text("Notify").tag(SendMode.notify)
            }
            .pickerStyle(.segmented)
            .fixedSize()
            .accessibilityIdentifier("composer.mode")

            HStack(alignment: .bottom, spacing: Theme.Space.s) {
                TextField(peer.status == .offline ? "@\(peer.label) is offline" : "Message @\(peer.label)", text: $text, axis: .vertical)
                    .lineLimit(1...6)
                    .focused($focused)
                    .padding(.horizontal, Theme.Space.m)
                    .padding(.vertical, Theme.Space.s + Theme.Space.xxs)
                    .background(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).fill(Theme.Palette.surface))
                    .overlay(
                        RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).strokeBorder(
                            Theme.Palette.border, lineWidth: Theme.Stroke.hairline)
                    )
                    .accessibilityIdentifier("composer.text")
                Button(action: send) {
                    Image(systemName: sending ? "ellipsis" : "arrow.up")
                        .font(.body.weight(.bold))
                        .foregroundStyle(Theme.Palette.onAccent)
                        .frame(width: Theme.Size.tapTarget, height: Theme.Size.tapTarget)
                        .background(Circle().fill(canSend ? Theme.Palette.accentFill : Theme.Palette.faint))
                }
                .disabled(!canSend)
                .accessibilityLabel("Send")
                .accessibilityIdentifier("composer.send")
            }
        }
        .padding(.horizontal, Theme.Space.l)
        .padding(.vertical, Theme.Space.s)
        .background(.bar)
    }

    private func send() {
        let message = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard canSend else { return }
        sending = true
        error = nil
        Task {
            defer { sending = false }
            do {
                try await model.send(message, to: peer, mode: mode)
                text = ""
                UINotificationFeedbackGenerator().notificationOccurred(.success)
                await onSent()
            } catch {
                self.error = error.localizedDescription
                UINotificationFeedbackGenerator().notificationOccurred(.error)
            }
        }
    }
}

extension View {
    /// A transcript opens at its newest turn, so older text always sits at the
    /// top edge; a hard edge keeps it from fading to half contrast (iOS 26+).
    @ViewBuilder
    fileprivate func hardTopScrollEdge() -> some View {
        if #available(iOS 26.0, *) {
            scrollEdgeEffectStyle(.hard, for: .top)
        } else {
            self
        }
    }
}
