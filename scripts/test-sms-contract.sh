#!/bin/sh
# Offline SMS contract fixtures (MAC-P4 / T34.6).
# Never connects to a live WEB gateway, Keychain, or local UDS backend.
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/onewifi-sms-contract.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

xcrun swiftc -parse-as-library \
  "$ROOT/app/Tests/SMSContract/main.swift" \
  -o "$TMP/sms-contract-tests"

"$TMP/sms-contract-tests"
