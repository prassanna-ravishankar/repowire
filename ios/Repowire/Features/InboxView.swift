import RepowireKit
import SwiftUI

/// What needs the human: open questions first, then messages addressed to you.
struct InboxView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

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
                                .transition(
                                    reduceMotion
                                        ? .opacity
                                        : .asymmetric(insertion: .opacity, removal: .opacity.combined(with: .move(edge: .trailing))))
                        }
                    }

                    if !model.inbox.isEmpty {
                        Eyebrow(text: "Messages")
                            .padding(.top, Theme.Space.s)
                        VStack(spacing: 0) {
                            ForEach(Array(model.inbox.prefix(50).enumerated()), id: \.element.id) { index, item in
                                if index > 0 { Divider().overlay(Theme.Palette.border) }
                                MessageRow(item: item)
                            }
                        }
                        .background(RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).fill(Theme.Palette.surface))
                        .overlay(
                            RoundedRectangle(cornerRadius: Theme.Radius.card, style: .continuous).strokeBorder(
                                Theme.Palette.border, lineWidth: Theme.Stroke.hairline))
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

/// One inbox item. Unread items carry an accent dot; opening the sender's
/// conversation marks the item read on every surface.
struct MessageRow: View {
    @Environment(AppModel.self) private var model
    let item: InboxItem

    var body: some View {
        let content = VStack(alignment: .leading, spacing: Theme.Space.xs) {
            HeaderRow {
                HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                    if !item.read {
                        Circle()
                            .fill(Theme.Palette.accent)
                            .frame(width: Theme.Size.statusDot, height: Theme.Size.statusDot)
                            .accessibilityLabel("Unread")
                    }
                    Text("@\(item.from)")
                        .font(Theme.Typeface.mono(.subheadline, weight: .semibold))
                        .foregroundStyle(Theme.Palette.foreground)
                }
            } trailing: {
                Timestamp(date: item.date)
            }
            Text(markdown: item.text)
                .font(.subheadline)
                .foregroundStyle(Theme.Palette.muted)
                .lineLimit(4)
        }
        .padding(Theme.Space.l)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityIdentifier("inbox.item.\(item.id)")

        if let peer = model.peer(named: item.from) {
            NavigationLink(value: peer) { content }
                .buttonStyle(.plain)
                .simultaneousGesture(TapGesture().onEnded { Task { await model.markRead(item) } })
        } else {
            content
                .onTapGesture { Task { await model.markRead(item) } }
        }
    }
}
