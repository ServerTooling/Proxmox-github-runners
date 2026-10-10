// Package config loads, validates and saves /etc/pve-runners/config.yaml.
//
// The file is written by `pve-runners setup` and is safe to hand-edit. It never holds secrets inline:
// each GitHub App key lives in its own 0600 file referenced by path.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ServerTooling/proxmox-github-runners/internal/buildinfo"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// CurrentVersion is the schema version this binary writes. Older files are migrated forward on load.
const CurrentVersion = 1

const (
	DefaultPath = "/etc/pve-runners/config.yaml"
	DefaultDir  = "/etc/pve-runners"
)

type Config struct {
	Version int `yaml:"version"`
	// HostID names this host in runner names so several independent hosts can serve the same orgs
	// and labels without clashing; GitHub spreads jobs across them. Defaults to proxmox.node.
	HostID  string           `yaml:"host_id,omitempty"`
	Proxmox Proxmox          `yaml:"proxmox"`
	GitHub  GitHub           `yaml:"github"`
	Targets []Target         `yaml:"targets"`
	Pools   map[string]*Pool `yaml:"pools"`
	Update  Update           `yaml:"update"`
	Images  Images           `yaml:"images"`
	Network Network          `yaml:"network"`
	Notify  Notify           `yaml:"notify"`
	Extra   map[string]any   `yaml:",inline"`
}

type Proxmox struct {
	Node string `yaml:"node"`
	// MaxRunners caps runner guests on this host across all pools and orgs.
	MaxRunners      int    `yaml:"max_runners"`
	VMIDRange       [2]int `yaml:"vmid_range"`
	Pool            string `yaml:"pool"`
	Storage         string `yaml:"storage"`
	MinFreeMemoryMB int    `yaml:"min_free_memory_mb"`
}

type GitHub struct {
	APIURL        string `yaml:"api_url"`
	RunnerVersion string `yaml:"runner_version"`
	RunnerSHA256  string `yaml:"runner_sha256"`
}

// Target is one account the service serves: an org (org-level runners) or a personal account
// (repo-level runners, since personal accounts have no org runner groups).
//
// Each target has its own private GitHub App, created by setup under that org or account. A private
// App can only be installed on the account that owns it, and only an org owner or app manager can
// create one there, so the tool can never be attached to an org you don't manage. Only private repos
// are served; public repos are refused (they can use GitHub-hosted runners).
type Target struct {
	Org            string   `yaml:"org,omitempty"`
	User           string   `yaml:"user,omitempty"`
	AppID          int64    `yaml:"app_id"`
	AppSlug        string   `yaml:"app_slug,omitempty"`
	PrivateKeyFile string   `yaml:"private_key_file,omitempty"`
	InstallationID int64    `yaml:"installation_id"`
	Repos          []string `yaml:"repos,omitempty"` // empty = all repos the installation can see
	RunnerGroup    string   `yaml:"runner_group,omitempty"`
	Pools          []string `yaml:"pools"`
}

func (t Target) Owner() string {
	if t.Org != "" {
		return t.Org
	}
	return t.User
}

type Pool struct {
	Type        string        `yaml:"type"` // lxc | vm (vm arrives in a later phase)
	Template    int           `yaml:"template"`
	Mode        string        `yaml:"mode"` // ephemeral | permanent
	Labels      []string      `yaml:"labels"`
	Cores       int           `yaml:"cores"`
	MemoryMB    int           `yaml:"memory_mb"`
	DiskGB      int           `yaml:"disk_gb"`
	Max         int           `yaml:"max"`
	Min         int           `yaml:"min,omitempty"`
	FullClone   bool          `yaml:"full_clone,omitempty"`
	MaxLifetime time.Duration `yaml:"max_lifetime,omitempty"`
	Bridge      string        `yaml:"bridge,omitempty"`
	VLAN        int           `yaml:"vlan,omitempty"`
}

// Network isolates runner guests: internet access yes, LAN no. Enforced with a Proxmox cluster
// firewall security group attached to every runner, so the datacenter firewall must be on.
type Network struct {
	Isolate *bool           `yaml:"isolate,omitempty"` // default true
	Blocked []string        `yaml:"blocked,omitempty"` // default pve.DefaultBlocked
	Allow   []pve.AllowRule `yaml:"allow,omitempty"`   // LAN exceptions; default DNS + DHCP
}

func (n Network) Isolated() bool { return n.Isolate == nil || *n.Isolate }

// Notify sends alerts (spawn failures, update failures or rollbacks, image failures) to a webhook.
// Off by default; the URL is kept in its own 0600 file because Discord and Slack URLs are credentials.
type Notify struct {
	WebhookURLFile string `yaml:"webhook_url_file,omitempty"`
	Format         string `yaml:"format,omitempty"` // auto | ntfy | discord | slack
}

