# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

`pve-runners`: a Go binary that runs as root on a Proxmox VE host and provides ephemeral GitHub
Actions runners (LXC) for the private repos of orgs the owner manages. The design and phase plan is
in `docs/plan.md`.

## Commands

```bash
go test ./...                      # all tests; no Proxmox or network needed
go vet ./... && gofmt -l .         # CI fails on either
scripts/privacy-scan.sh            # CI and release gate: no private IPs, hostnames, emails, secrets
go test ./internal/provision/ -run TestSpawnRunsExactSequence   # single test
SKIP_ROLES=go,helm templates/build.sh ubuntu-latest /tmp/out    # local image build (docker, ansible, zstd)
```

## Architecture

- `cmd/pve-runners`: CLI wiring only, no business logic.
- `internal/pve`: every `pct`/`pvesh` command is built by a function and run through `Runner`.
  New host operations get a builder plus a test asserting the exact argv. `Host.Owned` (tag
  `gh-runner` AND inside `vmid_range` AND not a template) guards every destructive call; keep it.
- `internal/github`: hand-written App client (JWT, installation tokens, JIT configs, manifest
  conversion). Tests assert exact request bodies against `githubtest.Fake`.
- `internal/provision`, `internal/reconcile`: spawn and cleanup; state lives only in guest tags.
- `internal/scaleset`: hand-written client for GitHub's runner scale set API (registration-token
  exchange, scale sets, message sessions, job acquire, JIT); tests assert exact requests against
  `scalesettest.Fake`. `internal/autoscale`: one listener per target x pool (x repo for personal
  accounts) that claims private-repo jobs and spawns runners tagged `scaleset-<id>`.
- `internal/setup`: one-time setup page (manifest flow, one private App per org), and the same server as
  the service's always-on settings page (`Service` set): Proxmox login (`PVELogin`, Sys.Modify), every
  config field in `fields.go`, secrets written to 0600 files, save validates then restarts the service.
- `internal/images`: published runner images: resolve release, verify signed SHA256SUMS, cache in
  the vztmpl dir, `pct create` a template in `images.vmid_range`, prune by age. `pve.OwnedTemplate`
  (tag `gh-template` AND inside that range) guards template deletion; keep it.
- `templates/`: image definitions (`<name>/template.yaml`) and Ansible roles; `templates/build.sh`
  builds one in Docker and `.github/workflows/templates.yml` publishes it. Every tarball a role
  downloads is checksum-verified against its publisher's sums.
- `internal/logship`: optional log shipping to Grafana Loki (the service journal plus `gh-runner` in each
  running runner), read with journal cursors; anything token-like is masked before it leaves the host.
- `internal/update`: self-update (ed25519-signed SHA256SUMS, atomic swap, health-checked rollback).
- `internal/config`: YAML with validation and forward migration (`version`).

## Rules

- This repo is **public**. Never commit real IPs, hostnames, usernames, emails, tokens or keys;
  use `192.0.2.x` and `example-org`. Run `scripts/privacy-scan.sh` before pushing.
- Only private repositories are served; never add a path that registers runners usable by public
  repos.
- Runners must stay off the LAN (cluster firewall group `gh-runner-egress`); spawning refuses when
  isolation can't be enforced.
- Tests only against in-repo fakes (`pvetest.Fake`, `githubtest.Fake`), never a live host or GitHub.
- Dependencies: only `gopkg.in/yaml.v3` so far; add others only with a clear reason.
- Kotlin-style "comments explain why, not what"; gofmt; ~120 columns.
