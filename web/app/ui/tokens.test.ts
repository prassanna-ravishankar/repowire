import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// The design system's contrast floor, checked against the tokens as written in
// globals.css: every text role reads at 4.5:1 or better on every surface, in
// both themes, and so does the text on each solid fill and on each tint the
// screens use (a status colour over its own 10% wash).
//
// Blocks are found by their exact selectors. Reformatting globals.css makes
// this test fail loudly rather than pass on nothing; update block() with it.

const css = readFileSync(join(__dirname, "..", "globals.css"), "utf8");

function block(selector: string): Record<string, string> {
  const start = css.indexOf(selector);
  if (start < 0) throw new Error(`no ${selector} block in globals.css`);
  const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("\n}", start));
  const out: Record<string, string> = {};
  for (const [, name, value] of body.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/g)) out[name] = value.trim();
  return out;
}

const raw = block(":root {\n  /* Neutrals");
const themes = {
  light: { ...raw, ...block(':root,\n[data-theme="light"] {') },
  dark: { ...raw, ...block('[data-theme="dark"] {') },
};
type Theme = keyof typeof themes;

type RGBA = [number, number, number, number];
function resolve(theme: Theme, name: string): string {
  const value = themes[theme][name];
  if (!value) throw new Error(`${theme}: ${name} is not defined`);
  const ref = /^var\((--[a-z0-9-]+)\)$/.exec(value);
  return ref ? resolve(theme, ref[1]) : value;
}
function parse(value: string): RGBA {
  const hex = /^#([0-9a-f]{6})$/i.exec(value);
  if (hex) return [0, 2, 4].map((i) => parseInt(hex[1].slice(i, i + 2), 16)).concat(1) as RGBA;
  const rgba = /^rgba\((\d+),\s*(\d+),\s*(\d+),\s*([\d.]+)\)$/.exec(value);
  if (rgba) return [Number(rgba[1]), Number(rgba[2]), Number(rgba[3]), Number(rgba[4])];
  throw new Error(`unparsed colour ${value}`);
}
function over(top: RGBA, base: RGBA): RGBA {
  const a = top[3];
  return [0, 1, 2].map((i) => top[i] * a + base[i] * (1 - a)).concat(1) as RGBA;
}
function luminance([r, g, b]: RGBA) {
  const f = (c: number) => (c / 255 <= 0.03928 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4);
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
function contrast(a: RGBA, b: RGBA) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}
const color = (theme: Theme, name: string, base?: RGBA) => {
  const c = parse(resolve(theme, name));
  return base ? over(c, base) : c;
};
const wash = (theme: Theme, name: string, base: RGBA, amount: number): RGBA => {
  const c = parse(resolve(theme, name));
  return over([c[0], c[1], c[2], amount], base);
};

const SURFACES = ["--page", "--surface-sunken", "--surface", "--surface-raised", "--surface-overlay"];
const TEXT = ["--fg", "--fg-muted", "--fg-subtle", "--accent", "--success", "--warning", "--danger", "--info"];
const FILLS = [["--accent", "--on-accent"], ["--success", "--on-success"], ["--warning", "--on-warning"], ["--danger", "--on-danger"], ["--info", "--on-info"]];
const FLOOR = 4.5;

describe.each(["light", "dark"] as Theme[])("%s theme", (theme) => {
  const page = color(theme, "--page");
  for (const surface of SURFACES) {
    const ground = color(theme, surface, page);
    it.each(TEXT)(`%s reads on ${surface}`, (text) => {
      expect(contrast(color(theme, text, ground), ground)).toBeGreaterThanOrEqual(FLOOR);
    });
    it.each(TEXT.slice(3))(`%s reads on its own 10% wash over ${surface}`, (text) => {
      const tint = wash(theme, text, ground, 0.1);
      expect(contrast(color(theme, text, tint), tint)).toBeGreaterThanOrEqual(FLOOR);
    });
  }
  // A selected row: neutral text over the accent's 10% wash.
  for (const surface of SURFACES) {
    const ground = color(theme, surface, page);
    it.each(["--fg", "--fg-muted", "--fg-subtle"])(`%s reads on a selected row over ${surface}`, (text) => {
      const selected = wash(theme, "--accent", ground, 0.1);
      expect(contrast(color(theme, text, selected), selected)).toBeGreaterThanOrEqual(FLOOR);
    });
  }
  it.each(FILLS)("text on a solid %s fill reads", (fill, on) => {
    const ground = color(theme, fill, page);
    expect(contrast(color(theme, on, ground), ground)).toBeGreaterThanOrEqual(FLOOR);
  });
  // A notice on its signal's soft fill (a pending-question banner) keeps
  // its text in the signal colour.
  it.each(["--accent", "--success", "--warning", "--danger", "--info"])("%s reads on its own soft fill", (signal) => {
    const ground = color(theme, `${signal}-soft`, color(theme, "--surface", page));
    expect(contrast(color(theme, signal, ground), ground)).toBeGreaterThanOrEqual(FLOOR);
  });
  it("accent-soft-fg reads on accent-soft", () => {
    const ground = color(theme, "--accent-soft", color(theme, "--surface", page));
    expect(contrast(color(theme, "--accent-soft-fg", ground), ground)).toBeGreaterThanOrEqual(FLOOR);
  });
});

// fg-faint is for hairlines and disabled glyphs; as text it is about 2:1.
it("no screen sets text in the decorative faint tone", () => {
  const files: string[] = [];
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const path = join(dir, name);
      if (statSync(path).isDirectory()) walk(path);
      else if (/\.tsx?$/.test(name) && !name.endsWith(".test.ts")) files.push(path);
    }
  };
  walk(join(__dirname, ".."));
  const offenders = files.filter((file) => /\btext-(fg-faint|outline-variant)\b/.test(readFileSync(file, "utf8")));
  expect(offenders).toEqual([]);
});
