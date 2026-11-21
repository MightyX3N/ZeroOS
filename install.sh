#!/usr/bin/env bash
# ZeroOS installer for Debian 13 (also Debian 12, Ubuntu Server and Raspberry Pi OS 64-bit).
#
#   curl -fsSL https://raw.githubusercontent.com/MightyX3N/ZeroOS/main/install.sh | sudo bash
#
# Every step is numbered and explained, so you can read it first or do the
# steps by hand (the README has the same steps under "Manual install").
set -euo pipefail

REPO="MightyX3N/ZeroOS"
BIN="/usr/local/bin/zeroos"
say()  { printf '\n\033[1;36m%s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# --- Step 1: check this computer -------------------------------------------
say "Step 1 of 6: Checking this computer"
[ "$(id -u)" -eq 0 ] || fail "Run this with sudo."
. /etc/os-release 2>/dev/null || fail "This doesn't look like a Linux system ZeroOS knows."
case "${ID:-} ${ID_LIKE:-}" in
  *debian*|*ubuntu*) ;;
  *) fail "ZeroOS supports Debian, Ubuntu Server and Raspberry Pi OS. This is ${PRETTY_NAME:-unknown}." ;;
esac
case "$(uname -m)" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) fail "ZeroOS needs a 64-bit Intel/AMD or ARM computer. This is $(uname -m)." ;;
esac
echo "✓ ${PRETTY_NAME}, ${ARCH}"

# --- Step 2: small tools ZeroOS needs --------------------------------------
# cifs-utils: connects NAS shares (SMB).  avahi-daemon: makes http://<name>.local work.
say "Step 2 of 6: Installing the tools ZeroOS needs"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl cifs-utils avahi-daemon >/dev/null
echo "✓ Tools installed"

# --- Step 3: Docker, from Docker's own package source ----------------------
say "Step 3 of 6: Installing Docker"
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  echo "✓ Docker is already installed"
else
  DOCKER_OS="$ID"
  case "$ID" in debian|ubuntu) ;; raspbian) DOCKER_OS=debian ;; *) case "${ID_LIKE:-}" in *ubuntu*) DOCKER_OS=ubuntu ;; *) DOCKER_OS=debian ;; esac ;; esac
  CODENAME="${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}"
  [ -n "$CODENAME" ] || fail "Couldn't tell which release of ${PRETTY_NAME} this is."
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/${DOCKER_OS}/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${DOCKER_OS} ${CODENAME} stable" > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin >/dev/null
  echo "✓ Docker installed"
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then systemctl enable --now docker >/dev/null 2>&1 || true; fi

# --- Step 4: the ZeroOS program --------------------------------------------
# One file. It is downloaded from the newest GitHub release and checked
# against that release's checksum before it is installed.
say "Step 4 of 6: Installing ZeroOS"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [ -n "${ZEROOS_BINARY:-}" ]; then
  cp "$ZEROOS_BINARY" "$TMP/zeroos"                       # used by the USB installer and for testing
else
  BASE="https://github.com/${REPO}/releases/latest/download"
  curl -fsSL "${BASE}/zeroos-linux-${ARCH}" -o "$TMP/zeroos-linux-${ARCH}" || fail "Couldn't download ZeroOS. Is this computer online, and does ${REPO} have a release?"
  curl -fsSL "${BASE}/checksums.txt" -o "$TMP/checksums.txt" || fail "Couldn't download the checksums."
  (cd "$TMP" && grep " zeroos-linux-${ARCH}\$" checksums.txt | sha256sum -c - >/dev/null) || fail "The download didn't match its checksum, so nothing was installed."
  mv "$TMP/zeroos-linux-${ARCH}" "$TMP/zeroos"
fi
install -m 0755 "$TMP/zeroos" "$BIN"
echo "✓ ZeroOS $("$BIN" version) installed at $BIN"

# --- Step 5: start ZeroOS now and on every boot -----------------------------
say "Step 5 of 6: Starting ZeroOS"
mkdir -p /var/lib/zeroos /mnt/zeroos
cat > /etc/systemd/system/zeroos.service <<'UNIT'
[Unit]
Description=ZeroOS
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/zeroos serve
Restart=always
RestartSec=3
# Apps keep running when ZeroOS itself restarts or updates.
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
  systemctl enable zeroos >/dev/null 2>&1
  systemctl restart zeroos
  for _ in $(seq 1 30); do "$BIN" web-address >/dev/null 2>&1 && break; sleep 1; done
  "$BIN" web-address >/dev/null 2>&1 || fail "ZeroOS didn't start. See why with: journalctl -u zeroos -n 50"
  echo "✓ ZeroOS is running and starts by itself after a restart"
else
  echo "! This system has no systemd, so ZeroOS wasn't started. Start it with: zeroos serve"
fi

# --- Step 6: where to go next ------------------------------------------------
say "Step 6 of 6: Done"
if "$BIN" web-address >/dev/null 2>&1; then
  echo "Open one of these in a browser on your phone or PC:"
  "$BIN" web-address | sed 's/^/    /'
  if "$BIN" setup-code >/dev/null 2>&1; then
    echo
    "$BIN" setup-code
    echo "The web page asks for this code once, so nobody else can claim your server."
  fi
fi
