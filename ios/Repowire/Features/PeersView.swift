import RepowireKit
import SwiftUI

/// Addressable peers grouped by circle; busy first, then online, then offline.
struct PeersView: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""
    /// Collapsed circle names, remembered across launches.
    @AppStorage("peers.collapsedCircles") private var collapsedStorage = ""

    private var collapsed: Set<String> {
        Set(collapsedStorage.split(separator: "\n").map(String.init))
    }

    private func toggle(_ circle: String) {
        var next = collapsed
        if next.remove(circle) == nil { next.insert(circle) }
        withAnimation(Theme.Motion.standard) { collapsedStorage = next.sorted().joined(separator: "\n") }
    }

    private var circles: [(name: String, peers: [Peer])] {
        let matches = model.listedPeers.filter { peer in
            query.isEmpty
                || [peer.label, peer.description ?? "", peer.circle ?? ""].contains { $0.localizedCaseInsensitiveContains(query) }
        }
        return Dictionary(grouping: matches) { $0.circle ?? "global" }
            .map { (name: $0.key, peers: $0.value) }
            .sorted { $0.name < $1.name }
    }

    var body: some View {
        NavigationStack {
            List {
                ForEach(circles, id: \.name) { circle in
                    // Searching shows every match, so collapse only applies to browsing.
                    let isCollapsed = query.isEmpty && collapsed.contains(circle.name)
                    Section {
                        if !isCollapsed {
                            ForEach(circle.peers) { peer in
                                NavigationLink(value: peer) { PeerRow(peer: peer) }
                                    .listRowBackground(Theme.Palette.surface)
                                    .accessibilityIdentifier("peer.\(peer.name)")
                            }
                        }
                    } header: {
                        CircleHeader(name: circle.name, peers: circle.peers, collapsed: isCollapsed) { toggle(circle.name) }
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

/// Tappable circle header: name, how many peers and how many are working, and a
/// chevron that turns with the collapse.
struct CircleHeader: View {
    let name: String
    let peers: [Peer]
    let collapsed: Bool
    let toggle: () -> Void

    private var working: Int { peers.filter(\.isWorking).count }

    var body: some View {
        Button(action: toggle) {
            HStack(spacing: Theme.Space.s) {
                Eyebrow(text: "\(name) · \(peers.count)")
                if collapsed && working > 0 {
                    ThinkingOrb()
                    Eyebrow(text: "\(working) working", color: Theme.Palette.warning)
                }
                Spacer(minLength: 0)
                Image(systemName: "chevron.down")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(Theme.Palette.faint)
                    .rotationEffect(.degrees(collapsed ? -90 : 0))
            }
            .frame(minHeight: Theme.Size.tapTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("\(name), \(peers.count) peers")
        .accessibilityValue(collapsed ? "Collapsed" : "Expanded")
        .accessibilityHint(collapsed ? "Shows the circle's peers" : "Hides the circle's peers")
        .accessibilityIdentifier("circle.\(name)")
    }
}

struct PeerRow: View {
    let peer: Peer

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Space.m) {
            PeerActivity(peer: peer)
                .padding(.top, Theme.Space.xxs)
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
                Text(peer.description ?? (peer.isWorking ? "Working" : peer.status.label))
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
        .accessibilityValue(peer.isWorking ? "Working" : peer.status.label)
    }
}
