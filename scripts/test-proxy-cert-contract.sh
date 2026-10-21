#!/bin/sh
# Offline PROXY+CERT contract fixtures (MAC-P5 / T35.6).
# Never connects to a live WEB gateway, Keychain, or local UDS backend.
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/onewifi-proxy-cert-contract.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

xcrun swiftc -parse-as-library \
  "$ROOT/app/Tests/ProxyCertContract/main.swift" \
  -o "$TMP/proxy-cert-contract-tests"

"$TMP/proxy-cert-contract-tests"
