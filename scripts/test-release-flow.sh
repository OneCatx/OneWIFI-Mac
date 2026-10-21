#!/bin/sh
set -eu

# Run the copied release scripts in an isolated fake repository workspace. The
# hdiutil shim writes plain text; this test never creates a real app or DMG.
case "$(basename "$0")" in
  xcodegen) exit 0 ;;
  xcodebuild)
    derived_data=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -derivedDataPath ]; then
        shift
        derived_data=$1
      fi
      shift
    done
    [ -n "$derived_data" ] || exit 2
    printf 'xcodebuild\n' >> "$MOCK_REPO/events"
    mkdir -p "$derived_data/Build/Products/Release/OneWIFI.app/Contents"
    printf 'mock app\n' > "$derived_data/Build/Products/Release/OneWIFI.app/Contents/mock.txt"
    exit 0
    ;;
  codesign)
    for last_argument do :; done
    [ -f "$last_argument/Contents/mock.txt" ]
    exit
    ;;
  ditto)
    /bin/cp -R "$1" "$2"
    exit
    ;;
  hdiutil)
    action=$1
    shift
    case "$action" in
      create)
        for output do :; done
        printf 'mock image\n' > "$output"
        [ "$MOCK_FAILURE" != create ]
        ;;
      verify)
        [ "$MOCK_FAILURE" != verify ] && grep -qx 'mock image' "$1"
        ;;
      *) exit 2 ;;
    esac
    exit
    ;;
  cp)
    /bin/cp "$@"
    if [ "$MOCK_FAILURE" = archive_copy ] && [ "${1:-}" = -p ]; then
      for output do :; done
      case "$output" in
        "$MOCK_WORKSPACE"/dist/archive/.OneWIFI-*.dmg.*)
          printf 'corruption\n' >> "$output"
          ;;
      esac
    fi
    exit
    ;;
  mv)
    printf 'mv %s %s\n' "$MOCK_FAILURE" "${2:-}" >> "$MOCK_REPO/events"
    if [ "$MOCK_FAILURE" = manifest_publish ] &&
       [ "${2:-}" = "$MOCK_WORKSPACE/dist/release-manifest.json" ]; then
      exit 1
    fi
    /bin/mv "$@"
    exit
    ;;
esac

SOURCE_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
SWITCH_SOURCE_ROOT="$SOURCE_ROOT"
. "$SOURCE_ROOT/scripts/workspace-env.sh"
FIXTURE=$(mktemp -d "${TMPDIR:-/tmp}/switch-release-flow-test.XXXXXX")
FIXTURE=$(CDPATH= cd -- "$FIXTURE" && pwd)
trap 'rm -rf "$FIXTURE"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$FIXTURE/bin"
for command_name in xcodegen xcodebuild codesign ditto hdiutil cp mv; do
  ln -s "$SOURCE_ROOT/scripts/test-release-flow.sh" "$FIXTURE/bin/$command_name"
done

set -- $(sh "$SOURCE_ROOT/scripts/version.sh")
VERSION=$1
BUILD_NUMBER=$2
DMG_NAME="OneWIFI-$VERSION-macOS27.dmg"

fail() {
  echo "release flow mock test failed: $*" >&2
  exit 1
}

new_case() {
  case_name=$1
  MOCK_WORKSPACE="$FIXTURE/$case_name"
  MOCK_REPO="$MOCK_WORKSPACE/src"
  CASE_TEMP="$MOCK_WORKSPACE/build/tmp"
  mkdir -p "$MOCK_REPO/scripts" "$MOCK_WORKSPACE/dist" \
    "$MOCK_WORKSPACE/dist/archive" "$CASE_TEMP"
  for script_name in release.sh package-dmg.sh sign-release-app.sh \
    generate-release-manifest.sh version.sh workspace-env.sh; do
    /bin/cp "$SOURCE_ROOT/scripts/$script_name" "$MOCK_REPO/scripts/$script_name"
  done
  /bin/cp "$SOURCE_ROOT/VERSION" "$SOURCE_ROOT/BUILD_NUMBER" "$MOCK_REPO/"
  DIST_DMG="$MOCK_WORKSPACE/dist/$DMG_NAME"
  ARCHIVE_DMG="$MOCK_WORKSPACE/dist/archive/$DMG_NAME"
}

run_release() {
  scenario=$1
  MOCK_REPO="$MOCK_REPO" MOCK_WORKSPACE="$MOCK_WORKSPACE" MOCK_FAILURE="$scenario" \
    SWITCH_NOTARY_PROFILE= \
    PATH="$FIXTURE/bin:$PATH" \
    sh "$MOCK_REPO/scripts/release.sh" > "$MOCK_REPO/output.log" 2>&1
}

