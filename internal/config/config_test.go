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
	if c.Proxmox.VMIDRange != [2]int{9000, 9499} || c.Update.Channel != "edge" || c.Update.Interval != 15*time.Minute || c.Images.RetentionDays != 30 {
		t.Fatalf("defaults not applied: %+v %+v", c.Proxmox, c.Update)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	bad := `
proxmox: {vmid_range: [9000, 9499], max_runners: -1}
update: {channel: nightly}
images: {retention_days: -5}
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
    labels: ["has space"]
`
	_, _, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"update.channel", "max_runners", "retention_days", "exactly one of org or user", "no installation_id", "no app_id", "unknown pool \"missing\"",
		"personal account", "pool name \"bad name\"", "mode must be", "inside vmid_range", "min (5)", "invalid label",
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
