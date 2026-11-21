# Sourced by the macOS build scripts. Checks the host toolchain before the long
# build and keeps caches inside the workspace when the user cache locations are
# not writable (for example in an agent sandbox that only allows the checkout).
# Sets swift_flags for every xcrun swiftc / xcrun swift call; expand it as
# ${swift_flags[@]+"${swift_flags[@]}"} so an empty array works under set -u
# in the bash 3.2 that ships with macOS.

macos_build_cache="$PWD/_output/cache"
swift_flags=()

macos_writable_directory() {
  local directory="$1"
  [[ -n "$directory" ]] || return 1
  mkdir -p "$directory" 2>/dev/null || return 1
  local probe
  probe="$(mktemp "$directory/.cage-write-check.XXXXXX" 2>/dev/null)" || return 1
  rm -f "$probe"
}

macos_prepare_caches() {
  # macOS mktemp ignores TMPDIR for default templates; the scripts pass
  # templates under _output, so only tool caches need redirecting here.
  if ! macos_writable_directory "${npm_config_cache:-$HOME/.npm}"; then
    export npm_config_cache="$macos_build_cache/npm"
    mkdir -p "$npm_config_cache"
    printf 'npm cache is not writable; using %s\n' "$npm_config_cache" >&2
  fi
  if ! macos_writable_directory "${UV_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uv}"; then
    export UV_CACHE_DIR="$macos_build_cache/uv"
    mkdir -p "$UV_CACHE_DIR"
    printf 'uv cache is not writable; using %s\n' "$UV_CACHE_DIR" >&2
  fi
  local user_cache
  user_cache="$(getconf DARWIN_USER_CACHE_DIR 2>/dev/null || true)"
  if ! macos_writable_directory "${user_cache:+${user_cache%/}/clang/ModuleCache}"; then
    local modules="$macos_build_cache/swift-modules"
    mkdir -p "$modules"
    # swiftc needs both: its own module cache and the one ClangImporter uses.
    swift_flags=(-module-cache-path "$modules" -Xcc "-fmodules-cache-path=$modules")
    printf 'Swift module cache is not writable; using %s\n' "$modules" >&2
  fi
}

macos_link_probe() {
  local directory="$1"
  printf 'int main(void) { return 0; }\n' > "$directory/probe.c"
  xcrun clang -mmacosx-version-min=14.0 -framework CoreFoundation -framework Security \
    "$directory/probe.c" -o "$directory/probe" 2> "$directory/probe.log"
}

macos_check_toolchain() {
  local probe
  probe="$(mktemp -d "$PWD/_output/toolchain-check.XXXXXX")"
  # cgo links against the default SDK. Command Line Tools can install an SDK
  # newer than their clang understands (tapi: unknown architecture arm64e.x1).
  if ! macos_link_probe "$probe"; then
    if [[ -z "${SDKROOT:-}" ]] && grep -q 'unknown architecture' "$probe/probe.log"; then
      local sdk developer
      developer="$(xcode-select -p 2>/dev/null || true)"
      while IFS= read -r sdk; do
        if SDKROOT="$sdk" macos_link_probe "$probe"; then
          export SDKROOT="$sdk"
          printf 'The default macOS SDK is newer than this clang; using %s\n' "$SDKROOT" >&2
          break
        fi
      done < <(for root in "$developer/SDKs" "$developer/Platforms/MacOSX.platform/Developer/SDKs" /Library/Developer/CommandLineTools/SDKs; do
        [[ -d "$root" ]] || continue
        find "$root" -maxdepth 1 -name 'MacOSX[0-9]*.sdk' -type d | while IFS= read -r path; do
          name="${path##*/MacOSX}"
          printf '%s\t%s\n' "${name%.sdk}" "$path"
        done
      done | sort -V -r | cut -f 2)
    fi
    if [[ -z "${SDKROOT:-}" ]] || ! macos_link_probe "$probe"; then
      cat "$probe/probe.log" >&2
      echo 'The macOS C toolchain cannot link a minimal program. If the error mentions an unknown architecture, the default SDK is newer than clang: update Xcode or the Command Line Tools, or set SDKROOT to an older SDK from xcrun --show-sdk-path.' >&2
      rm -rf "$probe"
      exit 1
    fi
  fi
  printf 'import Cocoa\n' > "$probe/probe.swift"
  if ! xcrun swiftc ${swift_flags[@]+"${swift_flags[@]}"} -typecheck "$probe/probe.swift" 2> "$probe/swift.log"; then
    cat "$probe/swift.log" >&2
    if grep -q "redefinition of module 'SwiftBridging'" "$probe/swift.log"; then
      echo 'The Command Line Tools contain leftover module maps from an older version (both usr/include/swift/module.modulemap and bridging.modulemap define SwiftBridging). Reinstall the Command Line Tools, or select Xcode with xcode-select, then retry.' >&2
    else
      echo 'xcrun swiftc cannot import Cocoa. Check xcode-select -p and the installed Xcode or Command Line Tools.' >&2
    fi
    rm -rf "$probe"
    exit 1
  fi
  rm -rf "$probe"
}

# File Provider (iCloud Drive, other sync clients) adds Finder information and
# provider attributes that codesign rejects as detritus. Signing covers only
# the bundle's contents, so clear extended attributes first. Licenses copied
# from the Go module cache are 0444, and xattr cannot remove attributes such
# as com.apple.provenance from a file its owner cannot write.
macos_clear_bundle_attributes() {
  chmod -R u+w "$1" 2>/dev/null || true
  if ! xattr -cr "$1" 2>/dev/null; then
    echo "Could not clear extended attributes in $1. If codesign reports detritus, build from a directory that is not managed by a file sync client." >&2
  fi
}
