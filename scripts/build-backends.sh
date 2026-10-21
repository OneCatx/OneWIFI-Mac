#!/bin/sh
set -eu

SWITCH_SOURCE_ROOT="${PROJECT_DIR}"
. "${PROJECT_DIR}/scripts/workspace-env.sh"
cd "${PROJECT_DIR}/backend"
export CGO_ENABLED=1 GOOS=darwin

LIBUSB_PKG_CONFIG=""
if [ -n "${LIBUSB_DYLIB_ARM64:-}" ]; then
  LIBUSB_PKG_CONFIG="$(dirname "${LIBUSB_DYLIB_ARM64}")/pkgconfig"
fi
export PKG_CONFIG_PATH="${LIBUSB_PKG_CONFIG:+${LIBUSB_PKG_CONFIG}:}${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig}"
MACOS_MIN="${MACOSX_DEPLOYMENT_TARGET:-27.0}"
export CGO_CFLAGS="-mmacosx-version-min=${MACOS_MIN} ${CGO_CFLAGS:-}"
export CGO_LDFLAGS="-mmacosx-version-min=${MACOS_MIN} ${CGO_LDFLAGS:-}"

[ "${ARCHS}" = "arm64" ] || { echo "错误：Switch 仅构建 Apple Silicon 版本" >&2; exit 1; }
mkdir -p "${TARGET_TEMP_DIR}/backend"
GOARCH=arm64 go build -trimpath \
  -ldflags="-linkmode external -extldflags=-mmacosx-version-min=${MACOS_MIN} -s -w" \
  -o "${TARGET_TEMP_DIR}/backend/switchd" ./cmd/proletarius-macos

# Rust 是本机 API 网关，Go 保留硬件所有权；两者均为生产依赖。
CARGO_BIN="${SWITCH_CARGO_BIN:-$(command -v cargo)}"
[ -x "${CARGO_BIN}" ] || { echo "错误：未找到 Rust cargo (${CARGO_BIN})" >&2; exit 1; }
"${CARGO_BIN}" build --locked --release \
  --manifest-path "${PROJECT_DIR}/rust/Cargo.toml" \
  -p proletariusd

# 合并到 app 资源目录
BACKEND_DIR="${TARGET_BUILD_DIR}/${UNLOCALIZED_RESOURCES_FOLDER_PATH}/backend"
rm -rf "${BACKEND_DIR}"
mkdir -p "${BACKEND_DIR}"
cp "${TARGET_TEMP_DIR}/backend/switchd" "${BACKEND_DIR}/switchd"
cp "${CARGO_TARGET_DIR}/release/switchd-rs" "${BACKEND_DIR}/switchd-rs"
LIBUSB="${LIBUSB_DYLIB_ARM64:-/opt/homebrew/opt/libusb/lib/libusb-1.0.0.dylib}"
[ -f "$LIBUSB" ] || { echo "错误：未找到 arm64 libusb (${LIBUSB})，请设置 LIBUSB_DYLIB_ARM64" >&2; exit 1; }
cp "$LIBUSB" "${BACKEND_DIR}/libusb-1.0.0.dylib"
old=$(otool -L "${BACKEND_DIR}/switchd" 2>/dev/null | awk '/libusb/ {print $1; exit}')
if [ -n "$old" ]; then
  install_name_tool -change "$old" "@loader_path/libusb-1.0.0.dylib" "${BACKEND_DIR}/switchd"
fi
if otool -L "${BACKEND_DIR}/switchd" | awk '/libusb/ {print $1}' | grep -qv '^@loader_path/libusb-1.0.0.dylib$'; then
  echo "错误：后端仍引用开发机的 libusb 路径，拒绝打包。" >&2
  exit 1
fi

# Keep source-license notices with the redistributed runtime too.
NOTICES_DIR="${TARGET_BUILD_DIR}/${UNLOCALIZED_RESOURCES_FOLDER_PATH}/licenses"
mkdir -p "${NOTICES_DIR}"
cp "${PROJECT_DIR}/THIRD_PARTY_NOTICES.md" "${NOTICES_DIR}/THIRD_PARTY_NOTICES.md"
cp "${PROJECT_DIR}/LICENSE" "${NOTICES_DIR}/OneWIFI-LICENSE"
for dependency in euicc-go uicc-go quectel-qmi-go swu-go opencore-amr; do
  cp "${PROJECT_DIR}/backend/third_party/${dependency}/LICENSE" "${NOTICES_DIR}/${dependency}-LICENSE"
done
cp "${PROJECT_DIR}/backend/third_party/euicc-go/bertlv/LICENSE" "${NOTICES_DIR}/euicc-go-bertlv-LICENSE"
cp "${PROJECT_DIR}/backend/third_party/opencore-amr/opencore/NOTICE" "${NOTICES_DIR}/opencore-amr-NOTICE"
LIBUSB_LICENSE="${LIBUSB_LICENSE_FILE:-$(dirname "$LIBUSB")/../COPYING}"
[ -f "$LIBUSB_LICENSE" ] || { echo "错误：未找到 libusb 许可，请设置 LIBUSB_LICENSE_FILE。" >&2; exit 1; }
cp "$LIBUSB_LICENSE" "${NOTICES_DIR}/libusb-COPYING"

# 所有嵌套 Mach-O 在 lipo/install_name_tool 完成后统一进行 ad-hoc 签名。
for code in \
"${BACKEND_DIR}/libusb-1.0.0.dylib" \
"${BACKEND_DIR}/switchd" \
"${BACKEND_DIR}/switchd-rs"; do
  /usr/bin/codesign --force --sign - --timestamp=none "$code"
done

# 增量构建时 Xcode 可能跳过外层签名；重新封装资源签名，首次构建仍会由 Xcode 最终签名覆盖。
APP_BUNDLE="${TARGET_BUILD_DIR}/${WRAPPER_NAME}"
if [ -d "${APP_BUNDLE}" ]; then
  /usr/bin/codesign --force --sign - --timestamp=none "${APP_BUNDLE}"
fi
