# Design System: Copper Mesh

## Overview

Repowire is an operator-grade mesh for AI coding agents. The interface should feel like a dense tmux window with a circuit-printed brand: local-first, technical, quiet, and direct.

The previous cyan/neon system is legacy. New product and marketing UI uses Copper Mesh.

## Voice

- Write builder-to-builder copy. Prefer short technical sentences over marketing language.
- Use second person for the user and third person for the product.
- Use sentence case for headings and actions. Reserve all caps with wide tracking for small labels, status pills, and console annunciators.
- Use mono for peer names, paths, IDs, branches, commands, timestamps, and routing strings.
- Do not use emoji in product chrome. User-authored content can contain anything the user wrote.

Common patterns:

| Pattern | Example |
| --- | --- |
| Console hint | `> No peers registered` |
| Route | `dashboard -> backend` |
| Peer attribution | `@backend says:` |
| Status label | `MESH CONNECTED` |

## Color

Palette: warm ink surfaces with copper as the wire and status colors tuned for terminal readability.

| Role | Token | Hex |
| --- | --- | --- |
| Brand | `--copper-500` | `#C77B3D` |
| Signal accent | `--signal-300` | `#5BA3F5` |
| Online | `--online` | `#B6E04A` |
| Busy | `--busy` | `#F0B548` |
| Offline | `--offline` | `#5C5F66` |
| Error | `--error` | `#D5503A` |
| Dark page | `--ink-950` | `#0F0E0C` |
| Light page | `--bone-100` | `#F7F2E7` |

Rules:

- Dark product surfaces default to flat ink with a subtle 22px copper dot mesh.
- Cards use `--ink-800`, a 1px warm border, and no rest shadow.
- Hover lifts by changing the surface tier, not by scaling.
- CTAs use solid copper, not gradients.
- Use blur only for the fixed top bar over the mesh background.
- Keep legacy cyan assets only for compatibility; do not use them in active UI.

## Type

Repowire is mono-forward:

| Family | Usage |
| --- | --- |
| JetBrains Mono | Display, headings, navigation, labels, code-like data |
| IBM Plex Sans | Body copy, longer descriptions, marketing prose |

Fonts are self-hosted in the web app as `.woff2` files.

Type rules:

- Body default: 14px IBM Plex Sans.
- Compact labels: 10-11px JetBrains Mono, uppercase, wide tracking.
- Dashboard headings: JetBrains Mono 600/700.
- Use tabular numbers for counts and timestamps.

## Components

### Dashboard Shell

- Desktop layout is a two-column operator console: fixed top bar, left peer roster, right live mesh log or peer chat.
- Mobile layout is single-column with a top bar and PEERS/MESH bottom switcher.
- Selecting a peer replaces the live log with that peer's chat and composer.
- Spawn and settings open centered modals with a 60% black scrim.

### Peer Rows

- Group peers by circle.
- Sort online first, busy second, offline last, then by label.
- Show status dot, peer label, backend/circle/branch, path, and description when present.
- Selected peer uses a 2px copper left strip and warm copper text.

### Live Feed

- The default dashboard view is `mesh.log`, a dense chronological event stream.
- Rows are mono, timestamped, and use arrow routing.
- Query/response/notify/broadcast types get subtle semantic color, but text remains readable on ink.

### Chat

- User/dashboard messages align right with a copper right strip.
- Peer messages align left with a copper left strip.
- Tool calls collapse behind a compact mono disclosure.
- Composer docks to the bottom of the peer view, supports attachments, and keeps existing notify/query behavior.

### Forms and Modals

- Inputs use `--ink-1000` background, 1px warm border, and copper focus ring.
- Buttons have 4px or smaller radii. Pills are only for status badges and switches.
- Modal elevation uses `--shadow-3`; no slide-over panels.

## Assets

Production web assets live under `web/public/brand/`.

- `logo-mark-copper.svg`: primary brand mark.
- `logo-mark-paper.svg`: mark on dark backgrounds.
- `logo-mark-ink.svg`: mark on light backgrounds.
- `logo-mark-black.svg`: single-color fallback.
- `repowire-arch.webp`: warm architecture/product visual.

The generated `repowire-design-system/` bundle is a handoff reference, not a production dependency.

## Implementation Notes

- Tailwind v4 tokens are defined in `web/app/globals.css`.
- Keep semantic aliases such as `primary`, `secondary`, `surface-container-low`, and `outline` so existing dashboard code can migrate incrementally.
- Prefer `lucide-react` for new icons. Existing Material Symbols can remain where they already match dashboard chrome.
- Avoid introducing one-off colors in components. If a color repeats, promote it to a token.
