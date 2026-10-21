#!/bin/sh
set -eu
SWITCH_SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$SWITCH_SOURCE_ROOT/scripts/workspace-env.sh"

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
OUTPUT=${1:-"$SWITCH_DIST_DIR/release-manifest.json"}
ARTIFACT_ARG=${2:-}
set -- $("$ROOT/scripts/version.sh")
VERSION=$1
BUILD_NUMBER=$2
MODERN="$SWITCH_DIST_DIR/Switch-$VERSION-macOS27.dmg"
ARTIFACT=${ARTIFACT_ARG:-$MODERN}
[ -f "$ARTIFACT" ] || { echo "错误：缺少发行文件 $ARTIFACT" >&2; exit 1; }

artifact_json() {
  file=$1
  minimum=$2
  name=$(basename "$MODERN")
  sha=$(shasum -a 256 "$file" | awk '{print $1}')
  size=$(stat -f '%z' "$file")
  printf '    {"name":"%s","architecture":"arm64","minimum_macos":"%s","size":%s,"sha256":"%s"}' \
    "$name" "$minimum" "$size" "$sha"
}

temporary=$(mktemp "${TMPDIR}/switch-release-manifest.XXXXXX")
trap 'rm -f "$temporary"' EXIT INT TERM
{
  printf '{\n'
  printf '  "schema_version":1,\n'
  printf '  "product":"OneWIFI",\n'
  printf '  "version":"%s",\n' "$VERSION"
  printf '  "build":"%s",\n' "$BUILD_NUMBER"
  printf '  "generated_at":"%s",\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf '  "artifacts":[\n'
  artifact_json "$ARTIFACT" "27.0"
  printf '\n  ]\n}\n'
} > "$temporary"

mkdir -p "$(dirname "$OUTPUT")"
mv "$temporary" "$OUTPUT"
trap - EXIT INT TERM
echo "created: $OUTPUT"
