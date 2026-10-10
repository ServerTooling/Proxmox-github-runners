package provision

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/github/githubtest"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

func setup(t *testing.T) (*Provisioner, *pvetest.Fake, *githubtest.Fake) {
	t.Helper()
	gh := githubtest.New(42)
	t.Cleanup(gh.Close)
	gh.RunnerGroups["example-org"] = []map[string]any{{"id": 7, "name": "proxmox"}}
	f := pvetest.New(pve.Guest{VMID: 8000, Type: "lxc", Template: 1, Node: "pve1"})
	f.ReadSrc = func(p string) (string, error) { b, err := os.ReadFile(p); return string(b), err }
	cfg := config.Default()
	cfg.Pools["small"] = &config.Pool{Type: "lxc", Template: 8000, Mode: "ephemeral", Labels: []string{"proxmox"},
		Cores: 2, MemoryMB: 2048, DiskGB: 40, Max: 2, Bridge: "vmbr1", VLAN: 50}
	cfg.Proxmox.VMIDRange = [2]int{9000, 9010}
	cfg.HostID = "pve1"
	p := &Provisioner{Cfg: cfg, Host: &pve.Host{R: f, Node: "pve1", VMIDRange: cfg.Proxmox.VMIDRange},
		GitHub: func(config.Target) (JITSource, error) { return github.NewClient(gh.URL, 42, gh.Key), nil }, Now: func() time.Time { return time.Unix(1700000000, 0) }, TmpDir: t.TempDir()}
	return p, f, gh
}

var org = config.Target{Org: "Example-Org", InstallationID: 5, RunnerGroup: "proxmox", Pools: []string{"small"}}

func TestSpawnRunsExactSequence(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	res, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org})
	if err != nil {
		t.Fatal(err)
	}
	if res.VMID != 9000 || res.Name != "gh-pve1-small-9000" {
		t.Fatalf("got %+v", res)
	}
	m := f.Mutations()
	// The first run creates the egress group: 1 create + 3 allows x 6 ranges + 6 drops.
	group := m[:1+3*6+6]
	if group[0] != "pvesh create /cluster/firewall/groups --group gh-runner-egress --comment Runner guests: internet only, no LAN (managed by pve-runners)" ||
		group[1] != "pvesh create /cluster/firewall/groups/gh-runner-egress --pos 0 --type out --action ACCEPT --dest 10.0.0.0/8 --enable 1 --comment pve-runners LAN exception --proto udp --dport 53" ||
		group[len(group)-1] != "pvesh create /cluster/firewall/groups/gh-runner-egress --pos 23 --type out --action DROP --dest fc00::/7 --enable 1 --comment pve-runners: no LAN access" {
		t.Fatalf("egress group: %q", group)
	}
	m = m[len(group):]
	push := m[6]
	want := []string{
		"pct clone 8000 9000 --hostname gh-pve1-small-9000 --pool gh-runners",
		"pct set 9000 --cores 2 --memory 2048 --tags gh-runner;pool-small;owner-example-org;mode-ephemeral;from-8000;created-1700000000 --onboot 0 --description GitHub Actions runner managed by pve-runners --net0 name=eth0,bridge=vmbr1,ip=dhcp,firewall=1,tag=50",
		"pct resize 9000 rootfs 40G",
		"pvesh set /nodes/pve1/lxc/9000/firewall/options --enable 1 --policy_in DROP --policy_out ACCEPT --dhcp 1 --ipfilter 1",
		"pvesh create /nodes/pve1/lxc/9000/firewall/rules --type group --action gh-runner-egress --enable 1",
		"pct start 9000",
		push,
		"pct exec 9000 -- systemctl start gh-runner.service",
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("commands\n got %q\nwant %q", m, want)
	}
	if !strings.HasPrefix(push, "pct push 9000 ") || !strings.HasSuffix(push, " /etc/gh-runner/jitconfig --perms 0600") {
		t.Fatalf("push: %q", push)
	}
	if f.Pushed["9000:/etc/gh-runner/jitconfig"] == "" {
		t.Fatal("JIT config not pushed")
	}
	for _, c := range f.Calls {
		if strings.Contains(c, f.Pushed["9000:/etc/gh-runner/jitconfig"]) {
			t.Fatalf("JIT config leaked into a command line: %q", c)
		}
	}
	tmp, _ := os.ReadDir(p.TmpDir)
	if len(tmp) != 0 {
		t.Fatalf("temp secret left behind: %v", tmp)
	}
	body := gh.JITRequests[0].Body
	if body["runner_group_id"] != float64(7) || !reflect.DeepEqual(body["labels"], []any{"self-hosted", "linux", "proxmox"}) {
		t.Fatalf("jit body %v", body)
	}
}

