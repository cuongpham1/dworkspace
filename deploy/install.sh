#!/bin/sh
# Installs dworkspace as a systemd service. Run as root on the target machine:
#   ./deploy/install.sh ./dworkspace
set -e

BIN="${1:-./dworkspace}"
[ -f "$BIN" ] || { echo "usage: $0 <path-to-dworkspace-binary>"; exit 1; }

id dworkspace >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin dworkspace
install -d /opt/dworkspace
install -m 755 "$BIN" /opt/dworkspace/dworkspace
install -d -o dworkspace -g dworkspace /opt/dworkspace/data
install -m 644 "$(dirname "$0")/dworkspace.service" /etc/systemd/system/dworkspace.service
systemctl daemon-reload
systemctl enable --now dworkspace

echo "dworkspace is running on port 80."
