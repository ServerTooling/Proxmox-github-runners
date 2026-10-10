package cache

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

func setup(guests ...pve.Guest) (*config.Config, *pve.Host, *pvetest.Fake) {
	cfg := config.Default()
	cfg.Proxmox.VMIDRange = [2]int{9000, 9010}
	cfg.Cache.MaxGB = 1
	cfg.Pools["cached"] = &config.Pool{Cache: true}
	cfg.Pools["plain"] = &config.Pool{}
	cfg.Targets = []config.Target{
		{Org: "Example-Org", Pools: []string{"plain", "cached"}},
		{Org: "busy-org", Pools: []string{"cached"}},
		{Org: "no-cache-org", Pools: []string{"plain"}},
	}
	f := pvetest.New(guests...)
	f.Outputs = map[string]string{
		"du -s -x -B1 /var/lib/pve-runners/cache/example-org": "2147483648\t/var/lib/pve-runners/cache/example-org\n",
		"du -s -x -B1 /var/lib/pve-runners/cache/busy-org":    "5368709120\t/var/lib/pve-runners/cache/busy-org\n",
	}
	return cfg, &pve.Host{R: f, Node: "pve1", VMIDRange: cfg.Proxmox.VMIDRange}, f
}

func runner(owner, status string) pve.Guest {
	return pve.Guest{VMID: 9001, Type: "lxc", Status: status,
		RawTags: strings.Join(provision.Tags("cached", owner, "ephemeral", 8000, time.Now()), ";")}
}

func TestTrimEmptiesOnlyOversizedIdleFolders(t *testing.T) {
	cfg, h, f := setup(runner("busy-org", "running"))
	got, err := Run(context.Background(), cfg, h, Trim)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{Owner: "Example-Org", Dir: "/var/lib/pve-runners/cache/example-org", Bytes: 2 << 30, Cleared: true},
		{Owner: "busy-org", Dir: "/var/lib/pve-runners/cache/busy-org", Bytes: 5 << 30, Skipped: "runners are using it"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if m := f.Mutations(); !reflect.DeepEqual(m, []string{"find /var/lib/pve-runners/cache/example-org -xdev -mindepth 1 -delete"}) {
		t.Fatalf("commands %q", m)
	}
}

func TestTrimLeavesFoldersUnderTheLimit(t *testing.T) {
	cfg, h, f := setup()
	cfg.Cache.MaxGB = 10
	got, err := Run(context.Background(), cfg, h, Trim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Cleared || got[1].Cleared || len(f.Mutations()) != 0 {
		t.Fatalf("got %+v, commands %q", got, f.Mutations())
	}
}

func TestClearEmptiesEveryIdleFolder(t *testing.T) {
	cfg, h, f := setup(runner("busy-org", "stopped"))
	cfg.Cache.MaxGB = 10
	got, err := Run(context.Background(), cfg, h, Clear)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Cleared || !got[1].Cleared {
		t.Fatalf("got %+v; a stopped runner no longer has the folder mounted", got)
	}
	if n := len(f.Mutations()); n != 2 {
		t.Fatalf("commands %q", f.Mutations())
	}
}

func TestListChangesNothing(t *testing.T) {
	cfg, h, f := setup()
	got, err := Run(context.Background(), cfg, h, List)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Bytes != 5<<30 || len(f.Mutations()) != 0 {
		t.Fatalf("got %+v, commands %q", got, f.Mutations())
	}
}
