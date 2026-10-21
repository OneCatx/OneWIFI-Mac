#!/bin/sh
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"
TEST_DIR=$(mktemp -d "$TMPDIR/switch-multistandby-tests.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT INT TERM
export CFFIXED_USER_HOME="$SWITCH_BUILD_DIR/test-home"
mkdir -p "$CFFIXED_USER_HOME"
cd "$SWITCH_SOURCE_ROOT"
xcrun swiftc -parse-as-library \
  app/Sources/MultiStandbyModels.swift app/Sources/MultiStandbyStore.swift \
  app/Sources/APIClient.swift app/Sources/BackendProcess.swift \
  app/Sources/UnixSocketURLProtocol.swift app/Sources/LocalBackendAuthentication.swift \
  app/Sources/AppPreferences.swift app/Tests/MultiStandby/main.swift \
  -o "$TEST_DIR/multistandby-tests"
"$TEST_DIR/multistandby-tests"
