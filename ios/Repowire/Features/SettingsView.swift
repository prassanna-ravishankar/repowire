import RepowireKit
import SwiftUI
import UserNotifications

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var notifications: UNAuthorizationStatus = .notDetermined
    @State private var confirmSignOut = false

    var body: some View {
        NavigationStack {
            List {
                Section {
                    row("Relay", value: host)
                    HStack {
                        Text("Status")
                        Spacer()
                        StatusDot(status: model.connection == .live ? .online : .busy)
                        Text(connectionLabel)
                            .font(Theme.Typeface.mono(.subheadline))
                            .foregroundStyle(Theme.Palette.muted)
                    }
                    row("Peers", value: "\(model.listedPeers.count)")
                } header: {
                    Eyebrow(text: "Connection")
                }

                Section {
                    switch notifications {
                    case .authorized, .provisional, .ephemeral:
                        Label("Notifications are on", systemImage: "bell.badge")
                            .foregroundStyle(Theme.Palette.foreground)
                    case .denied:
                        Button("Turn on in iOS Settings") {
                            if let url = URL(string: UIApplication.openNotificationSettingsURLString) { UIApplication.shared.open(url) }
                        }
                    default:
                        Button("Turn on notifications") {
                            Task {
                                _ = await Notifications.enable()
                                notifications = await Notifications.status()
                            }
                        }
                        .accessibilityIdentifier("settings.notifications")
                    }
                } header: {
                    Eyebrow(text: "Notifications")
                } footer: {
                    Text("Approvals and questions addressed to you arrive as notifications you can answer from the lock screen.")
                        .foregroundStyle(Theme.Palette.muted)
                }

                Section {
                    Button("Disconnect", role: .destructive) { confirmSignOut = true }
                        .accessibilityIdentifier("settings.disconnect")
                } footer: {
                    Text("Removes the relay key from this device. Your daemon and agents keep running.")
                        .foregroundStyle(Theme.Palette.muted)
                }
            }
            .listStyle(.insetGrouped)
            .pageBackground()
            .navigationTitle("Settings")
            .task { notifications = await Notifications.status() }
            .confirmationDialog("Disconnect from the relay?", isPresented: $confirmSignOut, titleVisibility: .visible) {
                Button("Disconnect", role: .destructive) { model.signOut() }
            }
        }
    }

    private var host: String {
        if case .connected(let host) = model.phase { return host }
        return "None"
    }

    private var connectionLabel: String {
        switch model.connection {
        case .live: "Live"
        case .connecting: "Connecting"
        case .reconnecting: "Reconnecting"
        case .failed: "Failed"
        }
    }

    private func row(_ title: String, value: String) -> some View {
        HStack {
            Text(title)
            Spacer()
            Text(value)
                .font(Theme.Typeface.mono(.subheadline))
                .foregroundStyle(Theme.Palette.muted)
        }
        .listRowBackground(Theme.Palette.surface)
    }
}
