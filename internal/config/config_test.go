package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
version: 1
proxmox:
  node: pve1
targets:
  - org: example-org
    app_id: 42
    installation_id: 7
    pools: [small]
pools:
  small:
    template: 8000
    labels: [proxmox, lxc-small]
`

func TestParseAppliesDefaults(t *testing.T) {
	c, migrated, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Fatal("v1 file should not migrate")
	}
	p := c.Pools["small"]
	if p.Type != "lxc" || p.Mode != "ephemeral" || p.Cores != 4 || p.MemoryMB != 8192 || p.DiskGB != 40 || p.Max != 4 || c.Proxmox.MaxRunners != 4 {
		t.Fatalf("pool defaults not applied: %+v", p)
	}
	if c.HostID != "pve1" {
		t.Fatalf("host_id should default to the node name, got %q", c.HostID)
	}
	if c.Proxmox.VMIDRange != [2]int{9000, 9499} || c.Update.Channel != "edge" || c.Update.Interval != 15*time.Minute || c.Images.RetentionDays != 30 ||
		c.Logs.On() || c.Logs.Interval != 15*time.Second || !c.Web.On() || c.Web.Listen != ":8765" {
		t.Fatalf("defaults not applied: %+v %+v %+v", c.Proxmox, c.Update, c.Logs)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	bad := `
proxmox: {vmid_range: [9000, 9499], max_runners: -1}
update: {channel: nightly}
images: {retention_days: -5}
network: {dns: [not-an-ip]}
logs: {url: "ftp://x", interval: 1s}
web: {listen: "8765"}
cache: {dir: /cache, max_gb: -1}
targets:
  - org: a
    user: b
    pools: [missing]
  - user: someone
    installation_id: 3
    pools: []
pools:
  "bad name":
    template: 9100
    mode: forever
    min: 5
    max: 1
    warm: 2
    labels: ["has space"]
`
	_, _, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"update.channel", "max_runners", "retention_days", "exactly one of org or user", "no installation_id", "no app_id", "unknown pool \"missing\"",
		"personal account", "pool name \"bad name\"", "mode must be", "inside vmid_range", "min (5)", "invalid label", "network.dns[0]", "logs.url \"ftp://x\"", "logs.token_file is not", "logs.interval", "web.listen",
		"cache.dir \"/cache\"", "cache.max_gb", "warm (2)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	_, _, err := Parse([]byte(valid + "\nsurprise: true\n"))
	if err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadMigratesUnversionedFileAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	orig := strings.Replace(valid, "version: 1\n", "", 1)
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != CurrentVersion {
		t.Fatalf("version %d", c.Version)
	}
	bak, _ := os.ReadFile(path + ".bak")
	if string(bak) != orig {
		t.Fatal("backup should hold the original file")
	}
	again, err := Load(path)
	if err != nil || again.Pools["small"].Template != 8000 {
		t.Fatalf("rewritten file did not round-trip: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perms %v", fi.Mode().Perm())
	}
}

func TestPermanentModeReservedButRejected(t *testing.T) {
	_, _, err := Parse([]byte(valid + "    mode: permanent\n"))
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("got %v", err)
	}
}

func TestNewerVersionRefused(t *testing.T) {
	_, _, err := Parse([]byte(strings.Replace(valid, "version: 1", "version: 99", 1)))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("got %v", err)
	}
}

func TestImagePools(t *testing.T) {
	c, _, err := Parse([]byte(strings.Replace(valid, "template: 8000", "image: ubuntu-latest", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Images.Repo != c.Update.Repo || c.Images.VMIDRange != [2]int{8900, 8999} || c.Images.CacheStorage != "local" ||
		c.Images.RootfsGB != 16 || c.Images.CheckInterval != 6*time.Hour || c.Pools["small"].Bridge != "vmbr0" || c.Proxmox.Bridge != "vmbr0" {
		t.Fatalf("image defaults not applied: %+v %+v", c.Images, c.Pools["small"])
	}
	for doc, want := range map[string]string{
		strings.Replace(valid, "template: 8000", "template: 8000\n    image: ubuntu-latest", 1): "exactly one of image",
		strings.Replace(valid, "template: 8000", "disk_gb: 0", 1):                               "exactly one of image",
		strings.Replace(valid, "template: 8000", "image: Ubuntu Latest", 1):                     "pools.small.image",
		strings.Replace(valid, "template: 8000", "image: ubuntu-latest\n    disk_gb: 8", 1):     "smaller than images.rootfs_gb",
		valid + "images: {vmid_range: [9400, 9600]}\n":                                          "overlaps proxmox.vmid_range",
	} {
		if _, _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	if _, _, err := Parse([]byte(strings.Replace(valid, "template: 8000", "image: ubuntu-latest@template-ubuntu-latest-20261012-5", 1))); err != nil {
		t.Fatalf("pinned image: %v", err)
	}
}

func TestBridgeIsHostWideWithPoolOverride(t *testing.T) {
	doc := strings.Replace(valid, "  node: pve1", "  node: pve1\n  bridge: vmbr4", 1) +
		"  other:\n    template: 8001\n    bridge: vmbr7\n"
	c, _, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools["small"].Bridge != "vmbr4" || c.Pools["other"].Bridge != "vmbr7" {
		t.Fatalf("bridges: small %q other %q", c.Pools["small"].Bridge, c.Pools["other"].Bridge)
	}
	if _, _, err := Parse([]byte(strings.Replace(valid, "  node: pve1", "  node: pve1\n  bridge: \"vmbr4; rm\"", 1))); err == nil ||
		!strings.Contains(err.Error(), "proxmox.bridge") {
		t.Fatalf("bad bridge accepted: %v", err)
	}
}

func TestRenderRoundTripsAndDocumentsEverySetting(t *testing.T) {
	c, _, err := Parse([]byte(valid + `
  img:
    image: ubuntu-latest@template-ubuntu-latest-20261012-5
    labels: [proxmox-img]
    max_lifetime: 6h
    vlan: 50
    full_clone: true
    min: 1
    warm: 2
    cache: true
    scale_set: pve-img
