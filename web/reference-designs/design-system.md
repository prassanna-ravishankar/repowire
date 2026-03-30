# Design System Specification: The Kinetic Mesh

## 1. Overview & Creative North Star
**Creative North Star: The Orchestration Engine**
This design system moves beyond the "SaaS Dashboard" trope to create a high-fidelity, technical environment that feels like a living command center. We reject the "flat web" aesthetic in favor of **Engineering Brutalism** -- a style defined by precision, high-density information, and tactical depth.

The experience is anchored by intentional asymmetry and "Floating Logic." Instead of centering everything, we lean into technical layouts where data determines the form. By using high-contrast typography scales and overlapping "glass" surfaces, we create an interface that feels like an advanced HUD (Heads-Up Display) rather than a website.

---

## 2. Color & Tonal Architecture
The palette is rooted in a "Deep Space" foundation, using the `primary` cyan to represent the pulse of the network.

### The "No-Line" Rule
**Explicit Instruction:** Designers are prohibited from using 1px solid, high-contrast borders to define sections. Layout boundaries must be established through:
1.  **Tonal Shifts:** Placing a `surface-container-low` component against a `surface` background.
2.  **Negative Space:** Using the Spacing Scale to create "Air Gaps" between functional groups.
3.  **Ghost Outlines:** If a boundary is required for legibility, use `outline-variant` at 15% opacity max.

