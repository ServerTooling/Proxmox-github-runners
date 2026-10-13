package setup

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// field is one setting on the settings page. Every value round-trips through a string so the page is
// one plain form; Parse validates the result as a whole before anything is saved.
type field struct {
	Name    string // form name, the config key
	Label   string
	Help    string
	Kind    string // text | number | bool | select
	Options []string
	get     func(c *config.Config) string
	set     func(c *config.Config, v string) error
}

type section struct {
	Title  string
	Fields []field
}

func text(name, label, help string, p func(c *config.Config) *string) field {
	return field{Name: name, Label: label, Help: help, Kind: "text",
		get: func(c *config.Config) string { return *p(c) },
		set: func(c *config.Config, v string) error { *p(c) = v; return nil }}
}

func number(name, label, help string, p func(c *config.Config) *int) field {
	return field{Name: name, Label: label, Help: help, Kind: "number",
		get: func(c *config.Config) string { return strconv.Itoa(*p(c)) },
		set: func(c *config.Config, v string) (err error) { *p(c), err = atoi(name, v); return err }}
}

func boolean(name, label, help string, p func(c *config.Config) *bool) field {
	return field{Name: name, Label: label, Help: help, Kind: "bool",
		get: func(c *config.Config) string { return strconv.FormatBool(*p(c)) },
		set: func(c *config.Config, v string) error { *p(c) = checked(v); return nil }}
}

// optBool is a *bool setting whose nil means true.
func optBool(name, label, help string, p func(c *config.Config) **bool) field {
	return field{Name: name, Label: label, Help: help, Kind: "bool",
		get: func(c *config.Config) string { return strconv.FormatBool(*p(c) == nil || **p(c)) },
		set: func(c *config.Config, v string) error { b := checked(v); *p(c) = &b; return nil }}
}

func duration(name, label, help string, p func(c *config.Config) *time.Duration) field {
	return field{Name: name, Label: label, Help: help, Kind: "text",
		get: func(c *config.Config) string { return fmtDur(*p(c)) },
		set: func(c *config.Config, v string) (err error) { *p(c), err = parseDur(name, v); return err }}
}

func strList(name, label, help string, p func(c *config.Config) *[]string) field {
	return field{Name: name, Label: label, Help: help, Kind: "text",
		get: func(c *config.Config) string { return strings.Join(*p(c), ", ") },
		set: func(c *config.Config, v string) error { *p(c) = splitList(v); return nil }}
}

func idRange(name, label, help string, p func(c *config.Config) *[2]int) field {
	return field{Name: name, Label: label, Help: help, Kind: "text",
		get: func(c *config.Config) string { r := *p(c); return fmt.Sprintf("%d-%d", r[0], r[1]) },
		set: func(c *config.Config, v string) error {
			lo, hi, ok := strings.Cut(strings.ReplaceAll(v, " ", ""), "-")
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if !ok || err1 != nil || err2 != nil {
				return fmt.Errorf("%s must look like 9000-9499", name)
			}
			*p(c) = [2]int{a, b}
			return nil
		}}
}

func choice(f field, options ...string) field {
	f.Kind, f.Options = "select", options
	return f
}

