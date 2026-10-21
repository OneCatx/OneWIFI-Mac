#!/bin/sh
# Offline Calls control-plane contract fixtures (MAC-RP3 / T43.6).
# Never connects to a live WEB gateway, Keychain, local UDS, or WebRTC.
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/onewifi-calls-contract.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

xcrun swiftc -parse-as-library \
  "$ROOT/app/Tests/CallsContract/main.swift" \
  -o "$TMP/calls-contract-tests"

"$TMP/calls-contract-tests"
