import RepowireKit
import SwiftUI

/// One screen, one job: paste the relay key and connect. Validates against the
/// relay before saving so a bad key fails here, inline, not later.
struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    @State private var relay = Credentials.defaultRelay.host() ?? "repowire.io"
    @State private var key = ""
    @State private var connecting = false
    @State private var error: String?
    @FocusState private var focus: Field?

    enum Field { case relay, key }

    private var relayURL: URL? { Credentials.normalizedRelayURL(relay) }
    private var canConnect: Bool { relayURL != nil && Credentials.isPlausibleKey(key) && !connecting }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Theme.Space.xl) {
                Image("LogoMark")
                    .resizable()
                    .scaledToFit()
                    .frame(height: Theme.Space.xxl + Theme.Space.l)
                    .accessibilityHidden(true)
                    .padding(.top, Theme.Space.xxl)

                VStack(alignment: .leading, spacing: Theme.Space.s) {
                    Text("Connect to your mesh")
                        .font(.largeTitle.weight(.bold))
                        .tracking(-0.6)
                        .foregroundStyle(Theme.Palette.foreground)
                    Text("Answer questions, approve tool calls, and message your agents from anywhere. The relay key stays in this device's Keychain.")
                        .font(.body)
                        .foregroundStyle(Theme.Palette.muted)
                }

                Card {
                    field("Relay") {
                        TextField("repowire.io", text: $relay)
                            .textContentType(.URL)
                            .keyboardType(.URL)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .focused($focus, equals: .relay)
                            .submitLabel(.next)
                            .onSubmit { focus = .key }
                            .accessibilityIdentifier("onboarding.relay")
                    }
                    Divider().overlay(Theme.Palette.border)
                    field("Relay key") {
                        HStack(spacing: Theme.Space.s) {
                            SecureField("rw_…", text: $key)
                                .textContentType(.password)
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                                .focused($focus, equals: .key)
                                .submitLabel(.go)
                                .onSubmit(connect)
                                .accessibilityIdentifier("onboarding.key")
                            PasteButton(payloadType: String.self) { strings in
                                if let first = strings.first { key = first.trimmingCharacters(in: .whitespacesAndNewlines) }
                            }
                            .labelStyle(.iconOnly)
                            .buttonBorderShape(.roundedRectangle(radius: Theme.Radius.control))
                            .tint(Theme.Palette.accent)
                        }
                    }
                }

                if let error {
                    Label(error, systemImage: "exclamationmark.circle")
                        .font(.callout)
                        .foregroundStyle(Theme.Palette.danger)
                        .accessibilityIdentifier("onboarding.error")
                        .transition(.opacity)
                }

                Button(action: connect) {
                    HStack(spacing: Theme.Space.s) {
                        if connecting { ProgressView().tint(.white) }
                        Text(connecting ? "Connecting" : "Connect")
                    }
                }
                .buttonStyle(.rwPrimary)
                .disabled(!canConnect)
                .opacity(canConnect || connecting ? 1 : 0.5)
                .accessibilityIdentifier("onboarding.connect")

                VStack(alignment: .leading, spacing: Theme.Space.xs) {
                    Eyebrow(text: "Where to find it")
                    Text("Run `repowire setup --relay` on the machine running your agents, then copy `relay.api_key` from `~/.repowire/config.yaml`.")
                        .font(.footnote)
                        .foregroundStyle(Theme.Palette.muted)
                }
            }
            .padding(.horizontal, Theme.Space.xl)
            .padding(.bottom, Theme.Space.xxl)
            .animation(Theme.Motion.standard, value: error)
        }
        .scrollDismissesKeyboard(.interactively)
        .background(Theme.Palette.page)
    }

    private func field(_ label: String, @ViewBuilder content: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: Theme.Space.xs) {
            Eyebrow(text: label)
            content()
                .font(Theme.Typeface.mono(.body))
                .foregroundStyle(Theme.Palette.foreground)
                .frame(minHeight: Theme.Size.tapTarget)
        }
    }

    private func connect() {
        guard canConnect, let relayURL else { return }
        connecting = true
        error = nil
        focus = nil
        Task {
            defer { connecting = false }
            do {
                try await model.connect(relay: relayURL, key: key)
            } catch {
                self.error = error.localizedDescription
            }
        }
    }
}
