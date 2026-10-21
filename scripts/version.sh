#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
VERSION=$(sed 's/[[:space:]]*$//' "$ROOT/VERSION")
BUILD_NUMBER=$(sed 's/[[:space:]]*$//' "$ROOT/BUILD_NUMBER")

# Accept legacy Switch X.YY or OneWIFI marketing semver X.Y.Z
case "$VERSION" in
  [0-9]*.[0-9][0-9])
    MAJOR=${VERSION%%.*}
    case "$MAJOR" in
      ''|*[!0-9]*|0[0-9]*) echo "错误：VERSION 主版本无效：$VERSION" >&2; exit 1 ;;
    esac
    ;;
  [0-9]*.[0-9]*.[0-9]*)
    MAJOR=${VERSION%%.*}
    REST=${VERSION#*.}
    MINOR=${REST%%.*}
    PATCH=${REST#*.}
    case "$MAJOR" in
      ''|*[!0-9]*|0[0-9]*) echo "错误：VERSION 主版本无效：$VERSION" >&2; exit 1 ;;
    esac
    case "$MINOR" in
      ''|*[!0-9]*) echo "错误：VERSION 次版本无效：$VERSION" >&2; exit 1 ;;
    esac
    case "$PATCH" in
      ''|*[!0-9]*) echo "错误：VERSION 修订号无效：$VERSION" >&2; exit 1 ;;
    esac
    ;;
  *)
    echo "错误：VERSION 必须为 X.YY 或 X.Y.Z：$VERSION" >&2
    exit 1
    ;;
esac
case "$BUILD_NUMBER" in
  ''|*[!0-9]*|0*) echo "错误：BUILD_NUMBER 必须是无前导零的正整数" >&2; exit 1 ;;
esac

printf '%s %s\n' "$VERSION" "$BUILD_NUMBER"
