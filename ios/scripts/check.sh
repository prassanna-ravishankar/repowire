#!/usr/bin/env bash
# One command for the iOS quality gates:
#   1. function     RepowireKit unit tests + FlowTests UI tests on FixtureMesh
#   2. consistency  swift-format lint (style + safety rules) and design-lint
#   3. taste        ScreenshotTour -> build/design-review/index.html, scored
#                   against the rubric in docs/contributing/ios-app.md
# Usage: ios/scripts/check.sh [--fast]   (--fast skips the simulator steps)
set -euo pipefail
cd "$(dirname "$0")/.."

DEVICE="${IOS_SIM_DEVICE:-iPhone 18 Pro}"
OUT=build/design-review
RESULT=build/TestResults.xcresult

step() { printf '\n== %s\n' "$1"; }

step "RepowireKit unit tests"
(cd RepowireKit && swift test 2>&1 | tail -3)

step "swift-format lint"
swift format lint --strict -r Repowire RepowireKit/Sources RepowireKit/Tests RepowireUITests
echo "swift-format: clean"

step "Design lint"
./scripts/design-lint.py

if [[ "${1:-}" == "--fast" ]]; then
  echo "fast mode: skipped build, UI tests, and screenshots"
  exit 0
fi

step "UI tests, accessibility audit, and screenshot tour on $DEVICE"
rm -rf "$RESULT" "$OUT"
xcodebuild test -project Repowire.xcodeproj -scheme Repowire \
  -destination "platform=iOS Simulator,name=$DEVICE" \
  -derivedDataPath build/DerivedData -resultBundlePath "$RESULT" \
  SWIFT_TREAT_WARNINGS_AS_ERRORS=YES \
  2>&1 | grep -E "Test Case .*(passed|failed)|error:|\*\* TEST" || true
xcrun xcresulttool get test-results summary --path "$RESULT" --compact > build/summary.json
python3 - build/summary.json <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
print(f"{s['passedTests']} passed, {s['failedTests']} failed, {s['skippedTests']} skipped")
sys.exit(1 if s["failedTests"] or s["result"] != "Passed" else 0)
PY

step "Design review sheet"
mkdir -p "$OUT/raw"
xcrun xcresulttool export attachments --path "$RESULT" --output-path "$OUT/raw" >/dev/null
./scripts/contact-sheet.py "$OUT/raw" "$OUT/index.html"
for variant in light dark largetext; do
  swift scripts/montage.swift "$OUT/$variant.png" "$OUT"/screens/"$variant"--*.png
done
echo "montages: $OUT/{light,dark,largetext}.png"
