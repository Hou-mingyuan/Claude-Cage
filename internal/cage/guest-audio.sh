# Virtual audio output and on-demand microphone input shared by guest
# applications. No hardware modules are loaded, and the PulseAudio protocol is
# Unix-socket only.
if ! command -v pulseaudio >/dev/null || ! command -v parec >/dev/null; then
  sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 update </dev/null
  sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 install -y --no-install-recommends pulseaudio pulseaudio-utils </dev/null
fi
getent group cage-audio >/dev/null || sudo groupadd --system cage-audio
sudo usermod -a -G cage-audio cage
sudo usermod -a -G cage-audio cage-browser
sudo install -d -m 0755 /etc/claude-cage /etc/pulse/client.conf.d
# On an older running environment PulseAudio owns the socket pathname.
# Its systemd listener takes ownership once during component migration.
if systemctl is-active --quiet cage-audio.service && ! systemctl is-active --quiet cage-audio.socket; then
  sudo systemctl stop cage-audio.service
fi
sudo install -d -m 0750 -o cage -g cage-audio /run/cage-audio
sudo tee /etc/pulse/client.conf.d/99-cage.conf >/dev/null <<'CLIENT'
default-server = unix:/run/cage-audio/native
autospawn = no
CLIENT
sudo tee /etc/claude-cage/audio.pa >/dev/null <<'PULSE'
load-module module-null-sink sink_name=cage_output rate=48000 channels=2 format=s16le sink_properties=device.description=Cage
load-module module-pipe-source source_name=cage_input file=/run/cage-audio/input.fifo format=s16le rate=48000 channels=1 source_properties=device.description=Cage
load-module module-native-protocol-unix socket=/run/cage-audio/native auth-anonymous=0 auth-cookie-enabled=0 auth-group-enable=1 auth-group=cage-audio
set-default-sink cage_output
set-default-source cage_input
PULSE
sudo tee /etc/systemd/system/cage-audio.socket >/dev/null <<'UNIT'
[Unit]
Description=Cage virtual audio listener
[Socket]
ListenStream=/run/cage-audio/native
SocketUser=cage
SocketGroup=cage-audio
SocketMode=0660
RemoveOnStop=yes
UNIT
sudo tee /usr/local/bin/cage-audio-start >/dev/null <<'SCRIPT'
#!/bin/sh
# The redacting log runner preserves stdin. Give PulseAudio the systemd
# listener on descriptor 3 with the identity of its actual daemon process.
exec 3<&0
export LISTEN_PID=$$ LISTEN_FDS=1
exec /usr/bin/pulseaudio -n --daemonize=no --exit-idle-time=-1 --disallow-exit --disallow-module-loading --disable-shm=true --realtime=no --high-priority=no --use-pid-file=no --file=/etc/claude-cage/audio.pa
SCRIPT
sudo chmod 0755 /usr/local/bin/cage-audio-start
sudo tee /etc/systemd/system/cage-audio.service >/dev/null <<'UNIT'
[Unit]
Description=Cage virtual audio service
Requires=cage-audio.socket
[Service]
User=cage
Group=cage-audio
Environment=HOME=/home/cage
Environment=PULSE_RUNTIME_PATH=/run/cage-audio/runtime
Environment=PULSE_STATE_PATH=/run/cage-audio/state
RuntimeDirectory=cage-audio
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=yes
StandardInput=socket
StandardOutput=journal
ExecStartPre=-/usr/bin/rm -f /run/cage-audio/input.fifo
ExecStart=/usr/local/bin/cage-log-run /usr/local/bin/cage-audio-start
ExecStartPost=/bin/sh -c 'for i in $(seq 1 50); do [ -p /run/cage-audio/input.fifo ] && chmod 0600 /run/cage-audio/input.fifo && exit 0; sleep .1; done; exit 0'
LogNamespace=cage
Restart=on-failure
RestartSec=1
TimeoutStopSec=5
NoNewPrivileges=true
UNIT
sudo chmod 0644 /etc/claude-cage/audio.pa /etc/pulse/client.conf.d/99-cage.conf /etc/systemd/system/cage-audio.service
sudo systemctl daemon-reload
sudo systemctl reset-failed cage-audio.service cage-audio.socket 2>/dev/null || true
sudo systemctl start cage-audio.socket