// hostFields are every setting outside targets and pools, in the config file's order.
func hostFields() []section {
	return []section{
		{"This host", []field{
			text("host_id", "Host ID", "Names this host in runner names, so several hosts can serve the same orgs.",
				func(c *config.Config) *string { return &c.HostID }),
			text("proxmox.node", "Proxmox node", "", func(c *config.Config) *string { return &c.Proxmox.Node }),
			number("proxmox.max_runners", "Max runners", "Most runners at once on this host, across all pools.",
				func(c *config.Config) *int { return &c.Proxmox.MaxRunners }),
			idRange("proxmox.vmid_range", "Runner guest IDs", "Only tagged guests in this range are ever changed or destroyed.",
				func(c *config.Config) *[2]int { return &c.Proxmox.VMIDRange }),
			text("proxmox.pool", "Proxmox resource pool", "", func(c *config.Config) *string { return &c.Proxmox.Pool }),
			text("proxmox.storage", "Container storage", "Empty picks one that can hold containers.",
				func(c *config.Config) *string { return &c.Proxmox.Storage }),
			number("proxmox.min_free_memory_mb", "Min free memory (MB)", "No runner starts if it would leave less free.",
				func(c *config.Config) *int { return &c.Proxmox.MinFreeMemoryMB }),
			text("proxmox.bridge", "Network bridge", "Runner NICs attach here; a pool can override it.",
				func(c *config.Config) *string { return &c.Proxmox.Bridge }),
			boolean("autoscale.enabled", "Autoscale", "Start a runner for each queued job.",
				func(c *config.Config) *bool { return &c.Autoscale.Enabled }),
		}},
		{"Network", []field{
			optBool("network.isolate", "Isolate runners from the LAN", "Needs the datacenter firewall on.",
				func(c *config.Config) **bool { return &c.Network.Isolate }),
			strList("network.blocked", "Blocked ranges", "Comma-separated CIDRs runners cannot reach.",
				func(c *config.Config) *[]string { return &c.Network.Blocked }),
			{Name: "network.allow", Label: "LAN exceptions", Kind: "text",
				Help: "proto/port or proto/port@dest, comma-separated, such as udp/53, tcp/53@192.0.2.1.",
				get:  func(c *config.Config) string { return fmtAllow(c.Network.Allow) },
				set: func(c *config.Config, v string) (err error) {
					c.Network.Allow, err = parseAllow(v)
					return err
				}},
			strList("network.dns", "DNS servers", "Comma-separated; empty copies the host's.",
				func(c *config.Config) *[]string { return &c.Network.DNS }),
		}},
		{"Runner cache", []field{
			text("cache.dir", "Cache folder", "Host folder for pools with the cache on; one subfolder per org, mounted at /opt/pve-cache.",
				func(c *config.Config) *string { return &c.Cache.Dir }),
			number("cache.max_gb", "Max size per org (GB)", "A bigger folder is emptied once none of its runners are running.",
				func(c *config.Config) *int { return &c.Cache.MaxGB }),
		}},
		{"Images", []field{
			number("images.retention_days", "Keep images (days)", "The newest, pinned and in-use images are always kept.",
				func(c *config.Config) *int { return &c.Images.RetentionDays }),
			text("images.repo", "Image repository", "", func(c *config.Config) *string { return &c.Images.Repo }),
			idRange("images.vmid_range", "Template guest IDs", "Must not overlap the runner guest IDs.",
				func(c *config.Config) *[2]int { return &c.Images.VMIDRange }),
			text("images.cache_storage", "Image cache storage", "", func(c *config.Config) *string { return &c.Images.CacheStorage }),
			number("images.rootfs_gb", "Template disk (GB)", "", func(c *config.Config) *int { return &c.Images.RootfsGB }),
			duration("images.check_interval", "Check for new images every", "",
				func(c *config.Config) *time.Duration { return &c.Images.CheckInterval }),
		}},
		{"Updates", []field{
			boolean("update.enabled", "Self-update", "", func(c *config.Config) *bool { return &c.Update.Enabled }),
			choice(text("update.channel", "Channel", "edge: every build from main. stable: v* tags only.",
				func(c *config.Config) *string { return &c.Update.Channel }), "edge", "stable"),
			duration("update.interval", "Check every", "", func(c *config.Config) *time.Duration { return &c.Update.Interval }),
			text("update.repo", "Release repository", "", func(c *config.Config) *string { return &c.Update.Repo }),
		}},
		{"Alerts", []field{
			choice(text("notify.format", "Webhook format", "", func(c *config.Config) *string { return &c.Notify.Format }),
				"auto", "ntfy", "discord", "slack"),
		}},
		{"Logs (Grafana Loki)", []field{
			text("logs.url", "Loki URL", "Such as https://logs-prod-000.grafana.net. Empty turns shipping off.",
				func(c *config.Config) *string { return &c.Logs.URL }),
			text("logs.user", "Loki user", "Grafana Cloud's numeric user; empty sends the token as a bearer token.",
				func(c *config.Config) *string { return &c.Logs.User }),
			duration("logs.interval", "Ship every", "", func(c *config.Config) *time.Duration { return &c.Logs.Interval }),
			optBool("logs.usage", "Runner usage", "Send each runner's CPU, memory, disk and network use, labelled with its job.",
				func(c *config.Config) **bool { return &c.Logs.Usage }),
		}},
		{"GitHub", []field{
			text("github.api_url", "API URL", "", func(c *config.Config) *string { return &c.GitHub.APIURL }),
			// Unpinned shows empty, since the version then moves with each self-update.
			{Name: "github.runner_version", Label: "Pinned runner version", Kind: "text",
				Help: "Set both to pin actions/runner; empty follows the version this build ships.",
				get:  func(c *config.Config) string { return pinned(c, c.GitHub.RunnerVersion) },
				set:  func(c *config.Config, v string) error { c.GitHub.Pin(v, c.GitHub.RunnerSHA256); return nil }},
			{Name: "github.runner_sha256", Label: "Pinned runner SHA-256", Kind: "text",
				get: func(c *config.Config) string { return pinned(c, c.GitHub.RunnerSHA256) },
				set: func(c *config.Config, v string) error { c.GitHub.Pin(c.GitHub.RunnerVersion, v); return nil }},
		}},
		{"Settings page", []field{
			optBool("web.enabled", "Serve this page", "Turning it off takes effect after saving; turn it back on in the file.",
				func(c *config.Config) **bool { return &c.Web.Enabled }),
			text("web.listen", "Listen on", "[address]:port", func(c *config.Config) *string { return &c.Web.Listen }),
		}},
	}
}

// poolField is a setting of one pool; its form name is pool.<name>.<key>.
type poolField struct {
	Key, Label, Kind string
	Options          []string
	get              func(p *config.Pool) string
	set              func(p *config.Pool, v string) error
}