// Images controls template images downloaded from GitHub releases. Runner containers are destroyed
// as soon as their job ends; images are kept and only aged out by RetentionDays. An image a pool
// still uses, and the newest image of each name, are never deleted regardless of age.
type Images struct {
	RetentionDays int `yaml:"retention_days"`
}

type Update struct {
	Enabled  bool          `yaml:"enabled"`
	Channel  string        `yaml:"channel"` // edge | stable
	Interval time.Duration `yaml:"interval"`
	Repo     string        `yaml:"repo"`
}

// Default returns a config with every optional field filled in; setup starts from this.
func Default() *Config {
	return &Config{
		Version: CurrentVersion,
		Proxmox: Proxmox{MaxRunners: 4, VMIDRange: [2]int{9000, 9499}, Pool: "gh-runners", Storage: "local-lvm", MinFreeMemoryMB: 2048},
		GitHub: GitHub{
			APIURL:        "https://api.github.com",
			RunnerVersion: buildinfo.RunnerVersion,
			RunnerSHA256:  buildinfo.RunnerSHA256,
		},
		Pools:  map[string]*Pool{},
		Images: Images{RetentionDays: 30},
		Network: Network{
			Blocked: append([]string(nil), pve.DefaultBlocked...),
			// Runners still need name resolution and DHCP leases, which at home usually come from
			// the router on the LAN. Only those ports are let through.
			Allow: []pve.AllowRule{{Proto: "udp", Port: 53}, {Proto: "tcp", Port: 53}, {Proto: "udp", Port: 67}},
		},
		Update: Update{
			Enabled: true, Channel: "edge", Interval: 15 * time.Minute,
			Repo: "ServerTooling/proxmox-github-runners",
		},
	}
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Proxmox.VMIDRange == [2]int{} {
		c.Proxmox.VMIDRange = d.Proxmox.VMIDRange
	}
	if c.HostID == "" {
		c.HostID = c.Proxmox.Node
	}
	if c.Proxmox.MaxRunners == 0 {
		c.Proxmox.MaxRunners = d.Proxmox.MaxRunners
	}
	if c.Proxmox.Pool == "" {
		c.Proxmox.Pool = d.Proxmox.Pool
	}
	if c.Proxmox.Storage == "" {
		c.Proxmox.Storage = d.Proxmox.Storage
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = d.GitHub.APIURL
	}
	// An empty runner pin follows the version baked into this build, so self-update also rolls the
	// runner agent forward. Setting both fields in the file pins it explicitly.
	if c.GitHub.RunnerVersion == "" {
		c.GitHub.RunnerVersion, c.GitHub.RunnerSHA256 = d.GitHub.RunnerVersion, d.GitHub.RunnerSHA256
	}
	if c.Update.Channel == "" {
		c.Update.Channel = d.Update.Channel
	}
	if c.Update.Interval == 0 {
		c.Update.Interval = d.Update.Interval
	}
	if c.Network.Blocked == nil {
		c.Network.Blocked = d.Network.Blocked
	}
	if c.Network.Allow == nil {
		c.Network.Allow = d.Network.Allow
	}
	if c.Images.RetentionDays == 0 {
		c.Images.RetentionDays = d.Images.RetentionDays
	}
	if c.Update.Repo == "" {
		c.Update.Repo = d.Update.Repo
	}
	for i := range c.Targets {
		if c.Targets[i].PrivateKeyFile == "" && c.Targets[i].Owner() != "" {
			c.Targets[i].PrivateKeyFile = KeyPath(c.Targets[i].Owner())
		}
	}
	if c.Pools == nil {
		c.Pools = map[string]*Pool{}
	}
	for _, p := range c.Pools {
		if p.Type == "" {
			p.Type = "lxc"
		}
		if p.Mode == "" {
			p.Mode = "ephemeral"
		}
		// Default runner size: 4 cores, 8 GB RAM, 40 GB disk.
		if p.Cores == 0 {
			p.Cores = 4
		}
		if p.MemoryMB == 0 {
			p.MemoryMB = 8192
		}
		if p.DiskGB == 0 {
			p.DiskGB = 40
		}
		if p.Max == 0 {
			p.Max = c.Proxmox.MaxRunners
		}
	}
}

// migrate upgrades older schema versions in place and reports whether anything changed.
func (c *Config) migrate() (bool, error) {
	switch {
	case c.Version == CurrentVersion:
		return false, nil
	case c.Version == 0:
		// Hand-written files without a version are treated as v1.
		c.Version = CurrentVersion
		return true, nil
	case c.Version > CurrentVersion:
		return false, fmt.Errorf("config version %d is newer than this binary supports (%d)", c.Version, CurrentVersion)
	}
	return false, fmt.Errorf("unknown config version %d", c.Version)
}

