# Runner images

Each directory here with a `template.yaml` is a runner image. CI (`.github/workflows/templates.yml`)
builds it, smoke-tests it, signs it and publishes it as a `template-<name>-<yyyymmdd>-<run>` release.
Hosts whose pools say `image: <name>` download the newest release, verify it, and create a Proxmox
template container from it.

| Image | Base | Tools |
| --- | --- | --- |
| `ubuntu-latest` | Ubuntu 24.04 (follows GitHub's `ubuntu-latest`) | Python 3 + pip/venv/pipx, Go, Node.js LTS + npm/corepack, Helm, kubectl, Docker Engine + Buildx/Compose, git, gh, jq, build-essential |

## How a build works

`build.sh <name> <out-dir>`:

1. Starts the image's `base` in Docker and applies its `roles` with Ansible over the docker connection
   (`playbook.yml`). The template's `vars` set tool versions; `latest`, `lts` and `stable` resolve
   at build time.
2. Runs every `smoke` command inside the container; any failure stops the build.
3. Clears caches and logs, then `docker export | zstd` into `<name>-<arch>.tar.zst`, a root
   filesystem tarball that Proxmox uses directly as an LXC template.
4. Writes `<name>-<arch>.json` with the build ID, runner version and the version each smoke command
   reported.

CI then signs `SHA256SUMS` with the same key as tool releases. Pull requests build and smoke-test
without publishing; `main`, a weekly rebuild (security updates, new tool versions) and manual runs
publish.

## Adding or changing an image

- New tool: add `roles/<tool>/tasks/main.yml` (and `defaults/main.yml` for its version), list it in
  `roles:` and add a smoke check under its name. Verify every download against the publisher's
  checksum.
- New image: copy `ubuntu-latest/` to a new directory and trim its roles. Pools pick it with
  `image: <dir-name>`.
- The `base`, `actions_runner` and `docker` roles are what make a working runner; keep them in every
  image.

Local build (needs docker, zstd and `pip install 'ansible==12.*'`):

```sh
SKIP_ROLES=go,helm templates/build.sh ubuntu-latest /tmp/out
```

`SKIP_ROLES` leaves out roles (and their smoke checks) when a download host isn't reachable. Without
`RUNNER_SHA256` set, the script trusts the downloaded runner tarball, which is fine locally but never
for a release; CI cross-checks it against GitHub's release notes.
