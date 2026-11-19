#!/usr/bin/env bash
set -euo pipefail

phase=$1
stage=$(cygpath -u "$2")
source_dir="$stage/source"
build_dir="$stage/build"
export TMPDIR="$stage/tmp"
export TMP="$(cygpath -w "$TMPDIR")"
export TEMP="$TMP"
mkdir -p "$TMPDIR"

if [[ "$phase" == configure ]]; then
    archive=$(cygpath -u "$3")
    keymap_archive=$(cygpath -u "$4")
    patch_dir=$(cygpath -u "$5")
    mkdir -p "$source_dir" "$build_dir"
    # Materialize relative links as native-readable copies without privileges.
    export MSYS=winsymlinks:deepcopy
    tar -xf "$archive" -C "$source_dir" --strip-components=1
    keymap_dir="$source_dir/subprojects/keycodemapdb"
    mkdir -p "$keymap_dir"
    tar -xzf "$keymap_archive" -C "$keymap_dir" --strip-components=1

    for patch_file in "$patch_dir"/*.patch; do
        patch -d "$source_dir" -p1 < "$patch_file"
    done
    cd "$build_dir"
    "$source_dir/configure" --target-list=x86_64-softmmu --without-default-features \
        --prefix=C:/CageQemu --bindir=. --datadir=share --with-suffix= \
        --extra-cflags=-D_WIN32_WINNT=0x0A00 \
        --enable-whpx --enable-tcg --enable-pixman --enable-slirp --enable-tools \
        --disable-docs --disable-guest-agent --disable-werror \
        --disable-debug-info -Dcage_install_bundle=false
    exit
fi
[[ "$phase" == package ]] || { echo "Unknown QEMU build phase: $phase" >&2; exit 2; }
output_dir=$(cygpath -u "$3")
system_dir=$(cygpath -u "$4")
mkdir -p "$output_dir"
cd "$build_dir"
for binary in qemu-system-x86_64.exe qemu-img.exe; do
    # Newer binutils can fail when asked to strip a PE in place.
    "$MINGW_PREFIX/bin/strip" -o "$output_dir/$binary" "$binary"
done

# Copy the DLL dependency closure from this toolchain, using the same versions
# that were linked into the executables. Windows system DLLs remain system-owned.
pending=("$output_dir"/*.exe)
packages=()
while ((${#pending[@]})); do
    binary=${pending[0]}
    pending=("${pending[@]:1}")
    imports=$("$MINGW_PREFIX/bin/objdump" -p "$binary")
    while read -r dll; do
        [[ -f "$output_dir/$dll" ]] && continue
        if [[ -f "$MINGW_PREFIX/bin/$dll" ]]; then
            cp "$MINGW_PREFIX/bin/$dll" "$output_dir/$dll"
            packages+=("$(pacman -Qqo "$MINGW_PREFIX/bin/$dll")")
            pending+=("$output_dir/$dll")
        elif [[ ! -f "$system_dir/$dll" && ${dll,,} != api-ms-win-* && ${dll,,} != ext-ms-* ]]; then
            echo "Missing QEMU runtime dependency $dll (imported by ${binary##*/}). Install the MSYS2 UCRT64 libraries in docs/windows.md; Meson fallback libraries cannot replace them." >&2
            exit 1
        fi
    done < <(awk '/DLL Name:/ {print $3}' <<<"$imports")
done
pacman -Q > "$output_dir/build-packages.txt"
# Keep the exact source package identity with cached binaries. A later runner
# must not collect the source of whichever library version happens to be installed.
for package in "${packages[@]}"; do
    read -r name version < <(pacman -Q "$package")
    base=$(awk '/^%BASE%$/ {getline; print; exit}' "/var/lib/pacman/local/$name-$version/desc")
    [[ -n "$base" ]] || { echo "Source package identity missing for $package" >&2; exit 1; }
    printf '%s %s\n' "$base" "$version"
done | sort -u > "$output_dir/build-source-packages.txt"