// Parse decodes, migrates, defaults and validates a config document.
func Parse(data []byte) (*Config, bool, error) {
	c := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(c); err != nil {
		return nil, false, fmt.Errorf("parse config: %w", err)
	}
	if len(c.Extra) > 0 {
		keys := make([]string, 0, len(c.Extra))
		for k := range c.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, false, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	migrated, err := c.migrate()
	if err != nil {
		return nil, false, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, false, err
	}
	return c, migrated, nil
}

// Load reads path; when the schema was migrated it rewrites the file, keeping a .bak of the original.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, migrated, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if migrated {
		if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
			return nil, err
		}
		if err := c.Save(path); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte("# Written by pve-runners setup; safe to hand-edit.\n"), data...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var labelRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate returns every problem at once so `config validate` can show them together.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.HostID != "" && !labelRe.MatchString(c.HostID) {
		add("host_id %q may only use letters, digits, '.', '_' and '-'", c.HostID)
	}
	lo, hi := c.Proxmox.VMIDRange[0], c.Proxmox.VMIDRange[1]
	if lo < 100 || hi < lo {
		add("proxmox.vmid_range must be [low, high] with 100 <= low <= high, got [%d, %d]", lo, hi)
	}
	if c.Proxmox.MaxRunners < 1 || c.Proxmox.MaxRunners > hi-lo+1 {
		add("proxmox.max_runners must be between 1 and the size of vmid_range (%d), got %d", hi-lo+1, c.Proxmox.MaxRunners)
	}
	for i, a := range c.Network.Allow {
		if a.Proto != "tcp" && a.Proto != "udp" {
			add("network.allow[%d].proto must be tcp or udp", i)
		}
		if a.Port < 1 || a.Port > 65535 {
			add("network.allow[%d].port must be 1-65535", i)
		}
	}
	switch c.Notify.Format {
	case "", "auto", "ntfy", "discord", "slack":
	default:
		add("notify.format must be auto, ntfy, discord or slack")
	}
	if c.Images.RetentionDays < 1 {
		add("images.retention_days must be at least 1, got %d", c.Images.RetentionDays)
	}
	if c.Update.Channel != "edge" && c.Update.Channel != "stable" {
		add("update.channel must be edge or stable, got %q", c.Update.Channel)
	}
	for name, p := range c.Pools {
		if !labelRe.MatchString(name) {
			add("pool name %q may only use letters, digits, '.', '_' and '-'", name)
		}
		if p.Type != "lxc" && p.Type != "vm" {
			add("pools.%s.type must be lxc or vm", name)
		}
		switch p.Mode {
		case "ephemeral":
		case "permanent":
			// Reserved so configs written today stay valid when permanent runners land.
			add("pools.%s.mode permanent is not supported yet; use ephemeral", name)
		default:
			add("pools.%s.mode must be ephemeral or permanent", name)
		}
		if p.Template < 100 {
			add("pools.%s.template must be a guest ID (>= 100)", name)
		} else if p.Template >= lo && p.Template <= hi {
			add("pools.%s.template %d is inside vmid_range; templates must live outside it", name, p.Template)
		}
		if p.Min > p.Max {
			add("pools.%s.min (%d) is larger than max (%d)", name, p.Min, p.Max)
		}
		for _, l := range p.Labels {
			if !labelRe.MatchString(l) {
				add("pools.%s has invalid label %q", name, l)
			}
		}
	}
	for i, t := range c.Targets {
		if (t.Org == "") == (t.User == "") {
			add("targets[%d] needs exactly one of org or user", i)
		}
		if t.AppID == 0 {
			add("targets[%d] (%s) has no app_id; re-run `pve-runners setup`", i, t.Owner())
		}
		if t.InstallationID == 0 {
			add("targets[%d] (%s) has no installation_id; re-run `pve-runners setup`", i, t.Owner())
		}
		if t.User != "" && len(t.Repos) == 0 {
			add("targets[%d] (%s) is a personal account; list its repos (personal accounts have no org runners)", i, t.User)
		}
		for _, pn := range t.Pools {
			if _, ok := c.Pools[pn]; !ok {
				add("targets[%d] (%s) uses unknown pool %q", i, t.Owner(), pn)
			}
		}
	}
	return errors.Join(errs...)
}

// KeyPath is where setup stores the App private key for an owner.
func KeyPath(owner string) string {
	return filepath.Join(DefaultDir, "apps", strings.ToLower(owner)+".pem")
}

// PoolNames returns pool names sorted for stable output.
func (c *Config) PoolNames() []string {
	names := make([]string, 0, len(c.Pools))
	for n := range c.Pools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
