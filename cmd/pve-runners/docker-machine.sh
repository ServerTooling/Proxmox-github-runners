#!/bin/sh
# Prepares this machine to run pve-runners docker pools: installs Sysbox (jobs get Docker inside their
# runner without privileges here) and adds the user {{.User}}, which only the Proxmox host's key can log
# in as. Run as root. Safe to run again.
set -eu
user={{q .User}}
key={{q .Key}}

command -v docker >/dev/null || { echo "Docker is not installed here (on UGOS, install the Docker app first)"; exit 1; }

if ! docker info --format '{{"{{"}}range $k, $v := .Runtimes{{"}}"}}{{"{{"}}$k{{"}}"}} {{"{{"}}end{{"}}"}}' | grep -qw sysbox-runc; then
  if [ -n "$(docker ps -q)" ]; then
    echo "Installing Sysbox restarts Docker. Stop the running containers first (docker ps lists them), then run this again."
    exit 1
  fi
  curl -fsSL -o /tmp/sysbox-ce.deb {{q .SysboxURL}}
  echo {{q .SysboxSHA256}}'  /tmp/sysbox-ce.deb' | sha256sum -c -
  apt-get update
  apt-get install -y /tmp/sysbox-ce.deb
  rm -f /tmp/sysbox-ce.deb
fi

getent group docker >/dev/null || groupadd docker
id "$user" >/dev/null 2>&1 || useradd -m -s /bin/sh "$user"
usermod -aG docker "$user"
home=$(getent passwd "$user" | cut -d: -f6)
install -d -m 700 -o "$user" -g "$user" "$home/.ssh"
# restrict: commands only, no terminal or forwarding.
printf 'restrict %s\n' "$key" > "$home/.ssh/authorized_keys"
chown "$user:$user" "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"

sock=$(stat -c %G /var/run/docker.sock)
[ "$sock" = docker ] || echo "warning: /var/run/docker.sock belongs to group $sock, not docker; $user may not reach Docker"
docker info --format '{{"{{"}}range $k, $v := .Runtimes{{"}}"}}{{"{{"}}$k{{"}}"}} {{"{{"}}end{{"}}"}}' | grep -qw sysbox-runc
echo "Done: Sysbox is installed and $user can use Docker."
