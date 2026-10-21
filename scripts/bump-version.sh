#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CURRENT=$(tr -d '[:space:]' < "$ROOT/VERSION")
BUILD=$(tr -d '[:space:]' < "$ROOT/BUILD_NUMBER")
NEXT=${1:-}

if [ -z "$NEXT" ]; then
  case "$CURRENT" in
    [0-9]*.[0-9]*.[0-9]*)
      MAJOR=${CURRENT%%.*}
      REST=${CURRENT#*.}
      MINOR=${REST%%.*}
      PATCH=${REST#*.}
      NEXT=$(printf '%s.%s.%s' "$MAJOR" "$MINOR" "$((PATCH + 1))")
      ;;
    [0-9]*.[0-9][0-9])
      MAJOR=${CURRENT%%.*}
      MINOR=$((1${CURRENT#*.} - 100 + 1))
      if [ "$MINOR" -ge 100 ]; then
        MAJOR=$((MAJOR + 1))
        MINOR=0
      fi
      NEXT=$(printf '%s.%02d' "$MAJOR" "$MINOR")
      ;;
    *)
      echo "无法从当前 VERSION 自动递增：$CURRENT" >&2
      exit 1
      ;;
  esac
fi
case "$NEXT" in
  [0-9]*.[0-9]*.[0-9]*)
    ;;
  [0-9]*.[0-9][0-9])
    ;;
  *)
    echo "用法：$0 [例如 0.1.1 或 0.03]" >&2
    exit 1
    ;;
esac
case "${NEXT%%.*}" in
  0|[1-9]|[1-9][0-9]*) ;;
  *) echo "主版本不能有前导零" >&2; exit 1 ;;
esac

# Rust crate version stays MAJOR.MINOR.PATCH; map legacy X.YY → X.Y.0
case "$NEXT" in
  [0-9]*.[0-9]*.[0-9]*)
    RUST_VERSION="$NEXT"
    ;;
  *)
    RUST_VERSION="${NEXT%%.*}.$((1${NEXT#*.} - 100)).0"
    ;;
esac

PBXPROJ=
if [ -f "$ROOT/OneWIFI.xcodeproj/project.pbxproj" ]; then
  PBXPROJ="$ROOT/OneWIFI.xcodeproj/project.pbxproj"
elif [ -f "$ROOT/Switch.xcodeproj/project.pbxproj" ]; then
  PBXPROJ="$ROOT/Switch.xcodeproj/project.pbxproj"
fi

printf '%s\n' "$NEXT" > "$ROOT/VERSION"
printf '%s\n' "$((BUILD + 1))" > "$ROOT/BUILD_NUMBER"

perl -0pi -e 's/MARKETING_VERSION: "[^"]+"/MARKETING_VERSION: "'"$NEXT"'"/g; s/CURRENT_PROJECT_VERSION: "[^"]+"/CURRENT_PROJECT_VERSION: "'"$((BUILD + 1))"'"/g' "$ROOT/project.yml"
if [ -n "$PBXPROJ" ]; then
  perl -0pi -e 's/MARKETING_VERSION = [^;]+;/MARKETING_VERSION = '"$NEXT"';/g; s/CURRENT_PROJECT_VERSION = [^;]+;/CURRENT_PROJECT_VERSION = '"$((BUILD + 1))"';/g' "$PBXPROJ"
fi
perl -0pi -e 's/^version = "[^"]+"$/version = "'"$RUST_VERSION"'"/m' "$ROOT/rust/Cargo.toml"
cargo +1.98.0 generate-lockfile --manifest-path "$ROOT/rust/Cargo.toml" --offline

echo "版本已更新：${CURRENT} → ${NEXT}（构建 $((BUILD + 1))）"
