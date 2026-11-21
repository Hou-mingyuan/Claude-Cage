#!/usr/bin/env bash
# Validate the actual downloaded distribution without installing or launching it.
set -euo pipefail
directory="${1:?Usage: scripts/verify-macos-release.sh <release-directory>}"
# stapler contacts Apple's CloudKit even for a stapled artifact. Hosted runners
# can lose this connection; retry service failures without bypassing validation.
validate_ticket() {
  local attempt status
  for attempt in 1 2 3; do
    if xcrun stapler validate "$1"; then return 0; else status=$?; fi
    if [[ "$status" != 68 || "$attempt" == 3 ]]; then return "$status"; fi
    echo "Apple ticket service unavailable; retry $attempt/2." >&2
    sleep 5
  done
}
shopt -s nullglob
# A directory holds at most one DMG per architecture: arm64 (Apple Silicon)
# and amd64 (Intel). Both bundle the same Mihomo, so one source serves both.
arm=("$directory"/Cage-*-macos-arm64.dmg)
intel=("$directory"/Cage-*-macos-amd64.dmg)
if [[ ${#arm[@]} -gt 1 || ${#intel[@]} -gt 1 || $(( ${#arm[@]} + ${#intel[@]} )) == 0 ]]; then
  echo 'Expected one Cage DMG per macOS architecture.' >&2; exit 1
fi
archives=(${arm[@]+"${arm[@]}"} ${intel[@]+"${intel[@]}"})
sources=("$directory"/mihomo-v*-source.tar.gz)
if [[ ${#sources[@]} != 1 ]]; then echo 'Expected exactly one Mihomo source archive.' >&2; exit 1; fi
# The release may also contain Windows installers and their corresponding
# sources. Validate this platform's downloaded files against the original list.
checksum_input="$(mktemp)"
trap 'rm -f "$checksum_input"' EXIT
for archive in "${archives[@]}"; do
  awk -v archive="${archive##*/}" -v source="${sources[0]##*/}" '
    NF == 2 && ($2 == archive || $2 == "*" archive) { archives++; print }
    NF == 2 && ($2 == source || $2 == "*" source) { sources++; print }
    END { if (archives != 1 || sources != 1) exit 1 }
  ' "$directory/SHA256SUMS" > "$checksum_input"
  (cd "$directory" && shasum -a 256 --check "$checksum_input")
done
rm -f "$checksum_input"
trap - EXIT

mount=''
detach() {
  [[ -n $mount ]] || return 0
  hdiutil detach "$mount" >/dev/null 2>&1 || true
  rmdir "$mount" 2>/dev/null || true
  mount=''
}
trap detach EXIT

verify_archive() {
  local archive=$1 version arch machine bundle executable
  version="${archive##*/Cage-}"
  arch="${version##*-macos-}"
  arch="${arch%.dmg}"
  version="${version%-macos-"$arch".dmg}"
  case "$arch" in arm64) machine=arm64 ;; amd64) machine=x86_64 ;; *) echo 'Unknown DMG architecture.' >&2; return 1 ;; esac
  codesign --verify --strict --verbose=2 "$archive"
  validate_ticket "$archive"
  spctl --assess --type open --context context:primary-signature --verbose=2 "$archive"
  mount="$(mktemp -d)"
  hdiutil attach -readonly -nobrowse -mountpoint "$mount" "$archive" >/dev/null
  bundle="$mount/Cage.app"
  test "$(/usr/libexec/PlistBuddy -c 'Print :CageVersion' "$bundle/Contents/Info.plist")" = "$version"
  codesign --verify --deep --strict --verbose=2 \
    -R='anchor apple generic and identifier "io.github.lee-xeo.claude-cage" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists' "$bundle"
  validate_ticket "$bundle"
  spctl --assess --type execute --verbose=2 "$bundle"
  # The DMG name is what the in-app updater selects; its App must match it.
  for executable in Contents/MacOS/Cage Contents/Resources/cage Contents/Resources/mihomo Contents/Resources/CageUpdateInstaller; do
    [[ "$(lipo -archs "$bundle/$executable")" == "$machine" ]] || { echo "Architecture mismatch in $executable" >&2; return 1; }
  done
  if [[ $arch == amd64 ]]; then
    [[ "$(lipo -archs "$bundle/Contents/Resources/intel-macos/runtime/bin/qemu-system-x86_64")" == x86_64 ]]
  fi
  detach
  echo "Verified Developer ID, notarization, Gatekeeper, version and corresponding source: Cage $version ($arch)"
}
for archive in "${archives[@]}"; do verify_archive "$archive"; done
