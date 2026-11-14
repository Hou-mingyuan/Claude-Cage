# shellcheck shell=bash
cage_is_ubuntu() (
  . /etc/os-release
  [ "$ID" = ubuntu ]
)

cage_install_ubuntu_browser() (
  set -euo pipefail
  sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get install -y --no-install-recommends \
    snapd dbus-user-session fonts-liberation fonts-dejavu-core xvfb xauth </dev/null

  # snapd downloads and verifies the store assertions in its own process.
  # Keep the short-lived maintenance proxy out of persistent snap settings.
  local unit_dir=/run/systemd/system/snapd.service.d
  local dropin proxy_file
  if [[ -n ${CAGE_INSTALL_PROXY:-} ]]; then
    sudo install -d -m 0755 "$unit_dir"
    proxy_file=$(sudo mktemp /run/browser-install.XXXXXX)
    dropin="$unit_dir/90-${proxy_file##*/}.conf"
    trap 'sudo rm -f "$dropin" "$proxy_file"; sudo systemctl daemon-reload; sudo systemctl restart snapd' EXIT
    printf 'HTTPS_PROXY=%s\nhttps_proxy=%s\n' "$CAGE_INSTALL_PROXY" "$CAGE_INSTALL_PROXY" | sudo tee "$proxy_file" >/dev/null
    printf '[Service]\nEnvironmentFile=%s\n' "$proxy_file" | sudo tee "$dropin" >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl restart snapd
  fi
  sudo snap wait system seed.loaded
  if snap list chromium >/dev/null 2>&1; then
    local channel
    channel=$(LC_ALL=C snap list chromium | awk '$1 == "chromium" {print $4}')
    if [[ $channel != latest/stable && $channel != stable ]]; then
      echo "Installed Chromium follows $channel. Keep that installation and select latest/stable before setting up the workbench." >&2
      return 1
    fi
    sudo snap refresh chromium
  else
    sudo snap install chromium --channel=latest/stable
  fi

  # Chrome for Testing wrote this directory in place. Do not give a browser
  # older than that profile an opportunity to downgrade its database schemas.
  local profile_version=/home/cage-browser/profile/'Last Version'
  if sudo test -s "$profile_version"; then
    local last current
    last=$(sudo -u cage-browser cat "$profile_version")
    current=$(LC_ALL=C snap list chromium | awk '$1 == "chromium" {print $2}')
    if ! dpkg --compare-versions "$current" ge "$last"; then
      echo "Chromium stable $current is older than the existing profile ($last); retry after the stable channel updates." >&2
      return 1
    fi
  fi
  # Keep distro launchers and other browser installations intact. Snap dispatches
  # by executable name, so the workbench entry must also be named chromium.
  sudo install -d -m 0755 /usr/local/lib/workbench-browser
  sudo ln -sfnT /snap/bin/chromium /usr/local/lib/workbench-browser/chromium
)

cage_install_chromium() {
  if cage_is_ubuntu; then
    cage_install_ubuntu_browser
  else
    cage_cache_chromium --no-install-recommends chromium chromium-l10n
    sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get install -y --no-install-recommends chromium chromium-l10n </dev/null
  fi
}

# Jammy ships noVNC 1.0. The shared input/clipboard patch needs 1.6.
# Keep the system package untouched; websockify serves the Cage-owned copy.
cage_install_ubuntu_novnc() (
  set -euo pipefail
  local cache="$HOME/.cache/cage/downloads/$CAGE_UBUNTU_NOVNC_SHA256" stage
  if [ -f /usr/local/share/cage-novnc/VERSION ] && [ "$(cat /usr/local/share/cage-novnc/VERSION)" = "$CAGE_UBUNTU_NOVNC_VERSION" ]; then
    sudo chmod 0755 /usr/local/share/cage-novnc
    return
  fi
  mkdir -p "$cache"
  exec 8>"$cache/lock"
  flock -w 60 8
  cage_download_tool "https://github.com/novnc/noVNC/archive/refs/tags/v$CAGE_UBUNTU_NOVNC_VERSION.tar.gz" "$CAGE_UBUNTU_NOVNC_SHA256" "$cache/novnc.tar.gz"
  sudo install -d -m 0755 /usr/local/share
  stage=$(sudo mktemp -d /usr/local/share/.cage-novnc-XXXXXX)
  trap 'sudo rm -rf "$stage"' EXIT
  sudo tar -xzf "$cache/novnc.tar.gz" --strip-components=1 -C "$stage"
  sudo chmod 0755 "$stage"
  printf '%s\n' "$CAGE_UBUNTU_NOVNC_VERSION" | sudo tee "$stage/VERSION" >/dev/null
  # Called only after cage-webvnc has stopped.
  sudo rm -rf /usr/local/share/cage-novnc
  sudo mv "$stage" /usr/local/share/cage-novnc
)
