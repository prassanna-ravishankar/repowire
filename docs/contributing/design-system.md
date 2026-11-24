# Design system

Repowire's product surfaces share one **Repowire Design System** (RDS): the dashboard, the managed-agent views, admin, and the account portal. It is light-first, with warm-paper neutrals, a single cobalt accent, Inter for body and prose, and JetBrains Mono for the technical chrome (labels, ids, paths, timestamps, the live feed). These docs use the same cobalt and neutrals. The marketing landing page is a separate brand surface with its own drafting-sheet palette (below).

The system has two halves:
- **Tokens:** `web/app/globals.css`, one semantic layer in both themes.
- **Components:** `web/app/ui/`.

A surface imports from `app/ui` and uses the token-backed Tailwind names; it does not restyle its own buttons or dialogs. The `/design` page renders every token and component in the current theme, with contrast measured live. Use it for screenshot review. It ships in the public web export: it is not linked and asks search engines not to index it, but anyone with the URL can open it, so keep it free of anything private.

## Voice

- Write builder-to-builder copy: calm, specific, short technical sentences over marketing language.
- Use second person for the reader (you) and third person for the product (Repowire).
- Write copy in sentence case: page headings, prose, nav, messages, and the text you type into a component (a button's `children`, a dialog's `title`).
- The product chrome renders short labels in uppercase mono with wide tracking: buttons, field labels, status words, dialog and panel titles. The components apply that styling, so the copy stays in sentence case ("New agent" renders as NEW AGENT). Never uppercase a paragraph, and never put a label above a page heading.
- Use mono for code, identifiers, paths, commands and inline literals (`ask`, `notify_peer`, `port 8377`), and for the product chrome above. Page headings are never mono.
- No emoji in product chrome. User-authored content can contain anything the user wrote.
- No exclamation marks, no superlatives. Prefer "Local by default" to "Blazing fast".

Common patterns:

| Pattern | Example |
| --- | --- |
| Inline command | `repowire up` |
| Route in prose | `dashboard → backend` (UTF-8 arrow) |
| Peer reference | `@backend` (mono, no special color) |
| Status label | "Online" (sentence case, paired with a status dot) |

## Color

Light-first warm paper with a single cobalt accent. Most of the surface is neutral; cobalt is for links, primary actions, selection and focus only. Status colours are not brand colours.

The palette is defined once, as semantic tokens scoped by `data-theme` on `<html>`. Tailwind's colour names point at them (`@theme inline`), so every utility follows the theme, alpha modifiers included: `bg-accent/10` compiles to `color-mix(in oklab, var(--accent) 10%, transparent)`.

| Role | Token | Tailwind | Light | Dark |
| --- | --- | --- | --- | --- |
| Page | `--page` | `bg-page` | `#fcfbfa` | `#0e0e0d` |
| Sunken (inputs, wells) | `--surface-sunken` | `bg-sunken` | `#f7f6f3` | `#0a0a09` |
| Surface (cards, panes) | `--surface` | `bg-surface` | `#ffffff` | `#161614` |
| Raised (hover, selected) | `--surface-raised` | `bg-raised` | `#ffffff` | `#1c1c1a` |
| Overlay (toasts, menus) | `--surface-overlay` | `bg-overlay` | `#ffffff` | `#232220` |
| Text | `--fg` | `text-fg` | `#141413` | `#f3f1ec` |
| Secondary text | `--fg-muted` | `text-fg-muted` | `#4e4b46` | `#b5b1a8` |
| Tertiary text, placeholders | `--fg-subtle` | `text-fg-subtle` | `#67635c` | `#9a968e` |
| Hairlines, disabled glyphs only | `--fg-faint` | `bg-fg-faint` | `#b7b3aa` | `#5b5853` |
| Accent | `--accent` | `text-accent`, `bg-accent` | `#2c54dd` | `#7590ec` |
| Text on a solid fill | `--on-accent`, `--on-success`, … | `text-on-accent` | white | `#0e0e0d` |
| Success / warning / danger / info | `--success` … | `text-success` … | `#1a7349` / `#925810` / `#b83528` / `#2563c9` | `#4eb37e` / `#e0b14a` / `#e57368` / `#6ea4f0` |
| Soft fills (badges, banners) | `--accent-soft`, `--success-soft`, … | `bg-success-soft` | opaque tints | opaque tints |
| Borders | `--rds-border-strong`, `--rds-border` | `border-border`, `border-border-faint` | 14% / 8% ink | 14% / 8% paper |