autoscale: {enabled: false}
cache: {dir: /srv/runner-cache, max_gb: 20}
network:
  allow: [{proto: udp, port: 53, dest: 192.0.2.1}]
  dns: [192.0.2.53, "2001:db8::53"]
notify: {webhook_url_file: /etc/pve-runners/notify-url, format: ntfy}
logs: {url: "https://logs.example.com", user: "123", token_file: /etc/pve-runners/loki-token, interval: 30s}
web: {enabled: false, listen: "127.0.0.1:9000"}
github: {runner_version: "9.9.9", runner_sha256: abc}
`))
	if err != nil {
		t.Fatal(err)
	}
	c.Targets = append(c.Targets, Target{User: "someone", AppID: 2, InstallationID: 3, Repos: []string{"side"}, Pools: []string{"img"}})
	c.applyDefaults()
	out, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := Parse(out)
	if err != nil {
		t.Fatalf("rendered config does not parse: %v\n%s", err, out)
	}
	// Rendering covers every field, so equal renders mean nothing was lost.
	again, _ := back.Render()
	if string(again) != string(out) {
		t.Fatalf("round trip changed the config:\n%s\n---\n%s", out, again)
	}
	img := back.Pools["img"]
	if img.Image != "ubuntu-latest@template-ubuntu-latest-20261012-5" || img.MaxLifetime != 6*time.Hour || img.VLAN != 50 ||
		!img.FullClone || img.Min != 1 || img.Warm != 2 || img.WarmFor != 30*time.Minute || !img.Cache ||
		back.Cache.Dir != "/srv/runner-cache" || back.Cache.MaxGB != 20 || img.ScaleSetName() != "pve-img" || back.Autoscale.Enabled || back.Notify.Format != "ntfy" || !back.GitHub.Pinned() ||
		back.Network.Allow[0].Dest != "192.0.2.1" || len(back.Network.DNS) != 2 || back.Network.DNS[1] != "2001:db8::53" || back.Targets[1].Repos[0] != "side" ||
		!back.Logs.On() || back.Logs.URL != "https://logs.example.com" || back.Logs.User != "123" ||
		back.Logs.Interval != 30*time.Second || back.Web.On() || back.Web.Listen != "127.0.0.1:9000" {
		t.Fatalf("values lost: %+v %+v", img, back)
	}
}

func TestInitWritesCommentedDefaultsOnlyWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, err := Parse([]byte("  \n")); err != nil || c.Proxmox.MaxRunners != 4 {
		t.Fatalf("an empty file should parse as defaults: %v", err)
	}
	wrote, err := Init(path, "pve1")
	if err != nil || !wrote {
		t.Fatalf("empty file not filled: %v", err)
	}
	data, _ := os.ReadFile(path)
	c, _, err := Parse(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	p := c.Pools["default"]
	if c.Proxmox.Node != "pve1" || c.Proxmox.MaxRunners != 4 || c.Proxmox.Bridge != "vmbr0" || p.Image != "ubuntu-latest" ||
		p.Labels[0] != "proxmox" || p.Mode != "ephemeral" || p.Cores != 4 || p.MemoryMB != 8192 || p.DiskGB != 40 ||
		c.Images.RetentionDays != 30 || c.Update.Channel != "edge" || c.Notify.WebhookURLFile != "" || c.GitHub.Pinned() {
		t.Fatalf("defaults: %+v %+v", c, p)
	}
	for _, key := range []string{"max_runners: 4", "bridge: vmbr0", "cores: 4", "memory_mb: 8192", "disk_gb: 40",
		"retention_days: 30", "labels: [proxmox]", "mode: ephemeral", "proto: udp, port: 53", "# webhook_url_file:",
		"channel: edge", "enabled: true", "# runner_version:", "# token_file: /etc/pve-runners/loki-token", "# Most runners at once on this host", "# bridge: vmbr1"} {
		if !strings.Contains(string(data), key) {
			t.Errorf("missing %q", key)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}

	// A non-empty file, even an edited one, is never overwritten.
	_ = os.WriteFile(path, []byte("proxmox: {node: mine}\n"), 0o600)
	if wrote, err := Init(path, "pve1"); err != nil || wrote {
		t.Fatalf("overwrote an existing config: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "proxmox: {node: mine}\n" {
		t.Fatalf("content changed: %q", b)
	}
}

func TestPoolBridgeFollowsHostBridgeUnlessOverridden(t *testing.T) {
	c := Initial("pve1")
	out, _ := c.Render()
	// Changing only proxmox.bridge in the written file must move the default pool too.
	edited := strings.Replace(string(out), "  bridge: vmbr0\n", "  bridge: vmbr4\n", 1)
	back, _, err := Parse([]byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	if back.Proxmox.Bridge != "vmbr4" || back.Pools["default"].Bridge != "vmbr4" {
		t.Fatalf("host %q pool %q", back.Proxmox.Bridge, back.Pools["default"].Bridge)
	}
}

func TestAutoscaleDefaultsAndScaleSetNames(t *testing.T) {
	c, _, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Autoscale.Enabled || c.Pools["small"].ScaleSetName() != "proxmox" {
		t.Fatalf("autoscale %v, scale set %q", c.Autoscale.Enabled, c.Pools["small"].ScaleSetName())
	}
	_, _, err = Parse([]byte(strings.Replace(valid, "pools: [small]", "pools: [small, big]", 1) + `
  big:
    template: 8001
    labels: [Proxmox]
`))
	if err == nil || !strings.Contains(err.Error(), `pools small and big both use scale set "proxmox"`) {
		t.Fatalf("err = %v", err)
	}
	_, _, err = Parse([]byte(strings.Replace(valid, "labels: [proxmox, lxc-small]", "labels: []", 1)))
	if err == nil || !strings.Contains(err.Error(), "pools.small needs a label") {
		t.Fatalf("err = %v", err)
	}
}
