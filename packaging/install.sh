#!/usr/bin/env bash
#
# Install samo-radio as a systemd service on this machine.
#
# Run it on the box with the sound card — the same one running samo-server:
#
#   sudo ./packaging/install.sh
#
# It is idempotent: re-run it to upgrade the binary in place.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
STATE_DIR="${STATE_DIR:-/var/lib/samo-radio}"
SERVICE_USER="${SERVICE_USER:-samo-radio}"
UNIT_PATH="/etc/systemd/system/samo-radio.service"

if [[ $EUID -ne 0 ]]; then
  echo "install.sh must run as root (sudo $0)" >&2
  exit 1
fi

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
# Soft warning. Defined because the mixer step's fallback calls it, and an
# undefined function under `set -e` would abort the install at the last step —
# on the branch that exists precisely to let a non-fatal problem pass.
note() { printf '    \033[2m%s\033[0m\n' "$*"; }

# ---- dependencies -----------------------------------------------------------

missing=()
command -v ffmpeg >/dev/null 2>&1 || missing+=("ffmpeg")
# Either sink tool is enough; the daemon picks whichever is present. amixer and
# alsactl ship in alsa-utils too, which is what the unmute step below needs.
if ! command -v aplay >/dev/null 2>&1 && ! command -v paplay >/dev/null 2>&1; then
  missing+=("alsa-utils")
fi
command -v amixer >/dev/null 2>&1 || missing+=("alsa-utils")
if [[ ${#missing[@]} -gt 0 ]]; then
  say "installing missing packages: ${missing[*]}"
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y "${missing[@]}"
  else
    echo "please install: ${missing[*]}" >&2
    exit 1
  fi
fi

# ---- build ------------------------------------------------------------------

if [[ -x "${REPO_DIR}/samo-radio" ]]; then
  BINARY="${REPO_DIR}/samo-radio"
  say "using prebuilt binary ${BINARY}"
else
  command -v go >/dev/null 2>&1 || { echo "go toolchain required to build (or ship a prebuilt ./samo-radio)" >&2; exit 1; }
  say "building samo-radio"
  ( cd "$REPO_DIR" && CGO_ENABLED=0 go build -trimpath -o "${REPO_DIR}/samo-radio" ./cmd/samo-radio )
  BINARY="${REPO_DIR}/samo-radio"
fi

# ---- service account --------------------------------------------------------

if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  say "creating system user ${SERVICE_USER}"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
# Membership in `audio` is what grants /dev/snd access. The unit asks for it as
# a supplementary group, but adding it here too keeps `sudo -u samo-radio aplay`
# working for debugging.
if getent group audio >/dev/null 2>&1; then
  usermod -aG audio "$SERVICE_USER"
fi

# ---- install ----------------------------------------------------------------

say "installing ${BIN_DIR}/samo-radio"
# Write beside the target and rename, rather than writing over it.
#
# On an upgrade the old binary is running, and Linux refuses to open a busy
# executable for writing — a plain `install` over the top fails with "Text file
# busy". rename(2) is atomic and does not touch the running image, so the
# process keeps its old inode until the restart below picks up the new one.
install -m 0755 "$BINARY" "${BIN_DIR}/.samo-radio.new"
mv -f "${BIN_DIR}/.samo-radio.new" "${BIN_DIR}/samo-radio"

install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$STATE_DIR"

say "installing ${UNIT_PATH}"
install -m 0644 "${REPO_DIR}/packaging/samo-radio.service" "$UNIT_PATH"

# A control token means samo-server has to prove it is samo-server before it can
# take over the speakers. Generated once and left alone on upgrades.
CONFIG_FILE="${STATE_DIR}/config.json"
if [[ ! -f "$CONFIG_FILE" ]]; then
  TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat >"$CONFIG_FILE" <<EOF
{
  "deviceName": "$(hostname)",
  "listenAddr": "127.0.0.1:7970",
  "controlToken": "${TOKEN}",
  "server": { "baseUrl": "", "token": "" },
  "output": { "backend": "auto", "device": "", "sampleRate": 48000, "channels": 2, "bufferMillis": 300 },
  "volume": 1,
  "autoTuneOnBoot": true
}
EOF
  chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_FILE"
  chmod 0600 "$CONFIG_FILE"
fi

systemctl daemon-reload
# `enable --now` starts a stopped service but does NOTHING to a running one, so
# on an upgrade the freshly installed binary would sit on disk while the old
# process kept playing. Restart explicitly when it is already up.
if systemctl is-active --quiet samo-radio; then
  say "restarting samo-radio onto the new binary"
  systemctl restart samo-radio
else
  systemctl enable --now samo-radio
fi

# Give it a moment to bind before anything reports on it.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  systemctl is-active --quiet samo-radio && break
  sleep 0.5
done

# A muted card is the commonest reason a fresh install plays into silence: the
# daemon reports "playing", the position advances, nothing comes out. This
# unmutes and raises ONLY controls that are muted or at zero — a level somebody
# set is a decision, and an installer that resets it every upgrade is worse
# than one that does nothing.
say "checking the mixer is not muted"
sudo -u "$SERVICE_USER" "${BIN_DIR}/samo-radio" --config "$CONFIG_FILE" --unmute || \
  note "could not adjust the mixer — check 'alsamixer' by hand if you hear nothing"

say "audio outputs visible to this machine:"
sudo -u "$SERVICE_USER" "${BIN_DIR}/samo-radio" --config "$CONFIG_FILE" --devices || true

cat <<EOF

$(say "samo-radio is running")

  control API : http://$(grep -o '"listenAddr": *"[^"]*"' "$CONFIG_FILE" | cut -d'"' -f4)
  control token: $(grep -o '"controlToken": *"[^"]*"' "$CONFIG_FILE" | cut -d'"' -f4)
  state file  : $CONFIG_FILE
  logs        : journalctl -u samo-radio -f

Next: open Samo's web UI → RADIO → SAMO-RADIO → + ADD DEVICE, and paste the
control token above. Pick the output device and a default channel there; you do
not need to come back to this shell.
EOF
