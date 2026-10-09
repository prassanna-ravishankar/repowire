# iOS app

The native client lives in `ios/`. It is a SwiftUI app that reaches a daemon through the relay as the `@dashboard` human surface, plus a Swift package with everything that does not need UIKit.

| Path | Contents |
| --- | --- |
| `ios/RepowireKit/` | Swift package: models, `RelayAPI`, the SSE event stream, Keychain credentials, and `FixtureMesh` |
| `ios/Repowire/` | App target: `App/` (model, push), `Design/` (tokens and shared components), `Features/` (screens) |
| `ios/RepowireUITests/` | `FlowTests` (functional) and `ScreenshotTour` (design review captures) |
| `ios/scripts/` | `check.sh`, `design-lint.py`, `contact-sheet.py`, `render-icon.swift` |

Requirements: Xcode 26 or later with an iOS simulator runtime. The project uses folder-synchronized groups, so new Swift files under `ios/Repowire/` join the target without editing the project file.

## Run it

Open `ios/Repowire.xcodeproj` and run the `Repowire` scheme. To work without a relay, enable the `-fixtures` launch argument in the scheme: the app then serves `FixtureMesh`, a deterministic in-memory mesh whose answers and sends behave like a daemon's. With `-fixtures -onboarding`, any key ending in `demo` connects.

From a terminal:

```bash
ios/scripts/check.sh
```

`--fast` runs only the package tests and the lint. `IOS_SIM_DEVICE` picks the simulator (default `iPhone 18 Pro`).

## How it talks to the mesh

All traffic goes through the relay tunnel with `Authorization: Bearer <relay key>`. The relay strips the key before forwarding, so the daemon never sees it.

| Need | Route |
| --- | --- |
| Roster, transcript, jobs | `GET /peers`, `GET /peers/{name}/transcript`, `GET /jobs` |
| Live updates | `GET /events/stream` (SSE), with `GET /events?since=<id>` to recover gaps after a reconnect |
| Send | `POST /ask` or `POST /notify` with `from_peer: "dashboard"` |
| Answer a question | `POST /answer` with `option_id`, `outcome`, or `text` |
| Push registration | `POST /push/devices` |

Open questions are derived from the event log with the same rule the web dashboard uses: an `ask` carrying a `question` opens one, an `ack` with the same correlation id closes it.

## Quality gates

`check.sh` tests three things: whether the app works, whether it stays consistent, and whether it looks right.

### Function

- `RepowireKit` tests (Swift Testing) cover the relay client against a `URLProtocol` stub: auth header, request bodies, error mapping, lossy event decoding, SSE framing, question derivation, and fixture behavior.
- `FlowTests` drive the app on `FixtureMesh`: onboarding rejects a bad key and connects with a good one, Allow and Deny clear an approval, choosing an option answers a question, a sent message appears in the conversation, offline peers cannot be messaged, jobs list, and Disconnect returns to onboarding.

### Consistency

`scripts/design-lint.py` fails when feature code bypasses the design system:

| Rule | Catches |
| --- | --- |
| `raw-color` | `Color(red:…)`, `UIColor(…)`, `Color.blue`, `.foregroundStyle(.red)` |
| `raw-space` | numeric padding, spacing, radii, frame sizes, and stroke widths |
| `raw-font` | `.font(.system(size:))`, custom fonts |
| `raw-motion` | animations that are not `Theme.Motion` |
| `emoji`, `exclamation`, `title-case` | copy that breaks the Repowire voice |

Tokens live in `ios/Repowire/Design/Theme.swift` and mirror [Design system](design-system.md): warm-paper neutrals, one cobalt accent, a 4pt grid, 10pt control and 14pt card radii, monospaced type for technical chrome (peer names, ids, timestamps, eyebrows). Add a token instead of a literal.

`ScreenshotTour` also checks consistency across variants: it captures every screen in light, dark, and accessibility-large text, and `contact-sheet.py` lays them out as `ios/build/design-review/index.html` with screens as rows and variants as columns, so a screen that drifts in one variant stands out.

### Taste

Taste is reviewed against the contact sheet, not asserted in code. Score each screen 0 to 2 on every criterion. A screen passes at 14 of 16 with no zeros. Record scores and fixes in the PR.

| Criterion | 2 means |
| --- | --- |
| Purpose | The screen's main job is the most prominent thing on it. In the Inbox, what needs you comes first. |
| Hierarchy | Weight, size, and spacing make the reading order clear without help from color. |
| Restraint | One accent, used only for primary actions, selection, and links. Status colors only for status. |
| Rhythm | Spacing follows the 4pt grid. Related things are closer together than unrelated things. Edges line up. |
| Type | System text for prose, mono for identifiers. Large text wraps instead of truncating the important parts. |
| Feedback | Every action responds on press, shows progress while busy, and shows an inline error when it fails. |
| Wayfinding | You can tell where you are, what is tappable, and how to get back. Empty states say what to do next. |
| Platform | Native navigation, lists, search, and sheets. Nothing a native app would not do. Dark mode reads as warm dark, not inverted. |

## Push notifications

The daemon decides what deserves a push and the relay sends it. See [Relay](../operate/relay.md#push-notifications) for relay configuration. In the app, notifications are requested from Settings, not at launch, and APPROVAL and QUESTION notifications can be answered from the lock screen.
