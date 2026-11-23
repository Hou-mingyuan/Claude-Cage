#!/bin/sh
# Run as root before any dependency transaction. Keep the bootstrap independent
# of python3/curl and of archive mirrors that may have triggered APT's retry bug.
set -eu
. /etc/os-release
if [ "$ID" != debian ] || [ "${VERSION_ID:-}" != 13 ]; then
  exit 0
fi
# A package transaction cut off by a stopped environment or a power loss leaves
# dpkg's journal behind, and every later apt-get refuses to run until it is
# replayed. Replaying it only finishes the packages that were already chosen.
if [ -n "$(ls -A /var/lib/dpkg/updates 2>/dev/null)" ]; then
  printf 'Finishing an interrupted package installation...\n'
  env DEBIAN_FRONTEND=noninteractive dpkg --configure -a </dev/null
fi
version=$(dpkg-query -W -f='${Version}' apt)
if dpkg --compare-versions "$version" ge 3.0.4; then
  exit 0
fi
printf 'Updating Debian APT %s to a version with the delayed-item memory fix...\n' "$version"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
chmod 0755 "$stage"
mkdir -m 0755 "$stage/lists"
# HTTPS first where the image has a CA bundle, for networks that only allow
# port 443; HTTP also works in minimal images without one. Either way APT
# requires the Debian archive signature and package hashes; no insecure
# options are enabled.
schemes=http
if [ -s /etc/ssl/certs/ca-certificates.crt ]; then
  schemes='https http'
fi
apt_bootstrap_run() {
  env DEBIAN_FRONTEND=noninteractive apt-get \
    -o Acquire::Retries=0 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15 \
    -o DPkg::Lock::Timeout=60 -o APT::Update::Error-Mode=any \
    -o Dir::Etc::sourcelist="$stage/security.list" -o Dir::Etc::sourceparts=- \
    -o Dir::State::lists="$stage/lists" "$@" </dev/null
}
apt_bootstrap_get() {
  if apt_bootstrap_run "$@"; then
    return 0
  else
    status=$?
  fi
  # The same marker as agent-vm.setup.sh, so the host can name the failure.
  printf 'APT_FAILED exit=%s command=apt-get %s\n' "$status" "$*" >&2
  return "$status"
}
status=1
for scheme in $schemes; do
  printf 'deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] %s://deb.debian.org/debian-security trixie-security main\n' "$scheme" > "$stage/security.list"
  if apt_bootstrap_run update; then
    status=0
    break
  else
    status=$?
  fi
done
if [ "$status" != 0 ]; then
  printf 'APT_FAILED exit=%s command=apt-get update\n' "$status" >&2
  exit "$status"
fi
packages='apt libapt-pkg7.0'
if [ "$(dpkg-query -W -f='${Status}' apt-utils 2>/dev/null || true)" = 'install ok installed' ]; then
  packages="$packages apt-utils"
fi
# The package names above are fixed, and --only-upgrade preserves optional tools.
apt_bootstrap_get install -y --only-upgrade --no-install-recommends $packages
version=$(dpkg-query -W -f='${Version}' apt)
if ! dpkg --compare-versions "$version" ge 3.0.4; then
  printf 'APT is still %s; dependency installation stopped. Check the Debian security source and retry.\n' "$version" >&2
  exit 1
fi
printf 'Debian APT %s is ready.\n' "$version"