func TestSecondSpawnLeavesEgressGroupAlone(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	for i := 0; i < 2; i++ {
		if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, c := range f.Mutations() {
		if strings.Contains(c, "/cluster/firewall/groups") {
			n++
		}
	}
	if n != 25 {
		t.Fatalf("group should be written once (25 calls), got %d", n)
	}
}

func TestSpawnRefusesWhenFirewallOff(t *testing.T) {
	p, f, gh := setup(t)
	f.FirewallOn = false
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); !errors.Is(err, ErrFirewallOff) {
		t.Fatalf("got %v", err)
	}
	if len(f.Mutations()) != 0 || len(gh.JITRequests) != 0 {
		t.Fatal("nothing may be created or registered without isolation")
	}
	off := false
	p.Cfg.Network.Isolate = &off
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatalf("isolate=false should spawn: %v", err)
	}
}

func TestSpawnCleansUpOnFailure(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	f.FailOn["pct exec"] = errors.New("boom")
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err == nil {
		t.Fatal("expected failure")
	}
	m := f.Mutations()
	if last := m[len(m)-1]; last != "pct destroy 9000 --purge 1 --force 1" {
		t.Fatalf("half-built guest not destroyed: %q", m)
	}
}

func TestSpawnRespectsCapacity(t *testing.T) {
	p, f, _ := setup(t)
	f.Guests = append(f.Guests,
		pve.Guest{VMID: 9000, Type: "lxc", RawTags: "gh-runner;pool-small"},
		pve.Guest{VMID: 9001, Type: "lxc", RawTags: "gh-runner;pool-small"})
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("got %v", err)
	}
	f.Guests = f.Guests[:1]
	f.FreeMB = 3000 // pool needs 2048 + 2048 floor
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("got %v", err)
	}
	f.FreeMB = 32768
	p.Cfg.Proxmox.MaxRunners = 1 // host-wide cap applies across pools
	f.Guests = append(f.Guests, pve.Guest{VMID: 9005, Type: "lxc", RawTags: "gh-runner;pool-other"})
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); !errors.Is(err, ErrAtCapacity) || !strings.Contains(err.Error(), "max_runners") {
		t.Fatalf("got %v", err)
	}
	if len(f.Mutations()) != 0 {
		t.Fatalf("nothing should be created: %v", f.Mutations())
	}
}

func TestSpawnPersonalAccountUsesRepoRunner(t *testing.T) {
	p, _, gh := setup(t)
	gh.RepoVisibility["someone/side"] = "private"
	user := config.Target{User: "someone", InstallationID: 6, Repos: []string{"side"}, Pools: []string{"small"}}
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: user}); err == nil {
		t.Fatal("expected repo required")
	}
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: user, Repo: "side"}); err != nil {
		t.Fatal(err)
	}
	if gh.JITRequests[0].Path != "/repos/someone/side/actions/runners/generate-jitconfig" {
		t.Fatalf("path %s", gh.JITRequests[0].Path)
	}
}

