#!/bin/sh
# Offline Gateway auth contract fixtures (MAC-P2 / T32.6).
# Never connects to a live WEB gateway, Keychain, or local UDS backend.
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/onewifi-gateway-auth-contract.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

xcrun swiftc -parse-as-library \
  "$ROOT/app/Tests/GatewayAuthContract/main.swift" \
  -o "$TMP/gateway-auth-contract-tests"

"$TMP/gateway-auth-contract-tests"
