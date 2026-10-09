#!/usr/bin/env bash
# Live end-to-end check of the app against a real relay and daemon.
# Starts an isolated relay (:18080) and daemon (:18377) under a temporary HOME,
# so the machine's own daemon, config, and bots are untouched, then:
#   - registers a peer and a push device through the relay tunnel
#   - runs LiveRelayTests (question raised on the daemon, answered in the app)
#   - checks the daemon's push frame reached the relay (which reports that APNs
#     is not configured, unless REPOWIRE_RELAY_APNS_* are set)
set -euo pipefail
cd "$(dirname "$0")/.."

DEVICE="${IOS_SIM_DEVICE:-iPhone 18 Pro}"
WORK="$(mktemp -d)"
KEY="rw_e2e_$(date +%s)"
RELAY="http://127.0.0.1:18080"
PIDS=()
cleanup() { for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done; }
trap cleanup EXIT

echo "== building daemon into $WORK"
(cd ../daemon-go && go build -o "$WORK/repowire" .)

echo "== starting isolated relay and daemon"
HOME="$WORK" "$WORK/repowire" relay start --host 127.0.0.1 --port 18080 > "$WORK/relay.log" 2>&1 & PIDS+=($!)
sleep 1
HOME="$WORK" REPOWIRE_CONFIG="$WORK/none.yaml" "$WORK/repowire" serve --addr 127.0.0.1:18377 \
  --relay-enabled --relay-url ws://127.0.0.1:18080 --relay-api-key "$KEY" > "$WORK/daemon.log" 2>&1 & PIDS+=($!)
for _ in $(seq 20); do grep -q "relay: connected" "$WORK/daemon.log" && break; sleep 0.5; done
grep -q "relay: connected" "$WORK/daemon.log" || { cat "$WORK/daemon.log"; exit 1; }

AUTH="Authorization: Bearer $KEY"
curl -sf -H "Content-Type: application/json" -d '{"name":"e2e-worker","path":"/tmp/e2e","backend":"claude-code","circle":"e2e","role":"agent"}' http://127.0.0.1:18377/peers > /dev/null
curl -sf -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"token":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","environment":"sandbox","name":"e2e"}' \
  "$RELAY/push/devices" > /dev/null

echo "== LiveRelayTests on $DEVICE"
TEST_RUNNER_REPOWIRE_RELAY_URL="$RELAY" TEST_RUNNER_REPOWIRE_RELAY_KEY="$KEY" \
timeout 300 xcodebuild test -project Repowire.xcodeproj -scheme Repowire \
  -destination "platform=iOS Simulator,name=$DEVICE" -derivedDataPath build/DerivedData \
  -only-testing:RepowireUITests/LiveRelayTests 2>&1 | grep -E "Test Case .*(passed|failed|skipped)|error:|\*\* TEST" | tee "$WORK/test.log"
grep -q "TEST SUCCEEDED" "$WORK/test.log"

echo "== push pipeline"
if grep -q "relay: push failed: push is not configured" "$WORK/daemon.log"; then
  echo "daemon push frame reached the relay (APNs not configured here, as expected)"
elif grep -q "relay APNs: enabled" "$WORK/relay.log"; then
  echo "relay sent the push through APNs"
else
  echo "no push frame reached the relay"; tail -20 "$WORK/daemon.log"; exit 1
fi
