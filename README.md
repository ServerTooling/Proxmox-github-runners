# proxmox-github-runners

Ephemeral GitHub Actions self-hosted runners on Proxmox VE.

`pve-runners` is a single binary that runs as a root service on a Proxmox host. It serves the
private repositories of the GitHub organizations you choose: when a workflow job targets
`runs-on: proxmox`, it clones an LXC container from a prepared template, registers it as a
single-use runner, and destroys it as soon as the job finishes. Runners get internet access but
no access to your LAN.

Work in progress; see the open pull requests.

## License

Apache 2.0, see [LICENSE](LICENSE).
