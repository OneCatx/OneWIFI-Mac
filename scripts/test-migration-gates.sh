#!/bin/sh
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

# macOS 27 SDK 的架构描述需要 Xcode toolchain；避免系统
# CommandLineTools 与 Xcode SDK 混用导致 Go 链接器误报。
if [ -z "${DEVELOPER_DIR:-}" ] && [ -d /Applications/Xcode.app/Contents/Developer ]; then
  export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
fi

echo "[rust] format and workspace tests"
(cd "$ROOT/rust" && cargo +1.98.0 fmt --all -- --check && cargo +1.98.0 test --workspace)

echo "[go] vet and compatibility tests"
(cd "$ROOT/backend" && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  go vet ./... && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  go test ./...)

echo "[parity] Rust-owned carrier template expansion consumed by Go runtime bridge"
CARRIER_REPLAY=$(mktemp "${TMPDIR}/switch-carrier-replay.XXXXXX")
trap 'rm -f "$CARRIER_REPLAY"' EXIT INT TERM
(cd "$ROOT/rust" && cargo +1.98.0 run --locked -p proletarius-vowifi --example carrier_replay -- "$CARRIER_REPLAY")
(cd "$ROOT/backend" && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  PROLETARIUS_CARRIER_REPLAY="$CARRIER_REPLAY" \
  go test ./internal/vowifi/carrier -run '^TestRustCarrierParameterReplay$' -count=1)

echo "[parity] Rust and live Go PCMA codecs"
(cd "$ROOT/rust" && cargo +1.98.0 build --locked -p proletarius-call --example pcma_fixture)
(cd "$ROOT/backend" && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  PROLETARIUS_PCMA_FIXTURE="$CARGO_TARGET_DIR/debug/examples/pcma_fixture" \
  go test ./internal/vowifi/imscore -run '^TestPCMARustParity$' -count=1)

echo "[parity] Rust and live Go IMS audio SDP negotiation"
(cd "$ROOT/rust" && cargo +1.98.0 build --locked -p proletarius-call --example sdp_fixture)
(cd "$ROOT/backend" && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  PROLETARIUS_SDP_FIXTURE="$CARGO_TARGET_DIR/debug/examples/sdp_fixture" \
  go test ./internal/vowifi/imscore -run '^TestSDPRustParity$' -count=1)

echo "[parity] Rust and live Go giffgaff AMR payload framing"
(cd "$ROOT/rust" && cargo +1.98.0 build --locked -p proletarius-call --example amr_fixture)
(cd "$ROOT/backend" && \
  CC="$(xcrun --find clang)" \
  SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" \
  PROLETARIUS_AMR_FIXTURE="$CARGO_TARGET_DIR/debug/examples/amr_fixture" \
  go test ./internal/vowifi/imscore -run '^TestAMRRustParity$' -count=1)

echo "migration gates passed"
