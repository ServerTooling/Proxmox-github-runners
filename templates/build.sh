#!/usr/bin/env bash
# Builds one runner template image: applies templates/<name>/template.yaml with Ansible to a Docker
# container started from the template's base image, runs its smoke checks, and exports the root
# filesystem as a .tar.zst that Proxmox can use directly as an LXC template (vztmpl).
#
#   templates/build.sh <name> <out-dir>
#
# Needs docker, zstd, python3 with PyYAML, and ansible-playbook with the community.docker and
# community.general collections (the `ansible` pip package bundles both). Environment:
#   RUNNER_VERSION, RUNNER_SHA256   actions/runner to install (defaults: packaging/runner-version and
#                                   the hash of the downloaded tarball, for local builds only)
#   SKIP_ROLES                      comma-separated roles to leave out (local testing)
#   BUILD_ID                        recorded in template.json (CI passes the release tag)
set -euo pipefail

name=${1:?usage: build.sh <name> <out-dir>}
out=${2:?usage: build.sh <name> <out-dir>}
root=$(cd "$(dirname "$0")/.." && pwd)
def="$root/templates/$name/template.yaml"
[ -f "$def" ] || { echo "no template definition at $def" >&2; exit 1; }

case "$(uname -m)" in
  x86_64) arch=amd64; rarch=x64 ;;
  aarch64) arch=arm64; rarch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

base=$(python3 -c 'import sys, yaml; print(yaml.safe_load(open(sys.argv[1]))["base"])' "$def")

RUNNER_VERSION=${RUNNER_VERSION:-$(tr -d ' \n' < "$root/packaging/runner-version")}
if [ -z "${RUNNER_SHA256:-}" ]; then
  echo "RUNNER_SHA256 not set; trusting the downloaded tarball (fine for a local test, never for a release)" >&2
  RUNNER_SHA256=$(curl -fsSL "https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/actions-runner-linux-$rarch-$RUNNER_VERSION.tar.gz" | sha256sum | cut -d' ' -f1)
fi
skip_json=$(python3 -c 'import sys, json; print(json.dumps([r for r in sys.argv[1].split(",") if r]))' "${SKIP_ROLES:-}")

tmp=$(mktemp -d)
python3 -c 'import sys, yaml, json; json.dump(yaml.safe_load(open(sys.argv[1])).get("vars") or {}, open(sys.argv[2], "w"))' "$def" "$tmp/vars.json"

ctr="pve-runners-build-$name-$$"
cleanup() { docker rm -f "$ctr" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

docker pull -q "$base" >/dev/null
docker run -d --name "$ctr" --hostname runner-template "$base" sleep infinity >/dev/null
# Ansible needs Python in the target; everything else comes from the roles.
docker exec "$ctr" sh -c 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends python3 >/dev/null'

ANSIBLE_HOST_KEY_CHECKING=False ANSIBLE_NOCOLOR=1 ansible-playbook \
  -i "$ctr," -c community.docker.docker "$root/templates/playbook.yml" \
  -e "template_file=$def" -e "@$tmp/vars.json" -e "runner_version=$RUNNER_VERSION" -e "runner_sha256=$RUNNER_SHA256" \
  -e "{\"skip_roles\": $skip_json}" \
  -e 'ansible_python_interpreter=/usr/bin/python3'

# Smoke checks: each must pass, and its first output line is recorded as the tool's version.
tools="{}"
while IFS=$'\t' read -r role c; do
  if [[ ",${SKIP_ROLES:-}," == *",$role,"* ]]; then echo "smoke: skipped $c"; continue; fi
  if ! v=$(docker exec "$ctr" bash -lc "$c" 2>&1); then
    echo "smoke check failed: $c: $v" >&2; exit 1
  fi
  v=${v%%$'\n'*}
  echo "smoke: $c -> $v"
  tools=$(python3 -c 'import sys, json; d=json.loads(sys.argv[1]); d[sys.argv[2]]=sys.argv[3]; print(json.dumps(d))' "$tools" "$c" "$v")
done < <(python3 -c 'import sys, yaml
for role, cmds in yaml.safe_load(open(sys.argv[1]))["smoke"].items():
    for c in cmds: print(role + "\t" + c)' "$def")

# Cleanup runs here rather than as a role because Ansible executes out of /tmp and /root/.ansible.
docker exec "$ctr" sh -c 'apt-get clean
  rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /root/.cache /root/.ansible /root/.npm /opt/actions-runner/_diag
  find /var/log -type f -exec truncate -s 0 {} +
  rm -f /.dockerenv'
docker stop -t 1 "$ctr" >/dev/null
mkdir -p "$out"
img="$out/$name-$arch.tar.zst"
# Proxmox reads plain rootfs tarballs; Docker's injected hostname/hosts/resolv.conf are rewritten by
# Proxmox at container start.
docker export "$ctr" | zstd -q -T0 -19 -o "$img" -f

python3 -c 'import sys, json, datetime
out, name, arch, base, build, runner, skipped, tools = sys.argv[1:]
json.dump({"name": name, "arch": arch, "base": base, "build": build,
           "built_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
           "runner_version": runner, "skipped_roles": json.loads(skipped), "tools": json.loads(tools)},
          open(out, "w"), indent=2)' \
  "$out/$name-$arch.json" "$name" "$arch" "$base" "${BUILD_ID:-local}" "$RUNNER_VERSION" "$skip_json" "$tools"
echo "built $img ($(du -h "$img" | cut -f1))"
