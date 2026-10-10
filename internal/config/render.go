package config

import (
	"bytes"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Render writes the config as the commented file people edit by hand. Every setting is spelled out
// with its current value (defaults included) so the file documents itself; optional settings that
// are off appear commented out. Parse(Render(c)) gives back c.
func (c *Config) Render() ([]byte, error) {
	var b bytes.Buffer
	if err := fileTmpl.Execute(&b, c); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// q renders a scalar exactly as YAML needs it (quoted only when necessary).
func q(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return `""`
	}
	return strings.TrimSpace(string(out))
}

func list(vs []string) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = q(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// dur prints 15m rather than 15m0s.
func dur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func ids(r [2]int) string { return "[" + strconv.Itoa(r[0]) + ", " + strconv.Itoa(r[1]) + "]" }

var fileTmpl = template.Must(template.New("config").Funcs(template.FuncMap{
	"q": q, "list": list, "dur": dur, "ids": ids,
}).Parse(`# /etc/pve-runners/config.yaml: pve-runners on this Proxmox host (root, mode 0600).
# Written by pve-runners; safe to hand-edit. Check edits with "pve-runners config validate", apply
# them with "systemctl reload pve-runners". No secrets live here: each GitHub App key is its own
# 0600 file under /etc/pve-runners/apps/, and so is the notify URL.
version: {{.Version}}

# Names this host in runner names, so several Proxmox hosts can serve the same orgs.
host_id: {{q .HostID}}

proxmox:
  # This host's Proxmox node name.
  node: {{q .Proxmox.Node}}
  # Most runners at once on this host, across all pools and orgs.
  max_runners: {{.Proxmox.MaxRunners}}
  # Guest IDs reserved for runner containers. Only guests in this range that carry the gh-runner tag
  # are ever modified or destroyed.
  vmid_range: {{ids .Proxmox.VMIDRange}}
  # Proxmox resource pool that runners and templates are put in.
  pool: {{q .Proxmox.Pool}}
  # Storage for image template root disks and full clones.
  storage: {{q .Proxmox.Storage}}
  # Never start a runner that would leave the host with less free memory than this.
  min_free_memory_mb: {{.Proxmox.MinFreeMemoryMB}}
  # Network bridge for runner NICs (always with the Proxmox firewall on). A pool can override it.
  bridge: {{q .Proxmox.Bridge}}

network:
  # Runners get internet access but no LAN: a Proxmox firewall group drops the ranges below. Needs
  # the datacenter firewall on (Datacenter > Firewall > Options); spawning is refused otherwise.
  isolate: {{.Network.Isolated}}
  blocked:
{{- range .Network.Blocked}}
    - {{q .}}
{{- end}}
  # LAN exceptions to the block: DNS and DHCP by default. Add a dest to narrow one to a single host.
  allow:
{{- range .Network.Allow}}
    - {proto: {{.Proto}}, port: {{.Port}}{{if .Dest}}, dest: {{q .Dest}}{{end}}}
{{- end}}

# One entry per org (or personal account), each with its own private GitHub App. "pve-runners setup"
# adds these; only private repositories are ever served.
targets:{{if not .Targets}} []{{end}}
{{- range .Targets}}
  - {{if .Org}}org: {{q .Org}}{{else}}user: {{q .User}}{{end}}
    app_id: {{.AppID}}
{{- if .AppSlug}}
    app_slug: {{q .AppSlug}}
{{- end}}
    private_key_file: {{q .PrivateKeyFile}}
    installation_id: {{.InstallationID}}
{{- if .Repos}}
    # Only these repositories.
    repos: {{list .Repos}}
{{- end}}
{{- if .Org}}
    # "" is the Default group. The group must not allow public repositories.
    runner_group: {{q .RunnerGroup}}
{{- end}}
    pools: {{list .Pools}}
{{- end}}

# Kinds of runner. A workflow picks one with "runs-on: <label>".
pools:{{if not .Pools}} {}{{end}}
{{- range $name := .PoolNames}}{{with index $.Pools $name}}
  {{q $name}}:
    type: {{.Type}}
{{- if .Image}}
    # Published runner image (Ubuntu with Python, Go, Node, Helm, kubectl, Docker), newest release.
    # Pin one release with name@release, or use "template: <ID>" for a container you prepared.
    image: {{q .Image}}
{{- else}}
    # Container made with "pve-runners template prepare", or use "image: ubuntu-latest" instead.
    template: {{.Template}}
{{- end}}
    # ephemeral: one job per runner, then the container is destroyed.
    mode: {{.Mode}}
    # Labels workflows use in runs-on.
    labels: {{list .Labels}}
    # Runner size.
    cores: {{.Cores}}
    memory_mb: {{.MemoryMB}}
    disk_gb: {{.DiskGB}}
    # Most runners of this pool at once (proxmox.max_runners still caps the host).
    max: {{.Max}}
{{- if .Min}}
    min: {{.Min}}
{{- end}}
{{- if .FullClone}}
    full_clone: true
{{- end}}
{{- if .MaxLifetime}}
    # Destroy runners older than this.
    max_lifetime: {{dur .MaxLifetime}}
{{- else}}
    # Destroy runners older than this:
    # max_lifetime: 6h
{{- end}}
{{- if ne .Bridge $.Proxmox.Bridge}}
    bridge: {{q .Bridge}}
{{- else}}
    # Use another bridge than proxmox.bridge for this pool:
    # bridge: vmbr1
{{- end}}
{{- if .VLAN}}
    vlan: {{.VLAN}}
{{- else}}
    # Tag the runner NIC with a VLAN:
    # vlan: 50
{{- end}}
{{- end}}{{end}}

images:
  # Delete downloaded images and their templates after this many days. The newest image of each
  # name, pinned images, and images that runners were cloned from are always kept.
  retention_days: {{.Images.RetentionDays}}
  # Where images are published.
  repo: {{q .Images.Repo}}
  # Guest IDs for templates made from images; must not overlap proxmox.vmid_range.
  vmid_range: {{ids .Images.VMIDRange}}
  # Proxmox storage whose template cache keeps downloaded images.
  cache_storage: {{q .Images.CacheStorage}}
  # Template root disk; runners grow it to their pool's disk_gb.
  rootfs_gb: {{.Images.RootfsGB}}
  # How often to look for newer images and prune old ones.
  check_interval: {{dur .Images.CheckInterval}}

notify:
  # Alerts on failed spawns, updates and image downloads. Off until a file holding an ntfy,
  # Discord or Slack webhook URL is set here.
{{- if .Notify.WebhookURLFile}}
  webhook_url_file: {{q .Notify.WebhookURLFile}}
{{- else}}
  # webhook_url_file: /etc/pve-runners/notify-url
{{- end}}
  # auto, ntfy, discord or slack.
  format: {{q (or .Notify.Format "auto")}}

update:
  # Keep this binary updated from the repo's signed releases.
  enabled: {{.Update.Enabled}}
  # edge: every build from main. stable: only v* tags.
  channel: {{.Update.Channel}}
  interval: {{dur .Update.Interval}}
  repo: {{q .Update.Repo}}

github:
  api_url: {{q .GitHub.APIURL}}
{{- if .GitHub.Pinned}}
  # actions/runner pinned to this version; remove both lines to follow the version this build ships.
  runner_version: {{q .GitHub.RunnerVersion}}
  runner_sha256: {{q .GitHub.RunnerSHA256}}
{{- else}}
  # actions/runner follows the version this build ships{{with .GitHub.RunnerVersion}} ({{.}}){{end}}. To pin, set both:
  # runner_version: ""
  # runner_sha256: ""
{{- end}}
`))