**Contrast floor.** Every text role reads at 4.5:1 or better:
- on every surface, in both themes;
- on its own 10% wash (`text-success` on `bg-success/10`), and its own soft fill;
- for neutral text, on a selected row (the accent's 10% wash);
- as the text on its solid fill.

That is why the dark theme puts dark text on its bright fills, and why the light status colours are the 600 shades. `web/app/ui/tokens.test.ts` computes every pair from `globals.css` and fails CI on one below the floor. It also fails if any screen sets text in `--fg-faint`, which is about 2:1 and decorative only.

Text sits on a surface, a soft fill or a single wash, never on a wash layered over a fill. Signal-coloured text sits on at most a 10% wash of its colour: at 15% several signals fall below the floor. So a hover deepens the border, not the wash. A control inside a tinted banner is a solid `bg-surface` chip with signal-coloured text and a signal border. Status badges use the opaque soft fill (`bg-success-soft text-success`), not a wash, so they read the same on a selected row as anywhere else.

Rules:
- The page is warm paper, not pure white. White is for cards, so they sit slightly off the page.
- One accent only. Don't introduce a second brand hue.
- Amber (`warning`) means busy, degraded, or a caveat about a specific agent, never a generic action.
- Colour is never the only signal: a status dot comes with a word.
- CTAs are solid cobalt, no gradients.
- Reach for type, scale and space before reaching for colour.

**Earlier names.** The dashboard was written against Material-style names (`surface-container-high`, `on-surface-variant`, `outline`, `primary`, `secondary`, `tertiary`, `error`, …). They remain as aliases of the semantic tokens, so nothing breaks, and they are on their way out. New code uses the names in the table, and screens move over as they are touched.

## Type

| Family | Usage |
| --- | --- |
| Inter (400/500/600/700) | Body, prose, headings, form values |
| JetBrains Mono (400/500/700) | Product chrome: labels, buttons, status words, ids, paths, timestamps, the live feed; code everywhere |

Product labels are uppercase mono at 10px or larger with wide tracking. Body text is never below 12px. Inter loads via `next/font/google` and JetBrains Mono via `next/font/local`; the docs site loads Inter through mkdocs-material's `font.text` setting.

Scale (responsive via `clamp()` where it matters): `--fs-display` 40–64px (hero only), `--fs-h1` 32–44px, `--fs-h2` 24–32px, down to `--fs-body` 16px and `--fs-eyebrow` 12px. Tabular numerals on counts and timestamps; tight tracking on display and headings; body line-height 1.55.

## Space, radii, motion

- 4px grid. Marketing sections are airy (64–96px vertical). Container width is 1200px (`--container-xl`).
- Radii default to 10px (`--rds-radius`); cards 10–14px; large media 20px. Pills (`999px`) only for status badges and toggles.
- Borders are a single hairline, `1px solid var(--rds-border)`.
- Shadows are a soft family (`--shadow-xs`…`--shadow-xl`). Cards rest flat or at `--shadow-xs` and bump one tier on hover.
- Motion: `--rds-ease` `cubic-bezier(0.32, 0.72, 0, 1)`, 120ms for color/opacity, 200ms for layout. A soft `translateY(-1px)` card-hover lift and the online status-dot pulse are allowed. No scale on cards, no spring easings, no scroll-triggered staggers.
- Focus: 2px cobalt ring with a 2px page offset (`--ring`).

## Components

`web/app/ui/` holds the product components. Import them from `app/ui`:

| Component | Use |
| --- | --- |
| `Button`, `IconButton`, `buttonClass` | `primary` (one per screen), `secondary`, `ghost`, `danger`; `md` and `icon` sizes. 44px tall on touch layouts, compact from `md` up. |
| `inputClass`, `labelClass` | Text fields (16px on touch, so iOS does not zoom) and the mono field label. |
| `Modal` | A native `<dialog>`: a centred dialog on desktop, a bottom sheet under 640px. The page behind it is inert, focus stays inside, and Escape or a backdrop click closes it. Focus returns to the opener. |
| `Dot`, `StatusText`, `StatusBadge` | Status by `tone`: `success`, `warning`, `danger`, `accent` or `neutral`. Only a live connection pulses. |
| `InlineError`, `Banner` | A failure shown verbatim, with an optional retry; a page-wide notice under the top bar. |
| `EmptyState`, `SkeletonRows`, `PageMessage` | Empty, loading, and whole-page states such as signed out or not configured. |
| `useToasts`, `Toasts` | At most two transient confirmations. |
| `Brand`, `CopyId`, `ThemeToggle` | The top-bar wordmark, a copy-on-click id, and the light/dark switch. |

A new pattern that two surfaces need goes into `app/ui`, not into one of them.

## Craft floor

Judge UI work by rendered screenshots, at desktop and at 390px wide, in both themes, before calling it done. Release blockers:
- text below 4.5:1 (the token test covers the palette; check one-off colours by hand);
- interactive controls under 44px on mobile;
- core controls that are not reachable without scrolling;
- eyebrow labels above headings.

## Theming

- Light-first. The marketing site defaults to light and offers a dark toggle in the top bar; the choice persists to `localStorage["repowire-theme"]` and falls back to the OS `prefers-color-scheme`. A small inline script in `web/app/layout.tsx` sets `data-theme` before first paint to avoid a flash.
- The active theme is selected by `data-theme="light"` / `data-theme="dark"` on `<html>`. RDS semantic tokens are scoped to those attributes.
- The docs site uses mkdocs-material's built-in light/dark palette toggle, themed in `docs/stylesheets/repowire-cobalt.css`.

## Iconography

Lucide, 1.5–1.75px stroke. In the marketing app, use `lucide-react`. Brand glyphs Lucide no longer ships (e.g. GitHub) are inlined as small SVG components. The brand mark is `logo-mark.svg`. No emoji and no unicode glyphs as UI icons; real arrows in prose (`dashboard → backend`) are fine.

## Assets

Production web assets live under `web/public/brand/`; docs assets under `docs/assets/`.

- `logo-mark.svg`: primary brand mark (used in marketing nav/footer and the docs logo).
- `repowire-arch.webp`: architecture/product visual.

The generated `repowire-design-system/` handoff bundle is a reference, not a production dependency.

## Implementation notes

- The RDS tokens live in `web/app/globals.css`: a raw palette (warm neutrals, cobalt, signals), then `data-theme`-scoped semantic tokens (`--page`, `--surface`, `--fg`, `--accent`, `--on-accent`, `--rds-border`, `--shadow-*`, `--ring`), then the Tailwind colour names that point at them. There is one definition per theme and no second copy.
- The marketing landing page keeps its own drafting-sheet palette (cool paper, ink, the cobalt "signal", Geist), scoped under `.rw-marketing` in `web/app/marketing.css`, so it never reaches the product surfaces.
- Font tokens `--rds-font-sans` / `--rds-font-mono` are defined on `.rw-marketing` (not `:root`) because they reference the `next/font` variables set on `<body>`.
- Prefer `lucide-react` for new icons. Avoid one-off colors; if a color repeats, promote it to a token.
- The docs stylesheet mirrors these tokens by hand; keep `docs/stylesheets/repowire-cobalt.css` in sync with `globals.css`.

## Dashboard

The hosted relay dashboard (`web/app/dashboard/`) is on the design system. It inherits `data-theme` from the root layout, so it follows the same theme toggle as every other surface. It takes its empty states and theme toggle from `app/ui`, and its remaining screens move from the earlier colour names to the semantic ones as they are touched. Status semantics stay constant across surfaces:
- online: success (green);
- busy: warning (amber);
- offline: muted;
- error: danger (red);
- active: cobalt.
