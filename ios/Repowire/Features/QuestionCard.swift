import RepowireKit
import SwiftUI

/// An open question from an agent. Approvals put Allow and Deny side by side;
/// choices list their options; free-text questions take a reply.
struct QuestionCard: View {
    @Environment(AppModel.self) private var model
    let question: PendingQuestion
    @State private var reply = ""

    private var busy: Bool { model.answering.contains(question.correlationId) }
    private var options: [QuestionOption] { question.question.options ?? [] }

    var body: some View {
        Card {
            HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                Text("@\(question.from)")
                    .font(Theme.Typeface.mono(.subheadline, weight: .semibold))
                    .foregroundStyle(Theme.Palette.foreground)
                Badge(text: question.question.isApproval ? "Approval" : "Question",
                      tint: question.question.isApproval ? Theme.Palette.warning : Theme.Palette.accent)
                Spacer(minLength: 0)
                Timestamp(date: question.askedAt)
            }

            Text(markdown: question.text)
                .font(.body)
                .foregroundStyle(Theme.Palette.foreground)
                .fixedSize(horizontal: false, vertical: true)

            if question.question.isApproval, let prompt = question.question.prompt, prompt != question.text {
                Text(prompt)
                    .font(Theme.Typeface.mono(.footnote))
                    .foregroundStyle(Theme.Palette.muted)
                    .padding(Theme.Space.m)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).fill(Theme.Palette.sunken))
                    .textSelection(.enabled)
            }

            actions
                .disabled(busy)
                .opacity(busy ? 0.6 : 1)
                .overlay { if busy { ProgressView() } }
        }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("question.\(question.correlationId)")
    }

    @ViewBuilder private var actions: some View {
        if question.question.isApproval {
            VStack(spacing: Theme.Space.s) {
                HStack(spacing: Theme.Space.s) {
                    Button("Deny") { answer(outcome: "denied") }
                        .buttonStyle(.rwDestructive)
                        .accessibilityIdentifier("question.deny")
                    Button(options.first?.title ?? "Allow") { answer(optionId: options.first?.id) }
                        .buttonStyle(.rwPrimary)
                        .accessibilityIdentifier("question.allow")
                }
                ForEach(options.dropFirst()) { option in
                    Button(option.title) { answer(optionId: option.id) }
                        .buttonStyle(.rwSecondary)
                }
            }
        } else if !options.isEmpty {
            VStack(spacing: Theme.Space.s) {
                ForEach(options) { option in
                    Button(option.title) { answer(optionId: option.id) }
                        .buttonStyle(.rwSecondary)
                        .accessibilityIdentifier("question.option.\(option.id)")
                }
            }
        } else {
            HStack(spacing: Theme.Space.s) {
                TextField("Reply", text: $reply, axis: .vertical)
                    .lineLimit(1...4)
                    .padding(.horizontal, Theme.Space.m)
                    .frame(minHeight: Theme.Size.tapTarget)
                    .background(RoundedRectangle(cornerRadius: Theme.Radius.control, style: .continuous).fill(Theme.Palette.sunken))
                Button("Send") { answer(text: reply) }
                    .buttonStyle(RWButtonStyle(kind: .primary))
                    .fixedSize()
                    .disabled(reply.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
    }

    private func answer(optionId: String? = nil, outcome: String? = nil, text: String? = nil) {
        UIImpactFeedbackGenerator(style: .light).impactOccurred()
        Task { await model.answer(question, optionId: optionId, outcome: outcome, text: text) }
    }
}
