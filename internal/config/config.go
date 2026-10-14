// Package config loads, validates and saves /etc/pve-runners/config.yaml.
//
// The file is written by `pve-runners setup` and is safe to hand-edit. It never holds secrets inline:
// each GitHub App key lives in its own 0600 file referenced by path.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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
	Logs    Logs             `yaml:"logs"`
	Web     Web              `yaml:"web"`
	// Autoscale listens to GitHub's job queue and starts a runner for each queued job.
	Autoscale Autoscale      `yaml:"autoscale"`
	Cache     Cache          `yaml:"cache"`
	Extra     map[string]any `yaml:",inline"`
}

type Proxmox struct {
	Node string `yaml:"node"`
	// MaxRunners caps runner guests on this host across all pools and orgs.
	MaxRunners int    `yaml:"max_runners"`
	VMIDRange  [2]int `yaml:"vmid_range"`
	Pool       string `yaml:"pool"`
	// Storage holds container root disks; empty picks the best one that can (see pve.RootfsStorage).
	Storage         string `yaml:"storage"`
	MinFreeMemoryMB int    `yaml:"min_free_memory_mb"`
	// Bridge is the network bridge runner NICs (and image templates) attach to; a pool can override it.
	Bridge string `yaml:"bridge"`
}

type GitHub struct {
	APIURL        string `yaml:"api_url"`
	RunnerVersion string `yaml:"runner_version"`
	RunnerSHA256  string `yaml:"runner_sha256"`
	// pinned records that the file set the runner version, so Render writes it back only then.
	pinned bool
}

// Pinned reports whether the config pins actions/runner rather than following this build.
func (g GitHub) Pinned() bool { return g.pinned }

// Pin sets the runner pin as if the file had set it; an empty version follows this build again.
func (g *GitHub) Pin(version, sha256 string) {
	g.RunnerVersion, g.RunnerSHA256, g.pinned = version, sha256, version != ""
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
	Type     string   `yaml:"type"`               // lxc | vm (vm arrives in a later phase)
	Template int      `yaml:"template,omitempty"` // a template container prepared by hand, or
	Image    string   `yaml:"image,omitempty"`    // a published image: name, or name@release-tag to pin
	Mode     string   `yaml:"mode"`               // ephemeral | permanent
	Labels   []string `yaml:"labels"`
	Cores    int      `yaml:"cores"`
	MemoryMB int      `yaml:"memory_mb"`
	DiskGB   int      `yaml:"disk_gb"`
	Max      int      `yaml:"max"`
	Min      int      `yaml:"min,omitempty"`
	// Warm is how many spare runners are kept ready after a job, so the next one starts at once.
	// They are removed once no job has come for WarmFor.
	Warm    int           `yaml:"warm,omitempty"`
	WarmFor time.Duration `yaml:"warm_for,omitempty"`
	// Cache mounts the owner's shared cache folder (cache.dir) into each runner, so toolchains and
	// package downloads survive between jobs.
	Cache       bool          `yaml:"cache,omitempty"`
	FullClone   bool          `yaml:"full_clone,omitempty"`
	MaxLifetime time.Duration `yaml:"max_lifetime,omitempty"`
	Bridge      string        `yaml:"bridge,omitempty"`
	VLAN        int           `yaml:"vlan,omitempty"`
	// ScaleSet names the pool's GitHub runner scale set, which is what "runs-on:" matches; it
	// defaults to the first label.
	ScaleSet string `yaml:"scale_set,omitempty"`
}

// ScaleSetName is the scale set the autoscaler registers this pool's runners in.
func (p *Pool) ScaleSetName() string {
	if p.ScaleSet != "" || len(p.Labels) == 0 {
		return p.ScaleSet
	}
	return p.Labels[0]
}

// Cache is the host folder runners of pools with cache: true share: one subfolder per org or account,
// so one owner's jobs never see another's files. A subfolder over MaxGB is emptied once none of its
// runners are running.
type Cache struct {
	Dir   string `yaml:"dir"`
	MaxGB int    `yaml:"max_gb"`
}

// Autoscale controls the job-queue listener. With it off, runners start only with "pve-runners spawn".
type Autoscale struct {
	Enabled bool `yaml:"enabled"`
	// StartGrace is how long a new runner may take to connect to GitHub. Until then it is never
	// removed as idle; one that hasn't connected by then is destroyed and replaced.
	StartGrace time.Duration `yaml:"start_grace,omitempty"`
}

