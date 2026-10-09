import RepowireKit
import SwiftUI

/// Durable work: what is running now, then what finished recently.
struct JobsView: View {
    @Environment(AppModel.self) private var model

    private var active: [Job] { model.jobs.filter(\.isActive) }
    private var finished: [Job] { model.jobs.filter { !$0.isActive } }

    var body: some View {
        NavigationStack {
            List {
                if !active.isEmpty {
                    Section {
                        ForEach(active) { JobRow(job: $0) }
                    } header: {
                        Eyebrow(text: "Active")
                    }
                }
                if !finished.isEmpty {
                    Section {
                        ForEach(finished) { JobRow(job: $0) }
                    } header: {
                        Eyebrow(text: "Recent")
                    }
                }
            }
            .listStyle(.insetGrouped)
            .pageBackground()
            .overlay {
                if model.jobs.isEmpty {
                    ContentUnavailableView {
                        Label("No jobs", systemImage: "checklist")
                    } description: {
                        Text("Scheduled and tracked work from your agents shows up here.")
                    }
                }
            }
            .refreshable { await model.reloadJobs() }
            .navigationTitle("Jobs")
        }
    }
}

struct JobRow: View {
    @Environment(AppModel.self) private var model
    let job: Job

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.xs) {
            HeaderRow {
                Text(job.title ?? job.id)
                    .font(.body.weight(.medium))
                    .foregroundStyle(Theme.Palette.foreground)
            } trailing: {
                Badge(text: job.state ?? "unknown", tint: tint)
            }
            if let detail = job.phase ?? job.resultSummary {
                Text(detail)
                    .font(.subheadline)
                    .foregroundStyle(Theme.Palette.muted)
                    .lineLimit(2)
            }
            HStack(spacing: Theme.Space.s) {
                if let owner = job.assignedPeerId.flatMap({ id in model.peers.first { $0.peerId == id } }) {
                    Text("@\(owner.label)")
                        .font(Theme.Typeface.mono(.caption))
                        .foregroundStyle(Theme.Palette.faint)
                }
                Timestamp(date: job.updatedAt.flatMap { MeshEvent(id: "", type: "", timestamp: $0).date })
            }
        }
        .padding(.vertical, Theme.Space.xs)
        .listRowBackground(Theme.Palette.surface)
        .accessibilityElement(children: .combine)
    }

    private var tint: Color {
        switch job.state {
        case "running": Theme.Palette.accent
        case "completed": Theme.Palette.success
        case "failed", "cancelled": Theme.Palette.danger
        default: Theme.Palette.muted
        }
    }
}
