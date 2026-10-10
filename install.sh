#!/bin/sh
# Installs (or reinstalls) pve-runners on a Proxmox VE host, then runs first-time setup.
#   curl -fsSL https://github.com/ServerTooling/proxmox-github-runners/releases/latest/download/install.sh | sudo sh
# After this, the service keeps itself updated from this repo's releases.
set -eu

REPO="ServerTooling/proxmox-github-runners"
BASE="https://github.com/$REPO/releases/latest/download"

[ "$(id -u)" -eq 0 ] || { echo "Run as root (sudo)." >&2; exit 1; }
command -v pct >/dev/null 2>&1 || { echo "pct not found: this must run on a Proxmox VE host." >&2; exit 1; }
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "Downloading pve-runners ($ARCH)..."
curl -fsSL -o "$TMP/pve-runners-linux-$ARCH" "$BASE/pve-runners-linux-$ARCH"
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
# The first install trusts GitHub's TLS for SHA256SUMS; every later self-update also verifies the
# ed25519 signature with the key compiled into the binary.
(cd "$TMP" && grep " pve-runners-linux-$ARCH\$" SHA256SUMS | sha256sum -c -) || { echo "Checksum mismatch." >&2; exit 1; }

install -m 0755 "$TMP/pve-runners-linux-$ARCH" /usr/local/bin/pve-runners
install -d -m 0700 /etc/pve-runners /etc/pve-runners/apps /var/lib/pve-runners
/usr/local/bin/pve-runners version

cat > /etc/systemd/system/pve-runners.service <<'UNIT'
[Unit]
Description=pve-runners: ephemeral GitHub Actions runners on Proxmox VE
Documentation=https://github.com/ServerTooling/proxmox-github-runners
Wants=network-online.target
After=network-online.target pve-cluster.service

[Service]
Type=simple
ExecStart=/usr/local/bin/pve-runners run
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload

# Writes every setting with its default and an explanation; an existing non-empty config is kept.
/usr/local/bin/pve-runners config init

if /usr/local/bin/pve-runners config validate | grep -q ': 0 targets'; then
  echo
  echo "Settings: /etc/pve-runners/config.yaml (every option is listed there with its default)."
  echo "Next, connect GitHub:"
  echo "  1. Turn on the datacenter firewall (Datacenter > Firewall > Options); runners stay off the LAN through it."
  echo "  2. Run: pve-runners setup   (prints a link to open in your browser)"
  echo "  3. Run: systemctl enable --now pve-runners"
else
  systemctl enable pve-runners >/dev/null 2>&1
  systemctl restart pve-runners
  echo "pve-runners updated and restarted."
fi
