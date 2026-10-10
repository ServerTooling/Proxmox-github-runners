# Proxmox GitHub Runner Autoscaler: Plan

Repo: `ServerTooling/proxmox-github-runners` (public). Max runners: 4 by default, set per host with `proxmox.max_runners` in `/etc/pve-runners/config.yaml`. Runner OS follows GitHub's `ubuntu-latest` (24.04 today; the template's base moves when GitHub's does). Ephemeral runners only for now; `mode: permanent` is a reserved config value that validation rejects until a later phase. Default label: `proxmox`. Default runner size: 4 cores, 8 GB RAM, 40 GB disk (per pool: `cores`, `memory_mb`, `disk_gb`). Only private repos in orgs you manage; runners get internet but no LAN.

A single Go binary (`pve-runners`, name TBD) running as a root systemd service on the Proxmox host. It watches GitHub Actions demand for one or more organizations and creates LXC containers or VMs as self-hosted runners: ephemeral (one job, then destroyed); permanent runners are reserved for later.

## 0. Where it lives: `ServerTooling/proxmox-github-runners` (public)

Code, CI, releases and template images all live in the public repo (decided 2026-10-10; it replaced the earlier "folder in DellServerTools" idea).

```
proxmox-github-runners/
├── cmd/pve-runners/          # the binary: setup, run, spawn, reconcile, status, update, template ...
├── internal/config/          # YAML load, validate, migrate
├── internal/pve/             # pct/qm/pvesh command builders + Runner interface (exact-argv tests)
├── internal/github/          # App JWT, installation tokens, manifest conversion, JIT configs
├── internal/setup/           # first-run setup page (App manifest flow, install picker)
├── internal/provision/       # clone → tag → inject JIT → start
├── internal/reconcile/       # destroy finished/expired runners
├── internal/update/          # self-update with signature check and rollback
├── internal/runnerpkg/       # pinned actions/runner tarball download + verify
├── templates/                # runner OS images built in CI (section 3.1)
├── packaging/                # systemd unit, config example, runner-version pin
├── scripts/privacy-scan.sh   # blocks private IPs/hostnames/emails/tokens in source and releases
└── install.sh
```

Conventions carried over from DellServerTools: pure logic testable without Proxmox, every Proxmox operation built by a function with an exact-argv test, tests only against in-repo fakes, `192.0.2.x` placeholders, no new dependencies without a reason (only `yaml.v3` so far).

**Public repo rules:** nothing personal in source, issues, workflow logs or releases. CI runs `scripts/privacy-scan.sh` over the source tree on every PR and over every release file before publishing. Branch protection on `main` is free for public repos, so it's on (PR + passing CI required).

## 1. Architecture

```
GitHub (orgs A, B)  <──outbound HTTPS long-poll──  pve-runners (systemd, root)
                                                     ├─ setup (first run: GitHub login, pick orgs/repos, writes YAML)
                                                     ├─ config (YAML, hot reload on SIGHUP)
                                                     ├─ demand source (per org): scale-set listener  [or webhook receiver]
                                                     ├─ scheduler: label → pool → capacity check
                                                     ├─ provisioner: pct (LXC) / qm (VM) as root on the host
                                                     │     clone template → inject JIT config → start
                                                     ├─ reconciler loop (every 30–60s): destroy finished/stale guests,
                                                     │     remove orphaned GitHub runners, enforce TTL
                                                     └─ state: Proxmox tags/notes are the source of truth (no DB)
```

Job lifecycle (ephemeral):
1. GitHub reports a queued job for a label set (`runs-on: [self-hosted, proxmox, lxc-small]`).
2. Scheduler maps labels to a pool, checks pool max and host free RAM/CPU.
3. Provisioner asks GitHub for a JIT runner config (`POST /orgs/{org}/actions/runners/generate-jitconfig`, or the scale set equivalent).
4. Linked-clone the pool's template, tag it `gh-runner,pool-<name>,org-<org>`, inject the JIT config, start it.
5. Inside the guest, a baked-in systemd unit runs `./run.sh --jitconfig <cfg>`; the runner takes exactly one job then exits, and the unit powers the guest off.
6. Reconciler sees a stopped `gh-runner` guest and destroys it.

