import RepowireKit
import SwiftUI

/// Addressable peers grouped by circle; busy first, then online, then offline.
struct PeersView: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""

    private var circles: [(name: String, peers: [Peer])] {
        let matches = model.listedPeers.filter { peer in
            query.isEmpty || [peer.label, peer.description ?? "", peer.circle ?? ""].contains { $0.localizedCaseInsensitiveContains(query) }
        }
        return Dictionary(grouping: matches) { $0.circle ?? "global" }
            .map { (name: $0.key, peers: $0.value) }
            .sorted { $0.name < $1.name }
    }

    var body: some View {
        NavigationStack {
            List {
                ForEach(circles, id: \.name) { circle in
                    Section {
                        ForEach(circle.peers) { peer in
                            NavigationLink(value: peer) { PeerRow(peer: peer) }
                                .listRowBackground(Theme.Palette.surface)
                                .accessibilityIdentifier("peer.\(peer.name)")
                        }
                    } header: {
                        Eyebrow(text: circle.name)
                    }
                }
            }
            .listStyle(.insetGrouped)
            .pageBackground()
            .overlay {
                if model.listedPeers.isEmpty {
                    ContentUnavailableView {
                        Label("No peers online", systemImage: "point.3.connected.trianglepath.dotted")
                    } description: {
                        Text("Start an agent session on a machine running `repowire up` and it appears here.")
                    }
                } else if circles.isEmpty {
                    ContentUnavailableView.search(text: query)
                }
            }
            .searchable(text: $query, prompt: "Peers, circles, tasks")
            .refreshable { await model.refresh() }
            .navigationTitle("Peers")
            .navigationDestination(for: Peer.self) { PeerDetailView(peer: $0) }
        }
    }
}

struct PeerRow: View {
    let peer: Peer

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Space.m) {
            StatusDot(status: peer.status)
                .padding(.top, Theme.Space.s - Theme.Space.xxs)
            VStack(alignment: .leading, spacing: Theme.Space.xs) {
                HeaderRow {
                    Text("@\(peer.label)")
                        .font(Theme.Typeface.mono(.body, weight: .semibold))
                        .foregroundStyle(peer.status == .offline ? Theme.Palette.muted : Theme.Palette.foreground)
                } trailing: {
                    if let backend = peer.backend {
                        Badge(text: backend)
                    }
                }
                Text(peer.description ?? peer.status.label)
                    .font(.subheadline)
                    .foregroundStyle(Theme.Palette.muted)
                    .lineLimit(2)
                if peer.turnState == "awaiting_input" {
                    Badge(text: "Waiting on you", tint: Theme.Palette.accent)
                }
            }
        }
        .padding(.vertical, Theme.Space.xs)
        .accessibilityElement(children: .combine)
    }
}
