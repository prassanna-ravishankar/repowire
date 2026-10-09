#!/usr/bin/env python3
"""Design-consistency lint for the iOS app.

Feature code must take color, space, radius, type, and motion from
`Repowire/Design/Theme.swift`, and its copy must follow the Repowire voice
(sentence case, no emoji, no exclamation marks). Exits non-zero on findings.
Usage: scripts/design-lint.py [paths...]   (default: Repowire/Features)
"""
import pathlib
import re
import sys

RULES = [
    ("raw-color", re.compile(r"Color\((red|hue|white|\.sRGB|#)|UIColor\(|Color\.(white|red|blue|green|orange|yellow|purple|pink|gray|grey|black|brown|cyan|mint|teal|indigo)\b|(\.foregroundStyle|\.tint)\(\.(white|red|blue|green|orange|yellow|purple|pink|gray|black)\b"),
     "use a Theme.Palette token"),
    ("raw-space", re.compile(r"(\.padding\((\.\w+, )?|spacing: |cornerRadius: |\bwidth: |\bheight: |minHeight: |maxHeight: |lineWidth: )([1-9]\d*(\.\d+)?)\b"),
     "use Theme.Space / Theme.Radius / Theme.Size / Theme.Stroke"),
    ("raw-font", re.compile(r"\.font\(\.system\(size:|Font\.custom|\.custom\(\""), "use a text style or Theme.Typeface"),
    ("raw-motion", re.compile(r"(\.animation\(|withAnimation\()\.(easeIn|easeOut|easeInOut|linear|spring|bouncy|snappy|smooth|interpolatingSpring)"),
     "use Theme.Motion"),
    ("emoji", re.compile(r'"[^"\n]*[\U0001F300-\U0001FAFF☀-➿][^"\n]*"'), "no emoji in product chrome"),
    ("exclamation", re.compile(r'(Text|Label|Button|navigationTitle)\(\s*"[^"\n]*!"'), "no exclamation marks"),
    ("title-case", re.compile(r'(navigationTitle|Button|Label|Text|Eyebrow\(text:)\(?\s*"[A-Z][a-z]+ [A-Z][a-z]+'),
     "sentence case: only the first word is capitalized"),
]

def lint(path: pathlib.Path):
    for number, line in enumerate(path.read_text().splitlines(), 1):
        if line.lstrip().startswith("//"):
            continue
        for name, pattern, hint in RULES:
            if pattern.search(line):
                yield f"{path}:{number}: [{name}] {hint}\n    {line.strip()}"

def main(argv):
    roots = [pathlib.Path(p) for p in argv] or [pathlib.Path("Repowire/Features")]
    files = sorted(f for root in roots for f in ([root] if root.is_file() else root.rglob("*.swift")))
    findings = [finding for f in files for finding in lint(f)]
    print("\n".join(findings) if findings else f"design-lint: {len(files)} files clean")
    return 1 if findings else 0

if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
