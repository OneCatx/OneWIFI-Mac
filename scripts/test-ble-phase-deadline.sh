#!/bin/sh
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
BLE_TEST_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
BLE_TEST_BIN=$(mktemp "${TMPDIR}/switch-ble-deadline.XXXXXX")
trap 'rm -f "$BLE_TEST_BIN"' EXIT INT TERM
xcrun swiftc -parse-as-library \
  "$BLE_TEST_ROOT/app/Sources/BLEPhaseDeadline.swift" \
  "$BLE_TEST_ROOT/app/Tests/BLEPhaseDeadline/main.swift" \
  -o "$BLE_TEST_BIN"
"$BLE_TEST_BIN"