func TestSpawnFromImageClonesItsTemplate(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	f.Guests = append(f.Guests, pve.Guest{VMID: 8900, Type: "lxc", Template: 1, Node: "pve1"})
	p.Cfg.Pools["small"].Template, p.Cfg.Pools["small"].Image = 0, "ubuntu-latest"
	var asked string
	p.Templates = func(_ context.Context, image string) (int, error) { asked = image; return 8900, nil }
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatal(err)
	}
	m := strings.Join(f.Mutations(), "\n")
	if asked != "ubuntu-latest" || !strings.Contains(m, "pct clone 8900 9000 ") || !strings.Contains(m, ";from-8900;") {
		t.Fatalf("image %q, commands:\n%s", asked, m)
	}

	p.Templates = func(context.Context, string) (int, error) { return 0, errors.New("no published image") }
	before := len(f.Mutations())
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err == nil || !strings.Contains(err.Error(), "no published image") {
		t.Fatalf("got %v", err)
	}
	if len(f.Mutations()) != before {
		t.Fatal("nothing may be created when the image is unavailable")
	}
}

func TestPrepareTemplate(t *testing.T) {
	f := pvetest.New(pve.Guest{VMID: 8000, Type: "lxc", Status: "stopped"})
	h := &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9010}}
	if err := PrepareTemplate(context.Background(), h, 9005, "/x.tgz", t.TempDir()); err == nil {
		t.Fatal("in-range template must be refused")
	}
	if err := PrepareTemplate(context.Background(), h, 8000, "/x.tgz", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()
	want := []string{"pct set 8000 --features nesting=1,keyctl=1", "pct start 8000", "pct push 8000 /x.tgz /tmp/actions-runner.tar.gz --perms 0644"}
	if !reflect.DeepEqual(m[:3], want) || m[len(m)-1] != "pct template 8000" {
		t.Fatalf("got %q", m)
	}
}

func TestSpawnWithJITCallbackSkipsTheApp(t *testing.T) {
	p, f, gh := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	var asked string
	req := Request{Pool: "small", Target: org, Tags: []string{"scaleset-3"},
		JIT: func(_ context.Context, name string) (*github.JITConfig, error) {
			asked = name
			return &github.JITConfig{EncodedJITConfig: "from-scale-set"}, nil
		}}
	if _, err := p.Spawn(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if asked != "gh-pve1-small-9000" {
		t.Errorf("JIT asked for %q", asked)
	}
	if len(gh.JITRequests) != 0 {
		t.Errorf("the App's JIT endpoint was used: %+v", gh.JITRequests)
	}
	if got := f.Pushed["9000:/etc/gh-runner/jitconfig"]; got != "from-scale-set" {
		t.Errorf("pushed %q", got)
	}
	if !strings.Contains(f.Mutations()[1], "--tags gh-runner;pool-small;owner-example-org;mode-ephemeral;from-8000;created-1700000000;scaleset-3 ") {
		t.Errorf("set: %q", f.Mutations()[1])
	}
}

func TestConcurrentSpawnsRespectCapacity(t *testing.T) {
	p, f, _ := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	f.Delay = time.Millisecond
	jit := func(context.Context, string) (*github.JITConfig, error) {
		return &github.JITConfig{EncodedJITConfig: "x"}, nil
	}
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit})
			errs <- err
		}()
	}
	ok, full := 0, 0
	for range 4 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrAtCapacity):
			full++
		default:
			t.Fatal(err)
		}
	}
	if ok != 2 || full != 2 {
		t.Fatalf("%d started, %d refused; pool max is 2", ok, full)
	}
}

func TestSpawnCreatesMissingResourcePool(t *testing.T) {
	p, f, _ := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	f.ResourcePools = nil
	jit := func(context.Context, string) (*github.JITConfig, error) {
		return &github.JITConfig{EncodedJITConfig: "x"}, nil
	}
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit}); err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()
	if m[0] != "pvesh create /pools --poolid gh-runners --comment GitHub runners managed by pve-runners" ||
		!strings.HasPrefix(m[1], "pct clone 8000 9000 ") {
		t.Fatalf("commands: %q", m)
	}
}