// Network isolates runner guests: internet access yes, LAN no. Enforced with a Proxmox cluster
// firewall security group attached to every runner, so the datacenter firewall must be on.
type Network struct {
	Isolate *bool           `yaml:"isolate,omitempty"` // default true
	Blocked []string        `yaml:"blocked,omitempty"` // default pve.DefaultBlocked
	Allow   []pve.AllowRule `yaml:"allow,omitempty"`   // LAN exceptions; default DNS + DHCP
	// DNS servers for runners; empty copies the host's, which may not answer from the runner bridge.
	DNS []string `yaml:"dns,omitempty"`
}

func (n Network) Isolated() bool { return n.Isolate == nil || *n.Isolate }

// Notify sends alerts (spawn failures, update failures or rollbacks, image failures) to a webhook.
// Off by default; the URL is kept in its own 0600 file because Discord and Slack URLs are credentials.
type Notify struct {
	WebhookURLFile string `yaml:"webhook_url_file,omitempty"`
	Format         string `yaml:"format,omitempty"` // auto | ntfy | discord | slack
}

// Logs ships this service's log and each running runner's gh-runner log to Grafana Loki (Grafana Cloud
// or self-hosted), so both can be read without access to the host. Off until url and token_file are set.
type Logs struct {
	// URL is the Loki base URL, such as https://logs-prod-012.grafana.net.
	URL string `yaml:"url,omitempty"`
	// User is the basic-auth user (Grafana Cloud's numeric Loki user); empty sends the token as a bearer.
	User string `yaml:"user,omitempty"`
	// TokenFile holds the token (for Grafana Cloud, an access policy token with logs:write), mode 0600.
	TokenFile string        `yaml:"token_file,omitempty"`
	Interval  time.Duration `yaml:"interval"`
	// Usage also sends each runner's CPU, memory, disk and network use every interval, labelled with
	// its job, as the pve-runners-usage stream. Default true.
	Usage *bool `yaml:"usage,omitempty"`
}

func (l Logs) On() bool { return l.URL != "" && l.TokenFile != "" }

// UsageWanted is the usage setting; UsageOn also needs shipping configured.
func (l Logs) UsageWanted() bool { return l.Usage == nil || *l.Usage }

// UsageOn reports whether runner usage samples are sent along with the logs.
func (l Logs) UsageOn() bool { return l.On() && l.UsageWanted() }

// Web is the API the service serves over HTTPS with the Proxmox node's certificate, for the Dell Server
// Tools app. Requests carry a Proxmox token or ticket with Sys.Modify; saving restarts the service.
type Web struct {
	Enabled *bool  `yaml:"enabled,omitempty"` // default true
	Listen  string `yaml:"listen"`
}

func (w Web) On() bool { return w.Enabled == nil || *w.Enabled }

// Images controls template images downloaded from GitHub releases (see templates/ in this repo).
// Runner containers are destroyed as soon as their job ends; images are kept and only aged out by
// RetentionDays. The newest image of each name, pinned images, and images that runners were cloned
// from are never deleted regardless of age.
type Images struct {
	RetentionDays int `yaml:"retention_days"`
	// Repo publishes the images; defaults to update.repo.
	Repo string `yaml:"repo,omitempty"`
	// VMIDRange holds the template containers made from images; it must not overlap proxmox.vmid_range.
	VMIDRange [2]int `yaml:"vmid_range"`
	// CacheStorage is the Proxmox storage whose vztmpl directory keeps downloaded archives.
	CacheStorage string `yaml:"cache_storage"`
	// RootfsGB is the template's root disk; runners grow it to their pool's disk_gb.
	RootfsGB int `yaml:"rootfs_gb"`
	// CheckInterval is how often the service looks for newer images and prunes old ones.
	CheckInterval time.Duration `yaml:"check_interval"`
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
		Proxmox: Proxmox{MaxRunners: 4, VMIDRange: [2]int{9000, 9499}, Pool: "gh-runners", MinFreeMemoryMB: 2048,
			Bridge: "vmbr0"},
		GitHub: GitHub{
			APIURL:        "https://api.github.com",
			RunnerVersion: buildinfo.RunnerVersion,
			RunnerSHA256:  buildinfo.RunnerSHA256,
		},
		Pools: map[string]*Pool{},
		Images: Images{RetentionDays: 30, VMIDRange: [2]int{8900, 8999}, CacheStorage: "local", RootfsGB: 16,
			CheckInterval: 6 * time.Hour},
		Network: Network{
			Blocked: append([]string(nil), pve.DefaultBlocked...),
			// Runners still need name resolution and DHCP leases, which at home usually come from
			// the router on the LAN. Only those ports are let through.
			Allow: []pve.AllowRule{{Proto: "udp", Port: 53}, {Proto: "tcp", Port: 53}, {Proto: "udp", Port: 67}},
		},
		Autoscale: Autoscale{Enabled: true, StartGrace: 3 * time.Minute},
		Cache:     Cache{Dir: "/var/lib/pve-runners/cache", MaxGB: 50},
		Logs:      Logs{Interval: 15 * time.Second},
		Web:       Web{Listen: ":8765"},
		Update: Update{
			Enabled: true, Channel: "edge", Interval: 15 * time.Minute,
			Repo: "ServerTooling/proxmox-github-runners",
		},
	}
}

