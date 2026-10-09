#!/usr/bin/env python3
"""Builds the design review sheet from exported ScreenshotTour attachments.

Rows are screens, columns are variants (light, dark, largetext), so drift in
one variant is visible at a glance. Usage: contact-sheet.py <attachments-dir> <out.html>
"""
import html
import json
import pathlib
import shutil
import sys

def main(src: str, out: str) -> int:
    src_dir, out_path = pathlib.Path(src), pathlib.Path(out)
    images = out_path.parent / "screens"
    images.mkdir(parents=True, exist_ok=True)
    manifest = json.loads((src_dir / "manifest.json").read_text())
    grid: dict[str, dict[str, str]] = {}
    variants: list[str] = []
    for test in manifest:
        for attachment in test.get("attachments", []):
            name = attachment.get("suggestedHumanReadableName", "").split("_")[0]
            if "--" not in name:
                continue
            variant, screen = name.split("--", 1)
            target = images / f"{variant}--{screen}.png"
            shutil.copy(src_dir / attachment["exportedFileName"], target)
            grid.setdefault(screen, {})[variant] = f"screens/{target.name}"
            if variant not in variants:
                variants.append(variant)
    rows = "".join(
        f"<tr><th>{html.escape(screen)}</th>" + "".join(
            f'<td><img src="{grid[screen][v]}" alt="{html.escape(screen)} {v}"></td>' if v in grid[screen] else "<td>missing</td>"
            for v in variants) + "</tr>"
        for screen in sorted(grid))
    head = "".join(f"<th>{html.escape(v)}</th>" for v in variants)
    out_path.write_text(f"""<!doctype html><meta charset="utf-8"><title>Repowire iOS design review</title>
<style>body{{font:14px -apple-system,system-ui;background:#fcfbfa;color:#141413;margin:24px}}
table{{border-collapse:separate;border-spacing:16px 12px}}th{{font:600 12px ui-monospace,monospace;text-transform:uppercase;letter-spacing:.06em;color:#4e4b46;text-align:left;vertical-align:top}}
img{{width:260px;border-radius:24px;border:1px solid #e8e5df;display:block}}</style>
<h1>Repowire iOS design review</h1><p>{len(grid)} screens × {len(variants)} variants</p>
<table><tr><th></th>{head}</tr>{rows}</table>""")
    print(f"contact sheet: {out_path} ({len(grid)} screens x {len(variants)} variants)")
    return 0 if grid else 1

if __name__ == "__main__":
    sys.exit(main(*sys.argv[1:3]))
