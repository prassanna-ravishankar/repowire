import RepowireKit
import SwiftUI

/// One screen, one job: paste the relay key and connect. Validates against the
/// relay before saving so a bad key fails here, inline, not later.
enum Links {
    // swift-format-ignore: NeverForceUnwrap
    // Constant, valid URL literals; the same links as the relay landing page.
    static let docs = URL(string: "https://docs.repowire.io/")!
    // swift-format-ignore: NeverForceUnwrap
    static let github = URL(string: "https://github.com/prassanna-ravishankar/repowire")!
}

struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    @State private var relay = Credentials.defaultRelay.host() ?? "relay.repowire.io"
    @State private var key = ""
    @State private var connecting = false
    @State private var error: String?
    @FocusState private var focus: Field?

    enum Field { case relay, key }

    private var relayURL: URL? { Credentials.normalizedRelayURL(relay) }
    private var canConnect: Bool { relayURL != nil && Credentials.isPlausibleKey(key) && !connecting }

    var body: some View {
        ScrollView {
            VStack(spacing: Theme.Space.xl) {
                // Same mark, words, and hint as the relay landing page.
                VStack(spacing: Theme.Space.s) {
                    RelayMark()
                    Text("repowire")
                        .font(.title.weight(.semibold))
                        .tracking(-1.1)
                        .foregroundStyle(Theme.Palette.foreground)
                        .accessibilityAddTraits(.isHeader)
                    Text("Mesh network for AI coding agents")
                        .font(.body)
                        .foregroundStyle(Theme.Palette.muted)
                }
                .padding(.top, Theme.Space.xl)

                Card {
                    field("Relay") {
                        TextField("relay.repowire.io", text: $relay, axis: .vertical)
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

                VStack(spacing: Theme.Space.m) {
                    if let error {
                        Label(error, systemImage: "exclamationmark.circle")
                            .font(.callout)
                            .foregroundStyle(Theme.Palette.danger)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .accessibilityIdentifier("onboarding.error")
                            .transition(.opacity)
                    }

                    Button(action: connect) {
                        HStack(spacing: Theme.Space.s) {
                            if connecting { ProgressView().tint(Theme.Palette.onAccent) }
                            Text(connecting ? "Connecting" : "Connect")
                        }
                    }
                    .buttonStyle(.rwPrimary)
                    .disabled(!canConnect)
                    .opacity(canConnect || connecting ? 1 : 0.5)
                    .accessibilityIdentifier("onboarding.connect")
                }

                VStack(spacing: Theme.Space.m) {
                    Text("Run `repowire setup --relay` to get your key. It is `relay.api_key` in `~/.repowire/config.yaml`.")
                        .font(.footnote)
                        .foregroundStyle(Theme.Palette.muted)
                        .multilineTextAlignment(.center)
                    HStack(spacing: Theme.Space.xs) {
                        footerLink("Docs", Links.docs)
                        Text("·").foregroundStyle(Theme.Palette.faint).accessibilityHidden(true)
                        footerLink("GitHub", Links.github)
                    }
                    .font(.footnote)
                }
            }
            .padding(.horizontal, Theme.Space.xl)
            .padding(.bottom, Theme.Space.xxl)
            .animation(Theme.Motion.standard, value: error)
        }
        .scrollDismissesKeyboard(.interactively)
        .background(Theme.Palette.page)
    }

    /// Small text, full-size target.
    private func footerLink(_ title: String, _ url: URL) -> some View {
        Link(title, destination: url)
            .padding(.horizontal, Theme.Space.s)
            .frame(minWidth: Theme.Size.tapTarget, minHeight: Theme.Size.tapTarget)
            .contentShape(Rectangle())
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