func poolFields() []poolField {
	num := func(key, label string, p func(*config.Pool) *int) poolField {
		return poolField{Key: key, Label: label, Kind: "number",
			get: func(pl *config.Pool) string { return strconv.Itoa(*p(pl)) },
			set: func(pl *config.Pool, v string) (err error) { *p(pl), err = atoi(key, v); return err }}
	}
	str := func(key, label string, p func(*config.Pool) *string) poolField {
		return poolField{Key: key, Label: label, Kind: "text",
			get: func(pl *config.Pool) string { return *p(pl) },
			set: func(pl *config.Pool, v string) error { *p(pl) = v; return nil }}
	}
	mode := str("mode", "Mode", func(p *config.Pool) *string { return &p.Mode })
	// Permanent runners are planned; config validation refuses them until then, so the page does not offer them.
	mode.Kind, mode.Options = "select", []string{"ephemeral"}
	return []poolField{
		str("image", "Image (empty to use a template ID)", func(p *config.Pool) *string { return &p.Image }),
		num("template", "Template ID (when no image)", func(p *config.Pool) *int { return &p.Template }),
		mode,
		{Key: "labels", Label: "Labels (comma-separated)", Kind: "text",
			get: func(p *config.Pool) string { return strings.Join(p.Labels, ", ") },
			set: func(p *config.Pool, v string) error { p.Labels = splitList(v); return nil }},
		num("cores", "Cores", func(p *config.Pool) *int { return &p.Cores }),
		num("memory_mb", "Memory (MB)", func(p *config.Pool) *int { return &p.MemoryMB }),
		num("disk_gb", "Disk (GB)", func(p *config.Pool) *int { return &p.DiskGB }),
		num("max", "Max runners", func(p *config.Pool) *int { return &p.Max }),
		num("min", "Min runners", func(p *config.Pool) *int { return &p.Min }),
		num("warm", "Warm spares after a job", func(p *config.Pool) *int { return &p.Warm }),
		{Key: "warm_for", Label: "Keep warm spares for (empty for 30m)", Kind: "text",
			get: func(p *config.Pool) string { return fmtDur(p.WarmFor) },
			set: func(p *config.Pool, v string) (err error) {
				p.WarmFor, err = parseDur("warm_for", v)
				return err
			}},
		{Key: "cache", Label: "Shared cache (tools and packages kept between jobs)", Kind: "bool",
			get: func(p *config.Pool) string { return strconv.FormatBool(p.Cache) },
			set: func(p *config.Pool, v string) error { p.Cache = checked(v); return nil }},
		{Key: "full_clone", Label: "Full clones", Kind: "bool",
			get: func(p *config.Pool) string { return strconv.FormatBool(p.FullClone) },
			set: func(p *config.Pool, v string) error { p.FullClone = checked(v); return nil }},
		{Key: "max_lifetime", Label: "Max lifetime (empty for none)", Kind: "text",
			get: func(p *config.Pool) string { return fmtDur(p.MaxLifetime) },
			set: func(p *config.Pool, v string) (err error) {
				p.MaxLifetime, err = parseDur("max_lifetime", v)
				return err
			}},
		str("bridge", "Bridge (empty for the host's)", func(p *config.Pool) *string { return &p.Bridge }),
		num("vlan", "VLAN (0 for none)", func(p *config.Pool) *int { return &p.VLAN }),
		str("scale_set", "Scale set (empty for the first label)", func(p *config.Pool) *string { return &p.ScaleSet }),
	}
}

// checked reads a box: a browser form sends "on" when ticked, the app's JSON sends the "true" it was shown.
func checked(v string) bool { return v == "on" || v == "true" }

func pinned(c *config.Config, v string) string {
	if !c.GitHub.Pinned() {
		return ""
	}
	return v
}

func atoi(name, v string) (int, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number, got %q", name, v)
	}
	return n, nil
}

func parseDur(name, v string) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 15m or 6h, got %q", name, v)
	}
	return d, nil
}

// fmtDur prints 15m rather than 15m0s, and nothing for zero.
func fmtDur(d time.Duration) string {
	if d == 0 {
		return ""
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out = append(out, s)
	}
	return out
}

func fmtAllow(rules []pve.AllowRule) string {
	parts := make([]string, len(rules))
	for i, a := range rules {
		parts[i] = fmt.Sprintf("%s/%d", a.Proto, a.Port)
		if a.Dest != "" {
			parts[i] += "@" + a.Dest
		}
	}
	return strings.Join(parts, ", ")
}

func parseAllow(v string) ([]pve.AllowRule, error) {
	out := []pve.AllowRule{}
	for _, item := range splitList(v) {
		rule, dest, _ := strings.Cut(item, "@")
		proto, port, ok := strings.Cut(rule, "/")
		n, err := strconv.Atoi(port)
		if !ok || err != nil {
			return nil, fmt.Errorf("network.allow: %q should look like udp/53 or tcp/53@192.0.2.1", item)
		}
		out = append(out, pve.AllowRule{Proto: proto, Port: n, Dest: dest})
	}
	return out, nil
}