assert_clean() {
  [ ! -e "$DIST_DMG" ] || fail "dist DMG leaked in $case_name"
  [ ! -e "$ARCHIVE_DMG" ] || fail "archive DMG leaked in $case_name"
  [ -z "$(find "$MOCK_WORKSPACE/dist" -maxdepth 1 -name '.OneWIFI-*-release.*' -print)" ] ||
    fail "dist staging leaked in $case_name"
  [ -z "$(find "$MOCK_WORKSPACE/dist/archive" -maxdepth 1 -name '.OneWIFI-*.dmg.*' -print)" ] ||
    fail "archive staging leaked in $case_name"
  [ -z "$(find "$CASE_TEMP" -maxdepth 1 -name 'onewifi-release.*' -print)" ] ||
    fail "build staging leaked in $case_name"
}

new_case success
run_release none || {
  sed -n '1,80p' "$MOCK_REPO/output.log" >&2
  fail 'successful release returned failure'
}
grep -qx 'mock image' "$DIST_DMG" || fail 'dist output absent or invalid'
cmp -s "$DIST_DMG" "$ARCHIVE_DMG" || fail 'desktop archive differs from dist'
[ -f "$MOCK_WORKSPACE/dist/release-manifest.json" ] || fail 'manifest missing'
ruby -rjson -rdigest -e '
  manifest = JSON.parse(File.read(ARGV[0]))
  artifact = manifest.fetch("artifacts").fetch(0)
  abort "incorrect manifest" unless
    manifest.fetch("version") == ARGV[2] &&
    manifest.fetch("build") == ARGV[3] &&
    artifact.fetch("name") == File.basename(ARGV[1]) &&
    artifact.fetch("architecture") == "arm64" &&
    artifact.fetch("minimum_macos") == "27.0" &&
    artifact.fetch("size") == File.size(ARGV[1]) &&
    artifact.fetch("sha256") == Digest::SHA256.file(ARGV[1]).hexdigest
' "$MOCK_WORKSPACE/dist/release-manifest.json" "$DIST_DMG" "$VERSION" "$BUILD_NUMBER" ||
  fail 'manifest content mismatch'
[ -z "$(find "$MOCK_WORKSPACE/dist" -maxdepth 1 -name '.OneWIFI-*-release.*' -print)" ] ||
  fail 'successful release left dist staging'
[ -z "$(find "$MOCK_WORKSPACE/dist/archive" -maxdepth 1 -name '.OneWIFI-*.dmg.*' -print)" ] ||
  fail 'successful release left archive staging'

for scenario in create verify archive_copy manifest_publish; do
  new_case "$scenario"
  if [ "$scenario" = manifest_publish ]; then
    printf 'previous manifest\n' > "$MOCK_WORKSPACE/dist/release-manifest.json"
  fi
  if run_release "$scenario"; then
    sed -n '1,80p' "$MOCK_REPO/output.log" >&2
    sed -n '1,80p' "$MOCK_REPO/events" >&2
    fail "$scenario failure was accepted"
  fi
  assert_clean
  if [ "$scenario" = manifest_publish ]; then
    grep -qx 'previous manifest' "$MOCK_WORKSPACE/dist/release-manifest.json" ||
      fail 'manifest publish failure changed the prior manifest'
  fi
done

for conflict in dist_file archive_file dist_symlink archive_symlink; do
  new_case "$conflict"
  printf 'existing artifact\n' > "$MOCK_REPO/sentinel"
  case "$conflict" in
    dist_file) /bin/cp "$MOCK_REPO/sentinel" "$DIST_DMG" ;;
    archive_file) /bin/cp "$MOCK_REPO/sentinel" "$ARCHIVE_DMG" ;;
    dist_symlink) ln -s "$MOCK_REPO/sentinel" "$DIST_DMG" ;;
    archive_symlink) ln -s "$MOCK_REPO/sentinel" "$ARCHIVE_DMG" ;;
  esac
  if run_release none; then
    fail "$conflict was overwritten"
  fi
  [ ! -e "$MOCK_REPO/events" ] || fail "$conflict reached xcodebuild"
  grep -qx 'existing artifact' "$MOCK_REPO/sentinel" ||
    fail "$conflict changed its original target"
  case "$conflict" in
    dist_*) [ -e "$DIST_DMG" ] || fail "$conflict disappeared" ;;
    archive_*) [ -e "$ARCHIVE_DMG" ] || fail "$conflict disappeared" ;;
  esac
done

echo 'release flow tests passed (isolated fake app and text image only)'