### Surface Hierarchy & Nesting
We treat the UI as a series of physical layers. Hierarchy is dictated by the `surface-container` tokens:
*   **Base:** `surface` (#10131a) -- The infinite canvas.
*   **Sectioning:** `surface-container-low` (#191c22) -- Large grouping areas.
*   **Object Layer:** `surface-container-highest` (#32353c) -- Active interactive cards.
*   **Nesting:** To create depth, an inner card should always be one step higher or lower than its parent container to create a "recessed" or "elevated" feel without shadows.

### The Glass & Gradient Rule
Main CTAs and critical "Node" elements should utilize a linear gradient from `primary` (#c3f5ff) to `primary-container` (#00e5ff) at a 135deg angle. Floating panels should use **Glassmorphism**: `surface-variant` with a 20px Backdrop Blur and 60% opacity to let the background mesh patterns bleed through.

---

## 3. Typography: The Editorial Tech-Stack
We utilize a dual-font system to balance high-end editorial flair with developer-centric precision.

*   **Display & Headlines (`Space Grotesk`):** Used for high-level data points and section titles. Its wide apertures and geometric construction provide an "Aeronautical" feel.
*   **Body & Labels (`Inter`):** Used for all functional reading and data. It provides maximum legibility at small sizes (e.g., `label-sm` at 0.6875rem) for high-density AI logs.
*   **Code & Data (`JetBrains Mono`):** Used for monospaced content -- peer IDs, paths, tool calls, code blocks.

**Hierarchy as Identity:**
Use `display-lg` (3.5rem) for critical system stats to create an authoritative focal point. Contrast with `label-md` in all-caps with 0.05rem letter-spacing for metadata to reinforce the "engineered" aesthetic.

---

## 4. Elevation & Depth
In this system, depth is a tool for focus, not just decoration.

*   **Tonal Layering:** Avoid drop shadows for standard cards. Instead, stack `surface-container-lowest` (#0b0e14) on top of `surface-container-high` (#272a31) to create an "inset" terminal look.
*   **Ambient Shadows:** For floating Modals or Tooltips, use an ultra-diffused shadow: `0 24px 48px -12px rgba(0, 218, 243, 0.08)`. Note the use of the `primary` tint in the shadow to mimic the glow of a screen.
*   **The Mesh Grid:** Apply a background pattern using `outline-variant` at 5% opacity. This 24px grid acts as the "Floor" of the application, grounding all floating elements.

---

## 5. Components

### Primary Action Buttons
*   **Style:** Sharp `DEFAULT` (0.25rem) or `md` (0.375rem) corners.
*   **Fill:** Gradient of `primary` to `primary-fixed-dim`.
*   **Interaction:** On hover, apply a `primary` outer glow (4px spread, 15% opacity).

### High-Density Cards
*   **Constraint:** No borders. No dividers.
*   **Separation:** Use `surface-container-low` for the card body. Use a `surface-bright` header strip (2px height) at the very top of the card to indicate "Active" status.

### Status Indicators (The "Pulse")
*   **Online:** `secondary` (#d7ffc5) with a CSS animation "pulse" (1.5s infinite blur expansion).
*   **Busy:** `tertiary-fixed-dim` (#ffba38) with a static subtle outer glow.
*   **Offline:** `outline` (#849396) with no animation.

### Input Fields
*   **State:** Default state is `surface-container-lowest` with a "Ghost Border" (`outline-variant` at 20%).
*   **Focus:** Border transitions to `primary` (#c3f5ff) with a 2px inner glow. Use `JetBrains Mono` for input text to signify data entry.

### Bottom Navigation
*   Glass panel with backdrop blur, subtle top border in `cyan-900/20`.
*   Active tab: `cyan-400/10` background with `cyan-400` icon/text.
*   Inactive: `slate-500` with hover to `cyan-300`.

---

## 6. Do's and Don'ts

### Do
*   **DO** use intentional asymmetry. Align a large statistic to the far left and the supporting graph to the far right, leaving a "void" in the center.
*   **DO** use `secondary_fixed` for success states; it's our "Neon Green" that cuts through the dark background.
*   **DO** use the Spacing Scale religiously. Use `spacing-24` (5.5rem) for major section breathing room to maintain a "Premium" feel.

### Don't
*   **DON'T** use `full` (pill-shaped) rounding for buttons. It softens the "engineered" feel. Stick to `DEFAULT` (4px).
*   **DON'T** use pure white (#FFFFFF) for text. Always use `on-surface` (#e1e2eb) to prevent eye strain in dark mode.
*   **DON'T** use traditional horizontal rules (`<hr>`). Separate content using a `0.2rem` (spacing-1) height block of `surface-container-highest`.

---

## 7. Color Tokens

### Primary
| Token | Hex | Usage |
|-------|-----|-------|
| `primary` | #c3f5ff | Headlines, active elements, links |
| `primary-container` | #00e5ff | CTAs, gradient endpoints, accents |
| `primary-fixed` | #9cf0ff | Secondary accent, code highlights |
| `primary-fixed-dim` | #00daf3 | Gradient starts, progress bars |
| `on-primary` | #00363d | Text on primary backgrounds |
| `on-primary-container` | #00626e | Text on primary-container |
| `surface-tint` | #00daf3 | Ambient glow tint |

### Secondary (Neon Green)
| Token | Hex | Usage |
|-------|-----|-------|
| `secondary` | #d7ffc5 | Online status, success text |
| `secondary-container` | #2ff801 | Online indicators (bright) |
| `secondary-fixed` | #79ff5b | Success badges, active connections |
| `secondary-fixed-dim` | #2ae500 | Pulse animations |

### Tertiary (Amber)
| Token | Hex | Usage |
|-------|-----|-------|
| `tertiary` | #ffe9cd | Warm accent text |
| `tertiary-container` | #ffc769 | Warning badges |
| `tertiary-fixed-dim` | #ffba38 | Busy status, caution indicators |

### Error
| Token | Hex | Usage |
|-------|-----|-------|
| `error` | #ffb4ab | Error text, failed queries |
| `error-container` | #93000a | Error backgrounds |

### Surfaces
| Token | Hex | Usage |
|-------|-----|-------|
| `surface` | #10131a | Base canvas |
| `surface-dim` | #10131a | Same as surface |
| `surface-bright` | #363940 | Hover states, active strips |
| `surface-container-lowest` | #0b0e14 | Inset/recessed areas, code blocks |
| `surface-container-low` | #191c22 | Card bodies, sections |
| `surface-container` | #1d2026 | Mid-level containers |
| `surface-container-high` | #272a31 | Elevated cards, hover targets |
| `surface-container-highest` | #32353c | Badges, active interactive elements |
| `surface-variant` | #32353c | Glass panel base |

### Text & Outline
| Token | Hex | Usage |
|-------|-----|-------|
| `on-surface` | #e1e2eb | Primary text |
| `on-surface-variant` | #bac9cc | Secondary text, metadata |
| `on-background` | #e1e2eb | Text on background |
| `outline` | #849396 | Subtle borders, offline status |
| `outline-variant` | #3b494c | Ghost borders (use at 15-20% opacity) |

---

## 8. Tailwind Config

```javascript
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
  "on-primary-fixed": "#001f24",
  "on-primary-fixed-variant": "#004f58",
  "secondary": "#d7ffc5",
  "secondary-container": "#2ff801",
  "secondary-fixed": "#79ff5b",
  "secondary-fixed-dim": "#2ae500",
  "on-secondary": "#053900",
  "on-secondary-container": "#0f6d00",
  "on-secondary-fixed": "#022100",
  "on-secondary-fixed-variant": "#095300",
  "tertiary": "#ffe9cd",
  "tertiary-container": "#ffc769",
  "tertiary-fixed": "#ffdeac",
  "tertiary-fixed-dim": "#ffba38",
  "on-tertiary": "#432c00",
  "on-tertiary-container": "#775200",
  "on-tertiary-fixed": "#281900",
  "on-tertiary-fixed-variant": "#604100",
  "error": "#ffb4ab",
  "error-container": "#93000a",
  "on-error": "#690005",
  "on-error-container": "#ffdad6",
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

## 9. CSS Utilities

```css
/* Mesh grid background */
.mesh-bg {
  background-image: radial-gradient(circle at 2px 2px, rgba(59, 73, 76, 0.05) 1px, transparent 0);
  background-size: 24px 24px;
}

/* Glassmorphism panel */
.glass-panel {
  background: rgba(50, 53, 60, 0.6);
  backdrop-filter: blur(20px);
}

/* Online pulse animation */
.pulse-online {
  box-shadow: 0 0 0 0 rgba(121, 255, 91, 0.4);
  animation: pulse 1.5s infinite;
}
@keyframes pulse {
  0% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(121, 255, 91, 0.7); }
  70% { transform: scale(1); box-shadow: 0 0 0 10px rgba(121, 255, 91, 0); }
  100% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(121, 255, 91, 0); }
}

/* Custom scrollbar */
::-webkit-scrollbar { width: 4px; }
::-webkit-scrollbar-track { background: #10131a; }
::-webkit-scrollbar-thumb { background: #3b494c; border-radius: 2px; }
::-webkit-scrollbar-thumb:hover { background: #00e5ff; }
```
