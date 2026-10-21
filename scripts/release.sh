#!/bin/sh
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
set -- $("$ROOT/scripts/version.sh")
VERSION=$1
BUILD_NUMBER=$2

BUILD_ROOT=$(mktemp -d "${TMPDIR}/onewifi-release.XXXXXX")
STAGING_DIR=
ARCHIVE_TEMP=
DIST_PUBLISHED=0
ARCHIVE_PUBLISHED=0
COMPLETED=0
same_file() {
  [ -f "$1" ] && [ -f "$2" ] &&
    [ "$(stat -f '%d:%i' "$1")" = "$(stat -f '%d:%i' "$2")" ]
}
cleanup() {
  if [ "$COMPLETED" -eq 0 ]; then
    if [ "$ARCHIVE_PUBLISHED" -eq 1 ] && same_file "$ARCHIVE_DMG" "$ARCHIVE_TEMP"; then
      rm -f "$ARCHIVE_DMG"
    fi
    if [ "$DIST_PUBLISHED" -eq 1 ] && same_file "$DMG" "$STAGED_DMG"; then
      rm -f "$DMG"
    fi
  fi
  [ -z "$ARCHIVE_TEMP" ] || rm -f "$ARCHIVE_TEMP"
  [ -z "$STAGING_DIR" ] || rm -rf "$STAGING_DIR"
  rm -rf "$BUILD_ROOT"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

APP="$BUILD_ROOT/Build/Products/Release/OneWIFI.app"
DMG="$SWITCH_DIST_DIR/OneWIFI-$VERSION-macOS27.dmg"
ARCHIVE_DIR="$SWITCH_DIST_DIR/archive"
ARCHIVE_DMG="$ARCHIVE_DIR/OneWIFI-$VERSION-macOS27.dmg"

for artifact_path in "$DMG" "$ARCHIVE_DMG"; do
  if [ -e "$artifact_path" ] || [ -L "$artifact_path" ]; then
    echo "错误：发行文件已存在，拒绝覆盖：$artifact_path" >&2
    exit 1
  fi
done

mkdir -p "$SWITCH_DIST_DIR"
STAGING_DIR=$(mktemp -d "$SWITCH_DIST_DIR/.OneWIFI-$VERSION-release.XXXXXX")
STAGED_DMG="$STAGING_DIR/$(basename "$DMG")"
STAGED_MANIFEST="$STAGING_DIR/release-manifest.json"
cd "$ROOT"
xcodegen generate --spec project.yml
xcodebuild -project OneWIFI.xcodeproj -scheme OneWIFI \
  -configuration Release -destination 'platform=macOS' \
  -derivedDataPath "$BUILD_ROOT" ARCHS=arm64 MACOSX_DEPLOYMENT_TARGET=27.0 \
  MARKETING_VERSION="$VERSION" CURRENT_PROJECT_VERSION="$BUILD_NUMBER" \
  PRODUCT_BUNDLE_IDENTIFIER=com.oneims.onewifi.mac build
"$ROOT/scripts/sign-release-app.sh" "$APP"
"$ROOT/scripts/package-dmg.sh" "$APP" "$STAGED_DMG" "OneWIFI"
if [ -n "${SWITCH_NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$STAGED_DMG" --keychain-profile "$SWITCH_NOTARY_PROFILE" --wait
  xcrun stapler staple "$STAGED_DMG"
  hdiutil verify "$STAGED_DMG"
fi
"$ROOT/scripts/generate-release-manifest.sh" "$STAGED_MANIFEST" "$STAGED_DMG"

mkdir -p "$ARCHIVE_DIR"
ARCHIVE_TEMP=$(mktemp "$ARCHIVE_DIR/.OneWIFI-$VERSION-macOS27.dmg.XXXXXX")
cp -p "$STAGED_DMG" "$ARCHIVE_TEMP"
source_sha=$(shasum -a 256 "$STAGED_DMG" | awk '{print $1}')
archive_sha=$(shasum -a 256 "$ARCHIVE_TEMP" | awk '{print $1}')
if [ "$source_sha" != "$archive_sha" ]; then
  echo "错误：本地归档 SHA-256 校验失败：$ARCHIVE_DMG" >&2
  exit 1
fi
ln -n "$STAGED_DMG" "$DMG"
DIST_PUBLISHED=1
ln -n "$ARCHIVE_TEMP" "$ARCHIVE_DMG"
ARCHIVE_PUBLISHED=1
mv "$STAGED_MANIFEST" "$SWITCH_DIST_DIR/release-manifest.json"
COMPLETED=1
echo "发行完成：$DMG"
echo "本地归档：$ARCHIVE_DMG (SHA-256 $source_sha)"
