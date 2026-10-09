import RepowireKit
import SwiftUI

/// What needs the human: open questions first, then messages addressed to you.
struct InboxView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        NavigationStack {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: Theme.Space.l) {
                    ConnectionBanner()

                    if model.pendingQuestions.isEmpty && model.inbox.isEmpty {
                        ContentUnavailableView {
                            Label("Nothing needs you", systemImage: "checkmark.circle")
                        } description: {
                            Text("Questions, approvals, and messages from your agents land here.")
                        }
                        .padding(.top, Theme.Space.xxl)
                        .accessibilityIdentifier("inbox.empty")
                    }

                    if !model.pendingQuestions.isEmpty {
                        Eyebrow(text: "Needs you · \(model.pendingQuestions.count)")
                        ForEach(model.pendingQuestions) { question in
                            QuestionCard(question: question)
                                .transition(.asymmetric(insertion: .opacity, removal: .opacity.combined(with: .move(edge: .trailing))))
                        }
                    }

                    if !model.inbox.isEmpty {
                        Eyebrow(text: "Messages")
                            .padding(.top, Theme.Space.s)
                        VStack(spacing: 0) {
                            ForEach(Array(model.inbox.prefix(50).enumerated()), id: \.element.id) { index, event in
                                if index > 0 { Divider().overlay(Theme.Palette.border) }
                                MessageRow(event: event)
                            }
                        }
                        .background(RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).fill(Theme.Palette.surface))
                        .overlay(RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).strokeBorder(Theme.Palette.border, lineWidth: Theme.Stroke.hairline))
                    }
                }
                .padding(Theme.Space.l)
                .animation(Theme.Motion.standard, value: model.pendingQuestions)
            }
            .background(Theme.Palette.page)
            .refreshable { await model.refresh() }
            .navigationTitle("Inbox")
            .navigationDestination(for: Peer.self) { PeerDetailView(peer: $0) }
        }
    }
}

struct MessageRow: View {
    @Environment(AppModel.self) private var model
    let event: MeshEvent

    var body: some View {
        let content = VStack(alignment: .leading, spacing: Theme.Space.xs) {
            HStack(alignment: .firstTextBaseline) {
                Text("@\(event.from ?? "mesh")")
                    .font(Theme.Typeface.mono(.subheadline, weight: .semibold))
                    .foregroundStyle(Theme.Palette.foreground)
                Spacer()
                Timestamp(date: event.date)
            }
            Text(markdown: event.text ?? "")
                .font(.subheadline)
                .foregroundStyle(Theme.Palette.muted)
                .lineLimit(4)
        }
        .padding(Theme.Space.l)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())

        if let peer = event.from.flatMap(model.peer(named:)) {
            NavigationLink(value: peer) { content }
                .buttonStyle(.plain)
        } else {
            content
        }
    }
}
