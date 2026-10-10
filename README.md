# proxmox-github-runners

Ephemeral GitHub Actions self-hosted runners on Proxmox VE.

`pve-runners` is a single binary that runs as a root service on a Proxmox host. For the private
repositories of the GitHub organizations you choose, it creates an LXC container from a runner image
built in this repo's CI (Ubuntu 24.04 with Python, Go, Node, Helm, kubectl and Docker), registers it as a single-use (JIT) runner, and destroys it as soon as the job finishes.
Runners get internet access but no access to your LAN. The service keeps itself updated from this
repository's releases.

> Status: Phase 3. Runners start automatically for queued jobs, from CI-built images. See
> [docs/plan.md](docs/plan.md).

## How it works

- **GitHub:** setup creates one private GitHub App per org you manage (GitHub's App-manifest flow,
  so all permissions are pre-filled) and stores its key in `/etc/pve-runners/apps/`. Nothing
  connects into your network; the tool only makes outbound calls.
- **Autoscaling:** each pool becomes a GitHub *runner scale set* per org, named after its first
  label. The service holds a long-poll on GitHub's job queue (outbound HTTPS only), claims queued
  jobs from private repositories, and starts one runner per assigned job, up to the pool's `max`
  and `proxmox.max_runners`. Restarting the service picks up the runners already running.
- **Runners:** `pct clone` of your template into the reserved ID range, tagged `gh-runner`, firewall
  on, single-use runner config pushed in, runner started. The runner takes one job and powers the
  container off; the reconcile loop destroys it within 30 seconds.
- **Images:** [templates/](templates/) defines runner images with Ansible. CI builds, smoke-tests,
  signs and publishes them as `template-<name>-<date>-<run>` releases. The host downloads the newest
  one, checks the signature and checksum, keeps it in the Proxmox template cache and creates a
  template container from it (IDs 8900-8999). Old images age out after `images.retention_days`.
- **Safety:** the tool only ever modifies containers that carry its tag *and* sit in its ID range.
  Public repositories are refused (runner groups that allow them are rejected, and repo-level
  runners check visibility). Runners can't reach private, link-local or CGNAT addresses, only DNS
  and DHCP on the LAN.

## Install

On the Proxmox host, as root:

```sh
curl -fsSL https://github.com/ServerTooling/proxmox-github-runners/releases/latest/download/install.sh | sh
```

The installer writes `/etc/pve-runners/config.yaml` with every setting at its default and a comment
explaining each one (an existing config is never overwritten). Then:

1. **Firewall:** in Datacenter > Firewall > Options, set **Input Policy** to `ACCEPT` unless you
   already manage host rules, then set **Firewall** to `Yes`. Runner isolation needs the firewall on,
   but it comes from rules pve-runners puts on each runner, not from the input policy; with `DROP`,
   turning the firewall on can lock you out of the host.
2. **Connect GitHub:** run `pve-runners setup --open-firewall` and open the first printed link (the
   address of the Proxmox web UI). `--open-firewall` lets port 8765 through the Proxmox firewall
   until setup finishes; it is only needed when the input policy is `DROP`. For each org, click
   through "Create GitHub App" and "Install" on github.com. Make sure the org's runner group does not
   allow public repositories. Leave the template field empty to use the published `ubuntu-latest`
   image.
3. **Start:** `systemctl enable --now pve-runners`. The service downloads the image and creates its
   template right away (`pve-runners images list` shows it).
4. **Use it:** `runs-on: proxmox` in a workflow of a private repo. The job shows as queued, a runner
   starts within seconds of the container booting, and it is destroyed after the job. Use the bare
   label: scale sets match `runs-on` by name, so `[self-hosted, proxmox]` won't pick them up.

Each scale set can be served by one host at a time. To run several hosts for the same org, give
each host's pools a different `scale_set` (or labels).

## Commands

| Command | What it does |
| --- | --- |
| `pve-runners setup` | One-time setup page; re-run to add orgs |
| `pve-runners run` | The service (systemd runs it): autoscaling, cleanup, images, self-update |
| `pve-runners spawn --owner ORG [--pool default] [--repo R]` | Start one ephemeral runner by hand |
| `pve-runners status` | Runners on this host and pending cleanup |
| `pve-runners reconcile` | Destroy finished or expired runners now |
| `pve-runners images list\|pull\|prune` | Show image templates, fetch pools' images now, age out old ones |
| `pve-runners template prepare --ctid ID` | Make a custom runner template from a container you built |
| `pve-runners config validate` | Check `/etc/pve-runners/config.yaml` |
| `pve-runners update [--now]` | Check for, or install, a newer release |

Configuration reference: [packaging/config.example.yaml](packaging/config.example.yaml). The default
runner is 4 cores, 8 GB RAM and a 40 GB disk, with at most 4 runners per host
(`proxmox.max_runners`).

## Updates

The service checks this repo's releases every 15 minutes. A newer build is downloaded, its
`SHA256SUMS` signature is checked against the key compiled into the running binary, the binary is
swapped (the old one kept as `.prev`), and the service restarts. If the new build doesn't report
healthy within 90 seconds it is rolled back and skipped. `update.channel: stable` follows `v*` tags
only.

## Releases (maintainers)

Every push to `main` publishes `build-<n>`; a `v*` tag publishes a stable release. One repository
secret is required, `UPDATE_SIGNING_KEY`, an ed25519 private key in PEM:

```sh
openssl genpkey -algorithm ed25519 -out signing.pem   # paste the file's contents into the secret
```

Keep `signing.pem` offline or delete it after adding the secret. Rotating the key requires
reinstalling hosts with `install.sh`, because hosts trust the key their running binary was built
with.

Before anything is published, `scripts/privacy-scan.sh` checks the source and the release files for
private IP addresses, home hostnames, emails and credentials. This repository is public; use
`192.0.2.x` and placeholders in docs and tests.

## Development

```sh
go test ./...          # unit + integration tests against in-repo fakes (no Proxmox or GitHub needed)
go vet ./...
scripts/privacy-scan.sh
```

## License

Apache 2.0, see [LICENSE](LICENSE).
