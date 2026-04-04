# Design System: The Kinetic Mesh

## 1. Overview

Engineering Brutalism / HUD aesthetic. Precision, high-density information, tactical depth. Dark-only, mobile-first, responsive to desktop.

---

## 2. Color & Tonal Architecture

Palette rooted in "Deep Space" foundation. Cyan primary represents the network pulse.

### No-Line Rule
No 1px solid borders for sectioning. Boundaries via:
1. **Tonal Shifts:** `surface-container-low` against `surface` background.
2. **Negative Space:** Spacing scale for "Air Gaps" between groups.
3. **Ghost Outlines:** `outline-variant` at 15% opacity max, only when needed for legibility.

### Surface Hierarchy
| Token | Hex | Usage |
|-------|-----|-------|
| `surface` | #10131a | Base canvas |
| `surface-container-lowest` | #0b0e14 | Inset/recessed areas, code blocks |
| `surface-container-low` | #191c22 | Card bodies, sections, side rail |
| `surface-container` | #1d2026 | Mid-level containers |
| `surface-container-high` | #272a31 | Elevated cards, hover targets |
| `surface-container-highest` | #32353c | Badges, active interactive elements |
| `surface-bright` | #363940 | Hover states, active strips |

### Glass & Gradient Rule
CTAs use linear gradient from `primary` (#c3f5ff) to `primary-container` (#00e5ff) at 135deg. Floating panels use glassmorphism: `surface-variant` at 60% opacity with 20px backdrop blur.

---

## 3. Typography

| Family | Variable | Usage |
|--------|----------|-------|
| Space Grotesk | `font-headline` | Headlines, section titles, nav labels |
| Inter | `font-body` | Body text, labels, metadata |
| JetBrains Mono | `font-mono` | Peer IDs, paths, tool calls, code, inputs |

Headlines use `display-lg` (3.5rem) for stats. Labels use all-caps with `tracking-widest` at 10px for the engineered aesthetic.

---

## 4. Responsive Layout

### Mobile (`< md`, 390px target)
- **Top bar:** Fixed, logo + "REPOWIRE" + connection badge
- **Bottom tabs:** Glass panel with 3 tabs (Dash/Logs/Config), `pb-6` for iOS safe area
- **Content:** Single column, `max-w-2xl`, `pt-[68px] pb-24`
- **Peer cards:** Single column grid

### Desktop (`md:` and up)
- **Side rail:** Fixed left, `w-64`, `bg-surface-container-low`, `border-r border-outline-variant/15`
  - Logo + "REPOWIRE" at top
  - Vertical nav items (same 3 tabs) with active state: `text-cyan-400 bg-surface-container-highest border-r-2 border-primary`
  - "Deploy New Node" spawn button at bottom (primary gradient)
- **Top bar:** Spans `left-64` to right edge, `bg-surface/80 backdrop-blur-md`
  - Circle filter tabs (when multiple circles exist): "All Circles" + per-circle tabs
  - Connection badge + refresh button on right
- **Content:** `md:pl-64 md:pb-0`, wider max-widths (`md:max-w-5xl` for grid, `md:max-w-4xl` for feed/settings)
- **Peer cards:** `md:grid-cols-2 lg:grid-cols-3` with `md:hover:translate-y-[-4px]` lift effect
- **Bottom tabs:** Hidden on desktop (`md:hidden`)

---

## 5. Components

### Peer Cards
- `bg-surface-container-low` body, `overflow-hidden`
- 2px status-colored top strip (`statusTopStrip()`)
- 4px left border at 20% opacity (`statusBorderColor()`)
- Name in `font-headline text-lg font-bold`, truncated
- Status dot with pulse/glow utilities (`statusDot()`)
- Circle badge: `bg-surface-container-highest font-mono text-[10px] uppercase`
- Role badge: shown only for non-agent roles via `<RoleBadge>` component
- Path via `shortPath()`: parent truncates, folder name always visible

### Status Indicators
| Status | Dot class | Text class | Animation |
|--------|-----------|------------|-----------|
| Online | `bg-secondary pulse-online` | `text-secondary` | 1.5s pulse ring |
| Busy | `bg-tertiary-fixed-dim glow-busy` | `text-tertiary-fixed-dim` | Static glow |
| Offline | `bg-outline` | `text-outline` | None |

### Role Badges
Only shown when role is not "agent" (the default).
| Role | Badge class |
|------|-------------|
| service | `bg-primary/10 text-primary` |
| orchestrator | `bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim` |
| human | `bg-secondary/10 text-secondary` |

Service-role peers are hidden from the peer grid and shown in Settings > Integrations.

### Primary Action Buttons
- Sharp corners: `DEFAULT` (0.125rem)
- Fill: gradient `from-primary to-primary-container`
- Hover: `brightness-110`, active: `scale-[0.98]`

### Input Fields
- Background: `bg-surface-container-lowest`
- Border: `border-outline-variant/20` (ghost border)
- Focus: `focus:border-primary focus:ring-1 focus:ring-primary`
- Text: `font-mono`

### Compose Bar
- Glass panel wrapper with backdrop blur
- Mode toggle: pill buttons (cyan active, slate inactive)
- Send button: primary gradient with cyan shadow

### Bottom Navigation (Mobile)
- `bg-surface/80 backdrop-blur-xl border-t border-cyan-900/20`
- Active tab: `bg-cyan-400/10 text-cyan-400 rounded-lg`, filled icon
- Inactive: `text-slate-500 hover:text-cyan-300`
- Labels: `font-body text-[10px] uppercase tracking-widest`

---

## 6. Do's and Don'ts

### Do
- Use `secondary-fixed` for success states (neon green cuts through dark)
- Use generous spacing (`spacing-24` / 5.5rem) for section breathing room
- Use `font-mono` for any string that looks like an ID, path, or code
- Show the folder name prominently in paths (via `shortPath()`)

### Don't
- Use pill-shaped (`full`) rounding for buttons -- stick to `DEFAULT` (4px)
- Use pure white (#FFFFFF) -- always `on-surface` (#e1e2eb)
- Use `<hr>` -- separate with tonal shifts or 2px `surface-container-highest` blocks
- Show hardcoded placeholder text for peer descriptions -- show nothing if empty

---

## 7. Color Tokens (Full)

```javascript
// Tailwind @theme inline config
colors: {
  "surface": "#10131a",
  "surface-dim": "#10131a",
  "surface-bright": "#363940",
  "surface-container-lowest": "#0b0e14",
  "surface-container-low": "#191c22",
  "surface-container": "#1d2026",
  "surface-container-high": "#272a31",
  "surface-container-highest": "#32353c",
  "surface-variant": "#32353c",
  "surface-tint": "#00daf3",
  "background": "#10131a",
  "on-surface": "#e1e2eb",
  "on-surface-variant": "#bac9cc",
  "on-background": "#e1e2eb",
  "primary": "#c3f5ff",
  "primary-container": "#00e5ff",
  "primary-fixed": "#9cf0ff",
  "primary-fixed-dim": "#00daf3",
  "on-primary": "#00363d",
  "on-primary-container": "#00626e",
  "secondary": "#d7ffc5",
  "secondary-container": "#2ff801",
  "secondary-fixed": "#79ff5b",
  "secondary-fixed-dim": "#2ae500",
  "on-secondary": "#053900",
  "tertiary": "#ffe9cd",
  "tertiary-container": "#ffc769",
  "tertiary-fixed": "#ffdeac",
  "tertiary-fixed-dim": "#ffba38",
  "on-tertiary": "#432c00",
  "error": "#ffb4ab",
  "error-container": "#93000a",
  "on-error": "#690005",
  "outline": "#849396",
  "outline-variant": "#3b494c",
  "inverse-surface": "#e1e2eb",
  "inverse-on-surface": "#2e3037",
  "inverse-primary": "#006875",
},
fontFamily: {
  "headline": ["Space Grotesk"],
  "body": ["Inter"],
  "label": ["Inter"],
  "mono": ["JetBrains Mono"],
},
borderRadius: {
  "DEFAULT": "0.125rem",
  "lg": "0.25rem",
  "xl": "0.5rem",
  "full": "0.75rem",
},
```

---

## 8. CSS Utilities

```css
.mesh-bg {
  background-image: radial-gradient(circle at 2px 2px, rgba(59, 73, 76, 0.05) 1px, transparent 0);
  background-size: 24px 24px;
}

.glass-panel {
  background: rgba(50, 53, 60, 0.6);
  backdrop-filter: blur(20px);
}

.pulse-online {
  box-shadow: 0 0 0 0 rgba(121, 255, 91, 0.4);
  animation: pulse-ring 1.5s infinite;
}

.glow-busy {
  box-shadow: 0 0 8px rgba(255, 186, 56, 0.5);
}

::-webkit-scrollbar { width: 4px; }
::-webkit-scrollbar-track { background: #10131a; }
::-webkit-scrollbar-thumb { background: #3b494c; border-radius: 2px; }
::-webkit-scrollbar-thumb:hover { background: #00e5ff; }
```
