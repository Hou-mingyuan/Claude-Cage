#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=v1.19.33
license_checksum=3972dc9744f6499f0f9b2dbf76696f2ae7ad8af9b23dde66d6af86c9dfb36986
source_checksum=e7c596b37ff27ec487d19b1920f73e0c97a78067ea35766530ee9659ade72711
case "${CAGE_CORE_ARCH:-$(uname -m)}" in
  arm64) asset="mihomo-darwin-arm64-$version.gz"; checksum=060b64b0f2c9a310831db0fe4adb2878de67bfa606c311813c1f13966aa504b6 ;;
  x86_64|amd64) asset="mihomo-darwin-amd64-compatible-$version.gz"; checksum=eefe967b74312f70b64efa0dfa54929d00c7af8e94416e1b12df09863a40f35b ;;
  *) echo 'Unsupported Cage proxy-core architecture' >&2; exit 1 ;;
esac
directory=_output/proxy-core
mkdir -p "$directory"
archive="$directory/$asset"
node scripts/fetch-build-artifact.mjs "https://github.com/MetaCubeX/mihomo/releases/download/$version/$asset" "$checksum" "$archive"
gzip -dc "$archive" > "$directory/mihomo.part"
chmod 755 "$directory/mihomo.part"
mv "$directory/mihomo.part" "$directory/mihomo"
license_url="https://raw.githubusercontent.com/MetaCubeX/mihomo/$version/LICENSE"
if ! node scripts/fetch-build-artifact.mjs "$license_url" "$license_checksum" "$directory/LICENSE"; then
  source_url="https://github.com/MetaCubeX/mihomo/archive/refs/tags/$version.tar.gz"
  source_archive="$directory/mihomo-$version-source.tar.gz"
  echo "Could not verify LICENSE from the cache or $license_url; trying the pinned Mihomo source archive." >&2
  if ! { node scripts/fetch-build-artifact.mjs "$source_url" "$source_checksum" "$source_archive" &&
    tar -xOf "$source_archive" "mihomo-${version#v}/LICENSE" > "$directory/LICENSE.part" &&
    printf '%s  %s\n' "$license_checksum" "$directory/LICENSE.part" | shasum -a 256 -c -s; }; then
    rm -f "$directory/LICENSE.part"
    cat >&2 <<EOF
The Mihomo license could not be obtained or verified. The package requires the upstream LICENSE.
LICENSE: $license_url (SHA-256 $license_checksum)
Source archive: $source_url (SHA-256 $source_checksum)
To retry the download, remove any cached files reported as invalid. Alternatively:
  - Download the plain-text LICENSE from the URL above on a connected machine and copy it to $PWD/$directory/LICENSE
  - Save the source archive above as $PWD/$source_archive
  - Set CAGE_GITHUB_MIRROR to a reachable HTTPS GitHub mirror prefix and rerun the build
EOF
    exit 1
  fi
  mv "$directory/LICENSE.part" "$directory/LICENSE"
fi
cat > "$directory/NOTICE" <<EOF
Mihomo $version, distributed as a separate executable by Cage.
Upstream: https://github.com/MetaCubeX/mihomo
Source for this unmodified binary: https://github.com/MetaCubeX/mihomo/archive/refs/tags/$version.tar.gz
License: GPL-3.0. See LICENSE.
Archive: $asset
SHA-256: $checksum
Release digest: https://api.github.com/repos/MetaCubeX/mihomo/releases/tags/$version
EOF
"$directory/mihomo" -v