Permanent runners: same clone path, but registered with a registration token (`config.sh --unattended --labels ...` + `svc.sh install`) and kept at a fixed count (`min`) by the reconciler instead of being created per job.

## 2. GitHub side

**Auth: a GitHub App the tool creates for you on first run (no PAT, no copy-pasting keys).**

First-run setup, `sudo pve-runners setup` (also run automatically by `install.sh`):
1. The tool starts a temporary setup page on the host (e.g. `http://192.0.2.10:8765`, LAN only, one-time token in the URL) and prints the link. You open it in any browser on your network.
2. **Log in and create the App** with GitHub's [App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest): the page posts a manifest with every permission already filled in, you click "Create GitHub App" on github.com while logged in, and GitHub redirects back with a one-time code. The tool exchanges it for the App ID, private key and webhook secret and saves them to `/etc/pve-runners/` (0600). Pick whether the App lives on your user account or on an org.
3. **Choose orgs and repos.** The page sends you to the App's install screen, where GitHub itself asks which account/org and "All repositories" or "Only select repositories". Repeat for each org. This is GitHub's own picker, so it's always correct and you can change it later at github.com → Settings → Applications.
4. **Map to pools.** Back on the setup page (or in the terminal), the tool lists each installation it can see and asks which pools each org or repo should get, then writes `config.yaml` and starts the service.
5. Re-run `pve-runners setup` any time to add an org, change repos or pools. `pve-runners setup --headless` falls back to the [device flow](https://docs.github.com/en/apps/creating-github-apps/writing-code-for-a-github-app/building-a-cli-with-a-github-app) (shows a code to enter at github.com/login/device) for when no browser can reach the host; it still needs the App created once.

Permissions the manifest requests (all GitHub needs, nothing more):
- Organization: *Self-hosted runners: Read & write* (create JIT/registration tokens, list and delete runners).
- Repository: *Actions: Read*, *Metadata: Read*, and *Administration: Read & write* only if you want repo-level runners (needed for repos owned by a personal account, which has no org runners).

At runtime the binary signs a JWT with the App key and exchanges it per installation for 1-hour tokens (`bradleyfalzon/ghinstallation`). No long-lived personal token; access is visible and revocable per org.

"All repos in an org" → one org-level runner group with access to all (or the selected) repos. "Just these repos" → the group is restricted to them; personal-account repos get repo-level runners.

**Getting triggers: recommended is the runner scale set API, not webhooks.**
- GitHub's official Go client [`github.com/actions/scaleset`](https://pkg.go.dev/github.com/actions/scaleset) is the same protocol Actions Runner Controller uses: you register a "scale set" per org (name = label), then long-poll a message queue that tells you how many jobs are waiting and hands out JIT configs. It is **outbound only**, so the Proxmox host needs no public URL, port forward or tunnel. Covers every repo in the org that is allowed to use the runner group.
- `runs-on: <scale-set-name>` is how workflows target it. One scale set per pool per org (e.g. `proxmox-lxc-small`, `proxmox-vm-docker`).
- Alternative: `workflow_job` webhooks (App webhook, events `queued`/`completed`). Lower-level and flexible on labels, but needs an inbound HTTPS endpoint (reverse proxy, Cloudflare Tunnel, Tailscale Funnel), HMAC secret verification, and a reconcile loop anyway because webhook deliveries can be lost. Polling the REST API across all repos is the worst option (rate limits, latency).
- Prior art worth reading before building: [`ysya/runscaler`](https://pkg.go.dev/github.com/ysya/runscaler) (a scaleset-based autoscaler), and existing Proxmox projects/GARM providers, to borrow ideas or decide not to build at all.

**Runner software ("include the GitHub runner").** Two existing GitHub projects do the work, pulled in the way their languages expect rather than as git submodules:
- [`actions/runner`](https://github.com/actions/runner): the official runner agent (C#/.NET) that actually runs jobs. It runs **inside each LXC/VM**, not inside our binary, so it isn't something we can link in. The tool downloads the official release tarball for a pinned version, verifies its SHA-256, and caches it in `/var/lib/pve-runners/runner/`, then bakes it into templates (`pve-runners template build`) or pushes it into new guests with `pct push`. A git submodule of the C# source would mean building .NET on the host for no gain.
- [`actions/scaleset`](https://pkg.go.dev/github.com/actions/scaleset): GitHub's Go client for runner scale sets. Added as a Go module dependency, pinned in `go.mod`/`go.sum` (Go's equivalent of a submodule). If you want the source physically in the repo, `go mod vendor` copies it under `pve-runners/vendor/`.

**Only your orgs, only private repos (decided 2026-10-10):**
- **One private GitHub App per org (or your personal account), created by setup under that org.** A private App can only be installed on the account that owns it, and only an org owner or App manager can create one there, so the tool can never be attached to an org you don't manage. Setup repeats the one-click manifest flow per org; each App's key is stored at `/etc/pve-runners/apps/<org>.pem`.
- **Public repos are never served** (they can use GitHub-hosted runners). Enforced three ways: the org runner group must have "Allow public repositories" off or the tool refuses to register runners in it; repo-level runners are refused if the repo is public; and the autoscaler checks each queued job's repo and skips public ones (Phase 3).
- Only orgs listed in `targets` are served; anything else is ignored.

**Runner groups and repos.**
- Put runners in a dedicated org runner group. Restrict which repos can use it if not "all".
- Keep "Allow public repositories" **off**. A fork PR on a public repo can run arbitrary code on your Proxmox network.

## 3. Proxmox side

**`pct` for containers, `qm` for VMs.** The service runs as root on the Proxmox host, so it drives guests with the same commands you'd type: `pct clone/set/push/exec/start/stop/destroy` and `qm clone/set/start/stop/destroy/guest exec`. No API token or user to set up, and `pct exec`/`pct push` (which the REST API lacks) just work.
- Every command is built by a small builder (`internal/pve`) and run through one `Runner` interface. Tests assert the exact argv, the same rule DellServerTools uses for Proxmox API params, and a fake `Runner` stands in for the host. The interface also leaves room for an API backend later if the tool ever runs off-host.
- Status comes from `pvesh get /cluster/resources --type vm --output-format json` (local, no auth needed as root).
- Guests go in a resource pool `gh-runners` (`--pool`) and carry the tag `gh-runner`. The code only ever touches guests that have the tag **and** sit in a reserved VMID range (e.g. 9000–9499). Hard check in code, because root `pct destroy` can delete anything.

**Templates (built once, outside the tool, ideally scripted with Packer or a shell script):**
- LXC template: Ubuntu 24.04, unprivileged, `features: nesting=1,keyctl=1` if jobs need Docker. Pre-install the actions runner tarball, git, build tools, and a `gh-runner.service` that waits for `/etc/gh-runner/jitconfig`, runs `run.sh --jitconfig`, then `poweroff`.
- VM template: cloud-init image + `qemu-guest-agent` + same runner unit. Use VMs for anything needing real Docker, KVM, systemd-heavy tests or stronger isolation.
- Storage must support linked clones (LVM-thin, ZFS, Ceph) so clones take seconds. Directory storage falls back to full clones.
- Update templates on a schedule (runner version, OS patches); GitHub refuses runners that are too old.

**Injecting the JIT config:**
- LXC: `pct start <id>` then `pct push <id> <tmpfile> /etc/gh-runner/jitconfig` (or `pct exec <id> -- /opt/runner/run.sh --jitconfig ...` directly).
- VM: a cloud-init `cicustom` user-data snippet written to a `snippets`-enabled storage before `qm start` (no waiting on the agent), or `qm guest exec` after boot.
- Never put the JIT config in VM notes/description; it is a credential (short-lived, single use, but still).

**Cleanup (reconciler, every ~30s):**
- Destroy `gh-runner` guests that are stopped (ephemeral), older than `max_lifetime`, or stuck in creation.
- Delete GitHub runners that are offline and have no matching guest (`DELETE /orgs/{org}/actions/runners/{id}`).
- On startup, adopt existing tagged guests rather than duplicating them. State lives in Proxmox tags/notes, so a restart loses nothing.

**Capacity:** global `max_runners`, per-pool `max`, and a free-memory floor on the node so builds don't OOM the host (which on your box also runs everything else).

## 3.1 Runner template images (built in CI, pulled by the host)

Jobs need toolchains (Python, Go, Node/npm, Helm, kubectl, Docker, ...). Instead of hand-building templates, the repo defines them declaratively and CI builds them.

**Definition** (`templates/<name>/template.yaml` + Ansible roles in `templates/roles/`):
```yaml
name: ubuntu-latest
base: ubuntu:24.04            # tracks GitHub's ubuntu-latest; bumped in a PR when GitHub moves it
roles: [base, common, python, go, node, helm, kubectl, docker, actions_runner]
vars:                         # "latest"/"lts"/"stable" resolve at build time, or pin an exact version
  go_version: latest
  node_version: lts
  helm_version: latest
  kubectl_version: stable
smoke:                        # per role; every command must pass before the image is published
  go: [go version]
  node: [node --version, npm --version]
  actions_runner: [/opt/actions-runner/bin/Runner.Listener --version, systemctl is-enabled gh-runner.service]
```
Every download in the roles is checked against its publisher's checksum, and the resolved versions are recorded in the release's `<name>-<arch>.json`.
Adding a tool is a new role (or a version bump) in a PR: reviewable, repeatable, like Ansible because it *is* Ansible.

**Build (`.github/workflows/templates.yml`)**, on changes under `templates/**`, weekly for security patches, and on demand:
1. Start `ubuntu:24.04` in Docker on a GitHub-hosted runner; run the playbook against it with Ansible's docker connection.
2. Smoke-test inside it (`python3 --version`, `go version`, `node -v`, `helm version`, runner `--version`).
3. Clean (apt caches, machine-id, logs), `docker export` → `.tar.zst`, the format Proxmox accepts as an LXC OS template.
4. Privacy scan, `SHA256SUMS` + signature (same key as binaries), publish as release `template-<name>-<yyyymmdd>-<run>`. Release assets can be up to 2 GB; a full image is expected around 1–1.5 GB compressed, and CI fails if one ever exceeds the limit.

**On the host**, a pool names a template instead of a container ID:
```yaml
pools:
  default:
    image: ubuntu-latest          # latest release of this template
    # image: ubuntu-latest@template-ubuntu-latest-20261012-31   # or pin one
```
- The service checks for a newer image release (same loop as self-update). If one is newer than what's cached, it downloads it to `/var/lib/vz/template/cache/`, verifies signature + checksum, then `pct create`s a base container from it (unprivileged, `nesting=1,keyctl=1`) in a reserved template ID range (e.g. 8900–8999), tags it `gh-template;image-<name>;rel-<n>`, and runs `pct template`.
- Already pulled → reused as is; nothing is downloaded twice. New runners clone from the newest ready template.
- **Containers vs images:** runner containers are destroyed as soon as their job ends (the guest powers off, the next reconcile pass, every 30s, destroys it; a sweep also removes orphans and anything past `max_lifetime`). Downloaded images and their Proxmox templates are **not** deleted with the job.
- **Image retention:** `images.retention_days` (default 30, set per host) ages out downloaded images and their templates. Never deleted regardless of age: the newest image of each name, any image a pool is pinned to, and any template a runner was linked-cloned from and still exists.
**How a job gets the right template (labels → pool → image):**
- Each pool pairs one image with the labels workflows use. With the scale set API, each pool is registered as its own scale set per org, named after its label, so `runs-on` picks it directly.
- Default: one pool, label `proxmox`, image `ubuntu-latest` with all the common tools (Python, Go, Node/npm, Helm, kubectl, Docker). `runs-on: proxmox` is all a workflow needs.
- Optional variants are just more pools, e.g. `proxmox-go` → a slimmer image with only Go. A queued job lands on the scale set matching its label, the tool clones that pool's template, and the runner starts with the tools already installed: no install step in the workflow.

```yaml
pools:
  default:   {image: ubuntu-latest, labels: [proxmox]}
  go:        {image: ubuntu-go,     labels: [proxmox-go]}
```
```yaml
# workflow
jobs:
  build:
    runs-on: proxmox
```

- `pve-runners template prepare --ctid` stays as a manual fallback for one-off custom templates.

## 4. Config schema (draft)

```yaml
# /etc/pve-runners/config.yaml  (0600 root)
# Written by `pve-runners setup`; safe to hand-edit afterwards.
proxmox:
  node: pve1
  max_runners: 4                   # host-wide cap across all pools and orgs (per-host setting)
  vmid_range: [9000, 9499]
  pool: gh-runners
  storage: local-lvm
  snippets_storage: local          # for VM cloud-init snippets
  min_free_memory_mb: 4096

notify:
  # webhook_url_file: /etc/pve-runners/notify-url   # ntfy, Discord or Slack; off by default

network:
  isolate: true                    # internet only, no LAN (default)
  # allow: [{dest: 192.0.2.1, proto: udp, port: 53}]   # LAN exceptions; default DNS + DHCP to any LAN address

images:
  retention_days: 30               # age out downloaded template images (never the newest or one in use)

github:
  mode: scaleset                   # scaleset | webhook
  runner_version: 2.x.y            # pinned actions/runner release + sha256 below
  runner_sha256: "..."

targets:                           # one entry per App installation, from setup
  - org: my-org
    app_id: 123456                 # this org's own private App, created by setup
    installation_id: 111
    repos: all                     # all | [repo-a, repo-b]
    runner_group: proxmox
    pools: [lxc-small, vm-docker]
  - user: my-github-user           # personal account → repo-level runners
    repos: [side-project]
    pools: [lxc-small]

pools:
  lxc-small:
    type: lxc
    template: 8000
    mode: ephemeral          # ephemeral | permanent
    labels: [proxmox, linux, lxc-small]
    cores: 2
    memory_mb: 4096
    disk_gb: 20
    max: 4
    idle_warm: 0              # pre-booted spares for faster pickup
    max_lifetime: 6h
    network: { bridge: vmbr1, vlan: 50 }
  vm-docker:
    type: vm
    template: 8001
    mode: permanent            # reserved: rejected until permanent runners are built
    min: 1                     # permanent: keep this many running
    max: 2
    labels: [proxmox, linux, docker]
    cores: 4
    memory_mb: 8192
    network: { bridge: vmbr1, vlan: 50 }
```

Configuring after install: `pve-runners setup` (guided: GitHub login, orgs/repos, pools; re-runnable), `pve-runners config validate`, `config show` (secrets redacted), `pve-runners doctor` (App installs reachable, templates exist, storage supports linked clones, runner tarball checksum), `pve-runners status` (pools, guests, runners), `pve-runners template build --type lxc|vm` (makes the base templates with the runner baked in), and `systemctl reload pve-runners` for hot reload.

## 5. Security

**Runner network: internet only, no LAN (decided 2026-10-10, built in Phase 1).**
- Every runner container gets the Proxmox firewall turned on (inbound DROP, outbound ACCEPT) plus a cluster security group `gh-runner-egress` that drops all private, CGNAT, link-local and IPv6 ULA destinations (10/8, 172.16/12, 192.168/16, 100.64/10, 169.254/16, fc00::/7). That covers the Proxmox host, the iDRAC and everything else at home.
- LAN exceptions are only DNS (53) and DHCP (67) by default, so name resolution and leases from the home router keep working. Configurable under `network.allow`, e.g. pin DNS to the router only.
- Spawning refuses (before anything is created or registered) if the datacenter firewall is off, because guest rules don't apply without it. Turn it on under Datacenter > Firewall > Options after checking your own access rules. `network.isolate: false` opts out.

- Ephemeral is the default; permanent runners only for private repos you trust, since state leaks between jobs.
- Runner network on its own VLAN/SDN zone with a firewall that blocks the Proxmox management IP, port 8006/22, the iDRAC, and the rest of your LAN; allow only outbound internet (and a package cache if you add one).
- Unprivileged LXC only. Privileged containers are root on the host.
- Secrets in separate 0600 files; never logged; JIT configs passed by file and deleted by the runner on start.
- The setup page binds only while setup runs, requires a random one-time token in its URL, and shuts down once config is written. It never stays listening.
- The App key is the crown jewel: anyone with it can register runners in your orgs. Keep it only on the host.
- Self-update only installs builds signed by this repo's `main` workflow; see section 7.

**Alerts (decided 2026-10-10):** everything logs to the journal (`journalctl -u pve-runners`) and `pve-runners status` shows current state. Optionally, `notify.webhook_url_file` points at a 0600 file holding an ntfy, Discord or Slack webhook URL (format auto-detected). It fires on spawn failures, self-update failure or rollback, and image build/pull failures. Off by default.

**More hosts later (decided 2026-10-10):** one host today. The design already allows more: each host runs its own independent copy with a unique `host_id` (defaults to the Proxmox node name) baked into runner and scale set names, all hosts share the same labels, and GitHub spreads queued jobs across whichever host asks first. No coordination between hosts is needed; each enforces its own `max_runners`. A second host runs its own setup (its own App per org, or copies the first host's App keys). Note that Proxmox VE is x86-only, so a Mac Studio would need a different backend later (e.g. Linux VMs under a hypervisor there), which the `Runner` interface leaves room for.

## 6. Language and packaging

Go is the right call: single static binary, first-party GitHub client libraries (`actions/scaleset`, `google/go-github`, `ghinstallation`), easy systemd integration, trivial cross-compile for amd64/arm64.
- Proxmox: no client library; `os/exec` on `pct`/`qm`/`pvesh` through the `Runner` interface, so tests assert exact argv.
- Ship a `.deb` (via `nfpm`/GoReleaser) that installs the binary, `/etc/pve-runners/config.example.yaml`, and `pve-runners.service`.
- Tests: fake `Runner` for pct/qm, fake GitHub (httptest, including the manifest and installation endpoints); never a live server. One opt-in integration test against a real PVE in a nested VM.

## 7. Self-install and self-update from `main`

Goal: you seed the host once; after that, anything merged to `main` reaches the host by itself.

**Where releases live:** the same public repo. The seed one-liner and the updater need no token, and Releases on public repos are free. Binaries contain no secrets (everything sensitive lives in `/etc/pve-runners/` on the host). The release workflow uses the built-in `GITHUB_TOKEN`; the only secret is the signing key.

**Seed once:**
```bash
curl -fsSL https://github.com/ServerTooling/proxmox-github-runners/releases/latest/download/install.sh | sudo bash
```
`install.sh` detects the arch, downloads the latest binary, verifies it (below), installs it to `/usr/local/bin`, writes the systemd unit, and runs `pve-runners setup`. That's the only manual step.

**Publishing (CI, `.github/workflows/pve-runners.yml`):**
1. On push to `main` touching `pve-runners/**`: `go vet`, `go test ./...`, build linux/amd64 + arm64 with the commit SHA and build number baked in (`-ldflags -X main.version=...`).
2. Write `SHA256SUMS` and sign it with an ed25519 key held as an Actions secret in a `release` environment that only `main` can use.
3. Create a release `build-<n>` (marked latest) with the binaries, `install.sh`, `SHA256SUMS`, its signature and `latest.json`. Old builds stay as releases, which gives a version history and lets `pve-runners update --to build-<n>` pin or roll back.
Only builds that passed tests on `main` are ever published, so a broken PR or branch can't reach the host.

**Updating (on the host):**
- A built-in update loop (every 15 min, configurable; also `pve-runners update --now`) reads `latest.json`. If the build number is newer, it downloads the binary, checks the signature against the public key compiled into the running binary, then the checksum.
- It waits for a quiet moment: no guest mid-provisioning. Running jobs are unaffected because they live in their own guests, and state lives in Proxmox tags, so a restart loses nothing.
- Swap atomically: keep the current binary as `pve-runners.prev`, rename the new one into place, `systemctl restart`.
- Health check: the new process must come up and pass its self-check (config loads and migrates, GitHub App auth works, `pvesh` reachable) within 60s. If not, the old binary is restored and restarted, and that build is skipped until a newer one appears. `pve-runners status` shows the current version, last update and any rollback.
- Config schema carries a `version:`; a newer binary migrates the file forward (backup kept as `config.yaml.bak`), so updates never need you to edit config.
- Runner agent updates ride along: the pinned `actions/runner` version is part of the build, so when it changes the updater refreshes the cached tarball and rebuilds the templates in the background.

```yaml
update:
  enabled: true
  channel: edge          # edge = every main build | stable = only releases tagged v*
  interval: 15m
```

**Guardrails, since `main` now deploys to a root service:**
- Only builds whose tests passed on `main` are published, and branch protection (free on this public repo) requires a PR with green CI before anything lands on `main`. Use `channel: stable` to update only on `v*` tags.
- Signature check means a compromised download URL can't push a binary; only the `main` workflow holds the signing key.
- Switch to `channel: stable` any time you'd rather update only on tagged releases.

## 8. Phased build plan

**Phase 0: prerequisites (manual, small)**
- Proxmox: runner VLAN + firewall. The pool and ID ranges are created by the tool.
- Repo secrets for release signing (one-time, documented in the README).

**Phase 1: core tool (first PR)**
- Config, `pct` builders, GitHub App auth, JIT configs, runner tarball verify.
- `pve-runners setup` (App manifest flow, install picker), `spawn`, `reconcile`, `status`, `template prepare`.
- `install.sh`, CI with privacy scan, signed release workflow, self-updater with rollback, so every later phase installs itself.

**Phase 2: template images (delivered)**
- `templates/` with Ansible roles, `templates/build.sh`, the `templates.yml` build/publish workflow (PRs build and smoke-test; main, weekly and manual runs publish), and host-side pull/verify/cache/`pct create`/prune in `internal/images`. Setup defaults the pool to `image: ubuntu-latest`; `template prepare` stays for custom templates.

**Phase 3: autoscaling**
- Scale set listener per org and pool driving spawn; capacity limits; adopt-on-restart; structured logs.

**Phase 4: breadth**
- repo-level targets, warm idle runners, VM pools via `qm`, orphaned-runner cleanup, `doctor`, headless device-flow setup.

**Later (on request): permanent runners**
- `mode: permanent` pools kept at `min` running, registered with a registration token instead of JIT. The config field already exists so no rework is needed.

**Phase 5: hardening**
- Metrics, retries with backoff, `.deb` packaging.

## 9. Decisions (settled 2026-10-10)

1. **Triggers:** runner scale set API (`actions/scaleset`). The Proxmox host is on a private home network with no inbound access, and the scale set long-poll is outbound only.
2. **Default guest:** LXC. VMs are an optional pool type.
3. **Build vs adopt:** build it ourselves in `proxmox-github-runners`, on top of `actions/scaleset`. No existing project covers Proxmox `pct`/`qm`, guided GitHub setup and self-update together, and adapting one would take longer than a focused build. `ysya/runscaler` is the reference for the scale set listener loop.
4. **Repo:** code, CI, releases and template images all in public `ServerTooling/proxmox-github-runners`.
5. **Runner toolchains:** declarative Ansible-built LXC images published as releases and pulled by the host (section 3.1).