// DefaultImage is the published runner image a new host's default pool uses.
const DefaultImage = "ubuntu-latest"

// Initial is the config a fresh install starts from: every default, plus one pool on the published
// image with the `proxmox` label. Setup adds the GitHub targets.
func Initial(node string) *Config {
	c := Default()
	c.Proxmox.Node = node
	c.Pools["default"] = &Pool{Image: DefaultImage, Labels: []string{"proxmox"}}
	c.applyDefaults()
	return c
}

// Init writes Initial(node) to path unless a non-empty file is already there, and reports whether it
// wrote. Existing configs are never overwritten.
func Init(path, node string) (bool, error) {
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, Initial(node).Save(path)
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
	if c.Proxmox.Bridge == "" {
		c.Proxmox.Bridge = d.Proxmox.Bridge
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = d.GitHub.APIURL
	}
	// An empty runner pin follows the version baked into this build, so self-update also rolls the
	// runner agent forward. Setting both fields in the file pins it explicitly.
	c.GitHub.pinned = c.GitHub.RunnerVersion != "" && c.GitHub.RunnerVersion != d.GitHub.RunnerVersion ||
		c.GitHub.pinned
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
	if c.Images.Repo == "" {
		c.Images.Repo = c.Update.Repo
	}
	if c.Images.VMIDRange == [2]int{} {
		c.Images.VMIDRange = d.Images.VMIDRange
	}
	if c.Images.CacheStorage == "" {
		c.Images.CacheStorage = d.Images.CacheStorage
	}
	if c.Images.RootfsGB == 0 {
		c.Images.RootfsGB = d.Images.RootfsGB
	}
	if c.Images.CheckInterval == 0 {
		c.Images.CheckInterval = d.Images.CheckInterval
	}
	if c.Web.Listen == "" {
		c.Web.Listen = d.Web.Listen
	}
	if c.Autoscale.StartGrace == 0 {
		c.Autoscale.StartGrace = d.Autoscale.StartGrace
	}
	if c.Logs.Interval == 0 {
		c.Logs.Interval = d.Logs.Interval
	}
	if c.Cache.Dir == "" {
		c.Cache.Dir = d.Cache.Dir
	}
	if c.Cache.MaxGB == 0 {
		c.Cache.MaxGB = d.Cache.MaxGB
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
		if p.Warm > 0 && p.WarmFor == 0 {
			p.WarmFor = 30 * time.Minute
		}
		// Always attach a NIC with the firewall flag so isolation holds whatever the template had.
		if p.Bridge == "" {
			p.Bridge = c.Proxmox.Bridge
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
	// Decoding over the defaults means a key left out of the file keeps its default, including
	// booleans like update.enabled that applyDefaults can't tell apart from an explicit false.
	c := Default()
	c.Version = 0
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// An empty file is a config with every default, which `config init` and setup then fill in.
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("parse config: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		c.Version = CurrentVersion
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

// Save writes the commented config file atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	data, err := c.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var (
	labelRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// Linux interface names: at most 15 characters.
	bridgeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	imageRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(@template-[a-z0-9._-]+)?$`)
)

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
	if !bridgeRe.MatchString(c.Proxmox.Bridge) {
		add("proxmox.bridge %q is not a valid bridge name", c.Proxmox.Bridge)
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
	for i, ip := range c.Network.DNS {
		if net.ParseIP(ip) == nil {
			add("network.dns[%d] %q is not an IP address", i, ip)
		}
	}
	switch c.Notify.Format {
	case "", "auto", "ntfy", "discord", "slack":
	default:
		add("notify.format must be auto, ntfy, discord or slack")
	}
	if c.Logs.URL != "" {
		if u, err := url.Parse(c.Logs.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("logs.url %q must be an http(s) URL", c.Logs.URL)
		}
		if c.Logs.TokenFile == "" {
			add("logs.url is set but logs.token_file is not")
		}
	}
	if _, port, err := net.SplitHostPort(c.Web.Listen); err != nil || port == "" {
		add("web.listen %q must be [address]:port, such as :8765", c.Web.Listen)
	}
	if c.Autoscale.StartGrace < 30*time.Second {
		add("autoscale.start_grace must be at least 30s, got %s", c.Autoscale.StartGrace)
	}
	if c.Logs.Interval < 5*time.Second {
		add("logs.interval must be at least 5s, got %s", c.Logs.Interval)
	}
	// The cache folder gets emptied, so it must be a dedicated folder, never / or a top-level one.
	if d := filepath.Clean(c.Cache.Dir); !filepath.IsAbs(c.Cache.Dir) || strings.Count(d, "/") < 2 {
		add("cache.dir %q must be an absolute path at least two folders deep, such as /var/lib/pve-runners/cache", c.Cache.Dir)
	}
	if c.Cache.MaxGB < 1 {
		add("cache.max_gb must be at least 1, got %d", c.Cache.MaxGB)
	}
	if c.Images.RetentionDays < 1 {
		add("images.retention_days must be at least 1, got %d", c.Images.RetentionDays)
	}
	ilo, ihi := c.Images.VMIDRange[0], c.Images.VMIDRange[1]
	if ilo < 100 || ihi < ilo {
		add("images.vmid_range must be [low, high] with 100 <= low <= high, got [%d, %d]", ilo, ihi)
	} else if ilo <= hi && lo <= ihi {
		add("images.vmid_range [%d, %d] overlaps proxmox.vmid_range [%d, %d]", ilo, ihi, lo, hi)
	}
	if c.Images.RootfsGB < 4 {
		add("images.rootfs_gb must be at least 4, got %d", c.Images.RootfsGB)
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
		switch {
		case (p.Template == 0) == (p.Image == ""):
			add("pools.%s needs exactly one of image (a published template image) or template (a container ID)", name)
		case p.Image != "":
			if !imageRe.MatchString(p.Image) {
				add("pools.%s.image %q must be a name like ubuntu-latest, optionally pinned as name@template-name-date-run", name, p.Image)
			} else if p.DiskGB > 0 && p.DiskGB < c.Images.RootfsGB {
				add("pools.%s.disk_gb (%d) is smaller than images.rootfs_gb (%d); Proxmox can't shrink disks", name, p.DiskGB, c.Images.RootfsGB)
			}
		case p.Template < 100:
			add("pools.%s.template must be a guest ID (>= 100)", name)
		case p.Template >= lo && p.Template <= hi:
			add("pools.%s.template %d is inside vmid_range; templates must live outside it", name, p.Template)
		}
		if p.Bridge != "" && !bridgeRe.MatchString(p.Bridge) {
			add("pools.%s.bridge %q is not a valid bridge name", name, p.Bridge)
		}
		if p.Min > p.Max {
			add("pools.%s.min (%d) is larger than max (%d)", name, p.Min, p.Max)
		}
		if p.Min < 0 || p.Warm < 0 || p.WarmFor < 0 {
			add("pools.%s: min, warm and warm_for can't be negative", name)
		}
		if p.Warm > p.Max {
			add("pools.%s.warm (%d) is larger than max (%d)", name, p.Warm, p.Max)
		}
		for _, l := range p.Labels {
			if !labelRe.MatchString(l) {
				add("pools.%s has invalid label %q", name, l)
			}
		}
		if ss := p.ScaleSetName(); ss != "" && !labelRe.MatchString(ss) {
			add("pools.%s.scale_set %q may only use letters, digits, '.', '_' and '-'", name, ss)
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
		scaleSets := map[string]string{}
		for _, pn := range t.Pools {
			p, ok := c.Pools[pn]
			if !ok {
				add("targets[%d] (%s) uses unknown pool %q", i, t.Owner(), pn)
				continue
			}
			// One target's pools share its runner group, where scale set names must be unique.
			ss := strings.ToLower(p.ScaleSetName())
			if ss == "" {
				add("pools.%s needs a label (workflows pick a pool with runs-on: <label>)", pn)
				continue
			}
			if other, dup := scaleSets[ss]; dup {
				add("targets[%d] (%s): pools %s and %s both use scale set %q; set scale_set on one of them", i, t.Owner(), other, pn, ss)
			}
			scaleSets[ss] = pn
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
