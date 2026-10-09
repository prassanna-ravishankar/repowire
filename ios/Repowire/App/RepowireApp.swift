import SwiftUI

@main
struct RepowireApp: App {
    @UIApplicationDelegateAdaptor private var delegate: AppDelegate
    @State private var model: AppModel

    init() {
        let arguments = ProcessInfo.processInfo.arguments
        _model = State(initialValue: AppModel(fixtures: arguments.contains("-fixtures"), onboarding: arguments.contains("-onboarding")))
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
                .tint(Theme.Palette.accent)
                .onAppear { delegate.model = model }
        }
    }
}

struct RootView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Group {
            switch model.phase {
            case .onboarding:
                OnboardingView()
                    .transition(.opacity)
            case .connected:
                MainView()
                    .transition(.opacity)
            }
        }
        .animation(Theme.Motion.standard, value: model.phase)
    }
}

struct MainView: View {
    @Environment(AppModel.self) private var model
    @State private var tab: Tab = .inbox

    enum Tab: Hashable { case inbox, peers, jobs, settings }

    var body: some View {
        TabView(selection: $tab) {
            SwiftUI.Tab("Inbox", systemImage: "tray", value: .inbox) {
                InboxView()
            }
            .badge(model.pendingQuestions.count)

            SwiftUI.Tab("Peers", systemImage: "point.3.connected.trianglepath.dotted", value: .peers) {
                PeersView()
            }

            SwiftUI.Tab("Jobs", systemImage: "checklist", value: .jobs) {
                JobsView()
            }

            SwiftUI.Tab("Settings", systemImage: "gearshape", value: .settings) {
                SettingsView()
            }
        }
    }
}
