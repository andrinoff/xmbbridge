#!/usr/bin/env bash
#
# Installs XMBridge on an Ubuntu server as a systemd service.
#
# Run from the repository root, as root:
#
#   sudo ./deploy/install-ubuntu.sh config.yaml
#
# It installs ffmpeg, builds the binary, creates a dedicated system user,
# installs the config and service, then starts the bridge. Secrets are read
# from /etc/xmbbridge/xmbbridge.env, which is created empty on the first run.
#
# Safe to re-run: an existing config, env file, and database are left alone.

set -euo pipefail

SERVICE_NAME=xmbbridge
INSTALL_DIR=/var/lib/xmbbridge
CONFIG_DIR=/etc/$SERVICE_NAME
CONFIG_DST=$CONFIG_DIR/config.yaml
ENV_FILE=$CONFIG_DIR/$SERVICE_NAME.env
BIN=/usr/local/bin/bridge
GO_VERSION=1.27.1

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

CONFIG_SRC="${1:-config.yaml}"

[[ $(id -u) -eq 0 ]] || die "run this script with sudo"
command -v apt-get >/dev/null || die "this installer targets Ubuntu/Debian (apt-get not found)"

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[[ -f $REPO_ROOT/go.mod ]] || die "go.mod not found in $REPO_ROOT; run the script from the repository"
[[ -f $CONFIG_SRC ]] || die "config file not found: $CONFIG_SRC (copy config.example.yaml and fill it in)"

log "Installing runtime dependencies (ffmpeg, ca-certificates)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ffmpeg ca-certificates curl tar >/dev/null

# --- Go toolchain -----------------------------------------------------------
# Only fetch the official toolchain when the one on PATH cannot build the
# module; on a fresh Ubuntu the packaged Go is usually older than go.mod wants.
needs_go_toolchain() {
  command -v go >/dev/null 2>&1 || return 0
  (cd "$REPO_ROOT" && go build ./... >/dev/null 2>&1) || return 0
  return 1
}

if needs_go_toolchain; then
  arch=$(dpkg --print-architecture)   # amd64 or arm64
  tarball=/tmp/go-${GO_VERSION}.linux-${arch}.tar.gz
  log "Installing the Go ${GO_VERSION} toolchain for linux/${arch}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o "$tarball" \
    || die "could not download the Go toolchain; install Go ${GO_VERSION}+ manually and re-run"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tarball"
  rm -f "$tarball"
fi
export PATH=/usr/local/go/bin:$PATH

# --- Build ------------------------------------------------------------------
log "Building $(basename "$BIN")"
(
  cd "$REPO_ROOT"
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/bridge
)
chmod 0755 "$BIN"

# --- User and directories ---------------------------------------------------
if id "$SERVICE_NAME" >/dev/null 2>&1; then
  log "System user $SERVICE_NAME already exists"
else
  log "Creating system user $SERVICE_NAME"
  useradd --system --home-dir "$INSTALL_DIR" --create-home \
          --shell /usr/sbin/nologin "$SERVICE_NAME"
fi
install -d -m 0755 -o "$SERVICE_NAME" -g "$SERVICE_NAME" "$INSTALL_DIR"
install -d -m 0755 "$CONFIG_DIR"

if [[ -f $CONFIG_DST ]]; then
  log "Keeping the existing $CONFIG_DST"
else
  log "Installing config to $CONFIG_DST"
  install -m 0600 -o "$SERVICE_NAME" -g "$SERVICE_NAME" "$CONFIG_SRC" "$CONFIG_DST"
fi

if [[ -f $ENV_FILE ]]; then
  log "Keeping the existing $ENV_FILE"
else
  log "Creating empty $ENV_FILE for secrets"
  install -m 0600 -o "$SERVICE_NAME" -g "$SERVICE_NAME" /dev/null "$ENV_FILE"
fi

# --- systemd ----------------------------------------------------------------
log "Installing the systemd unit"
install -m 0644 "$REPO_ROOT/deploy/$SERVICE_NAME.service" "/etc/systemd/system/$SERVICE_NAME.service"
systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME" >/dev/null

sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
  log "Service is running"
else
  warn "Service is not running; recent log output follows"
fi

systemctl --no-pager --full status "$SERVICE_NAME" | head -n 12 || true
echo
log "Recent log output"
journalctl -u "$SERVICE_NAME" -n 20 --no-pager || true
echo
cat <<EOF
Next steps
  1. Put your credentials in $ENV_FILE, for example:
       XMBBRIDGE_MASTODON_ACCESS_TOKEN=...
       XMBBRIDGE_BLUESKY_APP_PASSWORD=...
       XMBBRIDGE_TWITTER_BEARER_TOKEN=...
     then restart: sudo systemctl restart $SERVICE_NAME
  2. Verify credentials without bridging:
       sudo systemctl stop $SERVICE_NAME
       sudo -u $SERVICE_NAME $BIN -config $CONFIG_DST -check
       sudo systemctl start $SERVICE_NAME
  3. Follow the logs:   journalctl -u $SERVICE_NAME -f
  4. State and media live in $INSTALL_DIR
EOF
