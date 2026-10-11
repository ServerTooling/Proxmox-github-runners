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
| `pve-runners logs ship` | Send new log lines to Loki now, to test `logs:` settings |
| `pve-runners cache list\|clear` | Show the shared runner caches' sizes, or empty the idle ones |

Configuration reference: [packaging/config.example.yaml](packaging/config.example.yaml). The default
runner is 4 cores, 8 GB RAM and a 40 GB disk, with at most 4 runners per host
(`proxmox.max_runners`).

## Faster job starts

Two pool settings cut the wait before a job's first step. Every job still gets a fresh container.

- `warm: N` keeps N spare runners started after a job, so the next job takes one at once instead of
  waiting for a clone and boot. Once no job has come for `warm_for` (30m by default) the spares are
  removed. `min: N` instead keeps N ready at all times. A runner is only removed after GitHub agrees
  to unregister it, which it refuses while the runner has a job.
- `cache: true` mounts a host folder (`cache.dir`, one subfolder per org or account) at
  `/opt/pve-cache` in each runner and points the actions tool cache (`setup-node`, `setup-go`, ...),
  npm, yarn, Go, pip and Playwright there, so these downloads are kept between jobs. Every repo of
  that org shares it, so a job could leave files that a later job of another repo picks up; turn it on
  only for orgs whose repos you trust with each other. A folder over `cache.max_gb` (50 by default) is
  emptied once none of that org's runners are running; `pve-runners cache clear` empties them now.
  The folder lives on the host's own disk unless `cache.dir` points elsewhere, such as a ZFS dataset
  with a quota. Turn off the built-in caching of `setup-go`, `setup-node` and `setup-python`
  (`cache: false`) in workflows that run here: the shared folder already holds what they would restore,
  and their restore unpacks an archive over files that jobs running beside it have open, which has
  crashed Go builds with `SIGBUS`.

## Settings page

The service serves a settings page at `https://<this host>:8765`, using the same certificate as the
Proxmox web UI. Log in with a Proxmox account that has `Sys.Modify` (such as `root@pam`). The login stays until you log
out or don't use the page for 7 days, including across restarts and updates (hashes of the session IDs are
kept in `/etc/pve-runners/web-sessions.json`, mode 0600). The page covers
every setting: host limits, network, images, updates, alerts, logs, each pool, and the connected orgs,
including adding an org. It also takes the notify webhook URL and the Loki token, which are written to
their own 0600 files and never shown again. Saving validates the whole config, writes it and restarts the
service. Buttons run what the CLI does (clean up, start a runner, list, pull or prune images, check for
or install an update, ship logs, show or clear the runner caches, show the config file, prepare a template), with their output on the
page; the status at the top is `pve-runners status`. Set `web.enabled: false` to turn the page off, or `web.listen` to move it. With the Proxmox
firewall on and its input policy set to DROP, allow the port on the host.

## Logs

The service logs to the journal (`journalctl -u pve-runners`). To read logs away from the host, set
`logs:` to ship them to Grafana Loki (a free Grafana Cloud stack works): the service's own log and the
`gh-runner` log inside each running runner, which is otherwise lost when the runner is destroyed. On
Grafana Cloud, create an access policy token with `logs:write`, then:

```bash
install -m 600 /dev/stdin /etc/pve-runners/loki-token   # paste the token, then Ctrl-D
```

```yaml
logs:
  url: https://logs-prod-000.grafana.net   # your stack's Loki URL
  user: "123456"                           # your stack's Loki user ID
  token_file: /etc/pve-runners/loki-token
```

Run `pve-runners logs ship` to check it, then `systemctl reload pve-runners`. Lines are labelled
`service="pve-runners"`, `host`, `unit` (`pve-runners` or `gh-runner`) and, for runners, `vmid`,
`runner` and `pool`. Anything that looks like a token is masked before it leaves the host.

### Runner usage

With logs on, each running runner's CPU, memory, disk and network use is also sent every
`logs.interval`, as JSON lines in the `service="pve-runners-usage"` stream (labels `host`, `vmid`,
`runner`, `pool`, and `repo` once the runner has a job; the job name, workflow and run ID are in the
line). The figures are the ones Proxmox shows for the container, so nothing runs inside the runner,
and the same logs token works. `logs.usage: false` turns it off.

Import [docs/grafana/runner-usage.json](docs/grafana/runner-usage.json) (Dashboards > New > Import) for
graphs per runner and a table per job of peak and average CPU, peak memory and peak disk. To query
it yourself:

```
max by (runner) (max_over_time({service="pve-runners-usage"} | json | unwrap mem_bytes [1m]))
```

## Network troubleshooting

If jobs stall in the middle of downloads (npm `ETIMEDOUT`, a checkout or `actions/cache` hanging for
10 minutes), check that the host doesn't also hold the runners' gateway address on their bridge. With IP
forwarding off, runners that resolve the gateway to the host lose their packets until the ARP entry
flips back. The service checks this against a running runner and logs a warning (also shown in
`pve-runners status`); the fix is on the host: make the bridge `inet manual` in
`/etc/network/interfaces` and run `ifreload -a`. Clones and destroys that time out on Proxmox's
storage lock while a slow ZFS destroy holds it are retried.

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
