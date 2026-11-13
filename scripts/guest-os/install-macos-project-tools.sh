#!/bin/bash
# Run as root inside a Cage macOS guest. The supplied directory contains the
# verified macFUSE 5.5.0 installer and the three project tools from this build.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
fail() { printf '%s\n' "$*" >&2; exit 1; }
[[ $# == 1 || $# == 2 ]] || fail 'Usage: install-macos-project-tools.sh /path/to/root-owned-guest-project-kit [--gui-authorize]'
kit_arg=$1
gui_authorize=no
[[ ${2:-} == "" || ${2:-} == --gui-authorize ]] || fail 'Unknown installer mode.'
[[ ${2:-} == --gui-authorize ]] && gui_authorize=yes
[[ $kit_arg == /* ]] || fail 'Usage: install-macos-project-tools.sh /path/to/root-owned-guest-project-kit'
[[ $(uname -sm) == 'Darwin arm64' && $(sysctl -n hw.model) =~ ^VirtualMac[0-9]+,[0-9]+$ && $(id -u) == 0 ]] ||
    fail 'Run this installer in the target arm64 Cage guest as administrator.'
marker=/var/db/cage-guest-os
[[ -f $marker && ! -L $marker && $(stat -f %u "$marker") == 0 && $(cat "$marker") == macos ]] ||
    fail 'Missing the root-owned Cage guest marker.'
mode=$(stat -f %OLp "$marker")
(( (8#$mode & 022) == 0 )) || fail 'The guest marker is writable by another account.'

# Copy before verification, so a source directory cannot change after checks.
# Root-owned inputs are required because unsigned development builds can use a
# local ad-hoc signature. No downloaded code is executed to verify identity.
kit=$(cd -P "$1" && pwd)
[[ $(stat -f %u "$kit") == 0 && -z $(find "$kit" \( -type l -o ! -user root -o -perm -0020 -o -perm -0002 \) -print -quit) ]] ||
    fail 'Use a root-owned kit with no symlinks or group/other-writable members.'
parent=$(dirname "$kit")
while [[ $parent != / ]]; do
    mode=$(stat -f %Op "$parent")
    [[ $(stat -f %u "$parent") == 0 ]] && (( (8#$mode & 022) == 0 || (8#$mode & 01000) != 0 )) ||
        fail 'The kit can be replaced through an untrusted parent directory.'
    parent=$(dirname "$parent")
done
for file in macfuse.pkg cage-sshfs cage-macos-terminal; do [[ -f $kit/$file ]] || fail "Missing $file"; done
[[ -d $kit/CageProjectAccess.app ]] || fail 'Missing CageProjectAccess.app.'
umask 077
work=$(mktemp -d /private/var/tmp/cage-project-kit.XXXXXXXX)
trap 'rm -rf "$work"' EXIT
for file in macfuse.pkg cage-sshfs cage-macos-terminal; do cp "$kit/$file" "$work/$file"; done
ditto "$kit/CageProjectAccess.app" "$work/CageProjectAccess.app"
pkg="$work/macfuse.pkg"
[[ $(shasum -a 256 "$pkg" | cut -d' ' -f1) == 5acf9e30bab227707de8d15eb28472a0cc9ed45709846b3e067d283294dc7f9e ]] ||
    fail 'The macFUSE installer SHA-256 does not match the pinned release.'
pkgutil --check-signature "$pkg" | grep -F 'Developer ID Installer: Benjamin Fleischer (3T5GSNBU6W)' >/dev/null ||
    fail 'Unexpected macFUSE installer signer.'
for file in cage-sshfs cage-macos-terminal; do
    codesign --verify --strict --architecture arm64 "$work/$file"
done
access_id=dev.cage.guest.project-access
codesign --verify --strict -R "=identifier \"$access_id\"" "$work/CageProjectAccess.app"
macfuse_requirement='anchor apple generic and identifier "io.macfuse.app" and certificate leaf[subject.OU] = "3T5GSNBU6W"'
driver=/Library/Filesystems/macfuse.fs
vendor_app="$driver/Contents/Resources/macfuse.app"
installed=''
if [[ -e $driver ]]; then
    codesign --verify --strict -R "=$macfuse_requirement" "$vendor_app"
    installed=$(plutil -extract CFBundleShortVersionString raw -o - "$driver/Contents/Info.plist")
fi
normalize_version() {
    local version=$1 major minor patch
    [[ $version =~ ^([0-9]+)\.([0-9]+)(\.([0-9]+))?$ ]] || return 1
    major=$((10#${BASH_REMATCH[1]}))
    minor=$((10#${BASH_REMATCH[2]}))
    patch=${BASH_REMATCH[4]:-0}
    patch=$((10#$patch))
    printf '%d.%d.%d' "$major" "$minor" "$patch"
}
is_supported_version() {
    local normalized major minor patch
    normalized=$(normalize_version "$1") || return 1
    IFS=. read -r major minor patch <<<"$normalized"
    (( major > 5 || (major == 5 && minor >= 5) ))
}
up_to_date=no
if [[ -n $installed ]]; then
    is_supported_version "$installed" || fail 'The installed macFUSE version is too old or unrecognized.'
    up_to_date=yes
fi
if [[ $up_to_date == no ]]; then
    if [[ $gui_authorize == yes ]]; then
        gui_uid=$(stat -f '%u' /dev/console)
        (( gui_uid >= 500 )) || fail 'Log in to the intended guest account before authorizing macFUSE.'
        launchctl print "gui/$gui_uid" >/dev/null 2>&1 || fail 'The intended guest GUI session is unavailable.'
        [[ $(stat -f '%Su' /dev/console) == "${CAGE_MACOS_GUI_USER:-}" ]] ||
            fail 'The guest GUI account does not match the pending installation.'
        chmod 755 "$work"
        chmod 644 "$pkg"
        launchctl asuser "$gui_uid" sudo -n -H -u "#$gui_uid" -- /usr/bin/open -W -a /System/Library/CoreServices/Installer.app "$pkg" ||
            fail 'Apple Installer was cancelled or failed. Installation can be retried.'
    else
        installer -pkg "$pkg" -target /
    fi
fi
if ! codesign --verify --strict -R "=$macfuse_requirement" "$vendor_app"; then
    fail 'Apple Installer did not complete the verified macFUSE installation. Installation can be retried.'
fi
installed=$(plutil -extract CFBundleShortVersionString raw -o - "$driver/Contents/Info.plist") ||
    fail 'Apple Installer did not complete the verified macFUSE installation. Installation can be retried.'
is_supported_version "$installed" || fail 'The installed macFUSE version is too old or unrecognized.'
receipt=$(pkgutil --pkg-info-plist io.macfuse.installer.components.core | plutil -extract pkg-version raw -o - -) ||
    fail 'The macFUSE installation receipt is missing. Installation can be retried.'
normalized_installed=$(normalize_version "$installed") || fail 'Unrecognized installed macFUSE version.'
normalized_receipt=$(normalize_version "$receipt") || fail 'Unrecognized macFUSE installation receipt version.'
[[ $normalized_receipt == "$normalized_installed" ]] ||
    fail 'The macFUSE installation receipt has the wrong version. Installation can be retried.'

install_app() {
    local source=$1 target=$2 requirement=$3 stage
    if [[ -e $target || -L $target ]]; then
        [[ ! -L $target && -d $target ]] || fail "Unexpected application at $target"
        codesign --verify --strict -R "=$requirement" "$target"
        if diff -qr "$source" "$target" >/dev/null; then return; fi
        fail "A different verified build is already installed at $target. Stop and update that application before retrying."
    fi
    stage=$(mktemp -d /Applications/.cage-project-install.XXXXXXXX)
    if ditto "$source" "$stage/app" && chmod -R a+rX,go-w "$stage/app" &&
        codesign --verify --strict -R "=$requirement" "$stage/app" && mv "$stage/app" "$target"; then
        rmdir "$stage"
    else
        rm -rf "$stage"
        fail "Could not install $target"
    fi
}
install_app "$vendor_app" /Applications/macFUSE.app "$macfuse_requirement"
install_app "$work/CageProjectAccess.app" /Applications/CageProjectAccess.app "identifier \"$access_id\""
libexec=/usr/local/libexec
mkdir -p "$libexec"
for file in cage-sshfs cage-macos-terminal; do
    [[ ! -L $libexec/$file ]] || fail "Refusing to replace a linked executable: $file"
    if [[ -f $libexec/$file ]] && cmp -s "$work/$file" "$libexec/$file"; then continue; fi
    stage=$(mktemp "$libexec/.cage-tool.XXXXXXXX")
    if install -o root -g wheel -m 755 "$work/$file" "$stage" && mv -f "$stage" "$libexec/$file"; then :; else
        rm -f "$stage"
        fail "Could not install $file"
    fi
done
printf '%s\n' 'Project tools are ready. In the guest user session, open macFUSE.app once, then enable both macFUSE and macFUSE (local) under By Category > File System Extensions.'
