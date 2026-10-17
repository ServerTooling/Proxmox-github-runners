package provision

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
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
		"pct set 9000 --cores 2 --memory 2048 --tags gh-runner;pool-small;owner-example-org;mode-ephemeral;from-8000;created-1700000000;runnerid-1 --onboot 0 --description GitHub Actions runner managed by pve-runners --net0 name=eth0,bridge=vmbr1,ip=dhcp,firewall=1,tag=50",
		"pct resize 9000 rootfs 40G",
		"pvesh set /nodes/pve1/lxc/9000/firewall/options --enable 1 --policy_in DROP --policy_out ACCEPT --dhcp 1 --ipfilter 1",
		"pvesh create /nodes/pve1/lxc/9000/firewall/rules --type group --action gh-runner-egress --enable 1",
		"pct start 9000",
		push,
		"pct exec 9000 -- sh -c " + startRunner,
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

func TestSpawnKeepsTemplateDiskWhenAlreadyThatSize(t *testing.T) {
	for _, msg := range []string{"disk is already at specified size", "unable to shrink disk size"} {
		p, f, gh := setup(t)
		gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
		f.FailOn["pct resize"] = errors.New("pct resize 9000 rootfs 40G: exit status 255: " + msg)
		if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
			t.Fatalf("%s: spawn failed: %v", msg, err)
		}
		for _, m := range f.Mutations() {
			if strings.HasPrefix(m, "pct destroy") {
				t.Fatalf("%s: runner destroyed: %q", msg, f.Mutations())
			}
		}
	}
}

func TestSpawnFailsOnOtherResizeErrors(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	f.FailOn["pct resize"] = errors.New("pct resize 9000 rootfs 40G: exit status 255: storage full")
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err == nil {
		t.Fatal("expected failure")
	}
}

func TestSpawnFullClonesWhereLinkedClonesCantGo(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Proxmox.Storage = "big-dir"
	p.NoLinkedClones = true
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.Mutations() {
		if strings.HasPrefix(m, "pct clone ") {
			if !strings.HasSuffix(m, " --full 1 --storage big-dir") {
				t.Fatalf("clone: %q", m)
			}
			return
		}
	}
	t.Fatalf("no clone: %q", f.Mutations())
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

// Proxmox locks the template for a whole clone, so clones run one at a time, but a runner's start
// and boot must not hold up the next runner's clone.
func TestSpawnStartsWhileTheNextOneClones(t *testing.T) {
	p, f, _ := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	jit := func(context.Context, string) (*github.JITConfig, error) {
		return &github.JITConfig{EncodedJITConfig: "x"}, nil
	}
	cloned := make(chan struct{})
	var once sync.Once
	f.Before = func(line string) {
		switch {
		case strings.HasPrefix(line, "pct clone 8000 9001 "):
			once.Do(func() { close(cloned) })
		case line == "pct start 9000":
			select {
			case <-cloned:
			case <-time.After(5 * time.Second):
				t.Error("the second clone waited for the first runner to start")
			}
		}
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit})
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	m := f.Mutations()
	set, clone := slices.IndexFunc(m, func(s string) bool { return strings.HasPrefix(s, "pct set 9000 ") }),
		slices.IndexFunc(m, func(s string) bool { return strings.HasPrefix(s, "pct clone 8000 9001 ") })
	if set < 0 || clone < set {
		// Untagged, the first guest isn't counted, so the second spawn could pass a full pool's check.
		t.Errorf("second clone at %d, before the first guest was tagged at %d: %q", clone, set, m)
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

func TestSpawnSetsConfiguredDNS(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Network.Isolate = new(bool)
	p.Cfg.Network.DNS = []string{"192.0.2.53", "198.51.100.53"}
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatal(err)
	}
	if set := f.Mutations()[1]; !strings.HasSuffix(set, " --nameserver 192.0.2.53 198.51.100.53") {
		t.Errorf("set: %q", set)
	}
}

// A pool with the cache on mounts the owner's folder and points the runner's tools into it.
func TestSpawnMountsTheOwnersCache(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Network.Isolate = new(bool)
	p.Cfg.Pools["small"].Cache = true
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()
	if m[1] != "install -d -m 0711 /var/lib/pve-runners/cache" || m[2] != "install -d -m 1777 /var/lib/pve-runners/cache/example-org" {
		t.Fatalf("folders: %q", m[1:3])
	}
	if !strings.HasSuffix(m[3], " --mp0 /var/lib/pve-runners/cache/example-org,mp=/opt/pve-cache") || !strings.HasPrefix(m[3], "pct set 9000 ") {
		t.Errorf("set: %q", m[3])
	}
	env := f.Pushed["9000:/opt/actions-runner/.env"]
	for _, want := range []string{"RUNNER_TOOL_CACHE=/opt/pve-cache/toolcache\n", "npm_config_cache=/opt/pve-cache/npm\n",
		"GOMODCACHE=/opt/pve-cache/go/mod\n", "PLAYWRIGHT_BROWSERS_PATH=/opt/pve-cache/ms-playwright\n"} {
		if !strings.Contains(env, want) {
			t.Errorf(".env lacks %q:\n%s", want, env)
		}
	}
	// The .env must be in place before the jitconfig, which lets the runner start at boot.
	if last := m[len(m)-1]; !strings.HasPrefix(last, "pct exec 9000 -- sh -c ") ||
		!strings.HasSuffix(m[len(m)-2], " /etc/gh-runner/jitconfig --perms 0600") ||
		!strings.HasSuffix(m[len(m)-3], " /opt/actions-runner/.env --perms 0644") {
		t.Errorf("order: %q", m[len(m)-3:])
	}
}

func TestSpawnWithoutCacheMountsNothing(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Network.Isolate = new(bool)
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Mutations() {
		if strings.Contains(c, "pve-cache") || strings.HasPrefix(c, "install ") {
			t.Errorf("cache touched: %q", c)
		}
	}
}

func TestDrainWaitsForTheRunningSpawnAndStopsNewOnes(t *testing.T) {
	p, f, _ := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	f.Delay = 20 * time.Millisecond
	old := drainPoll
	drainPoll = time.Millisecond
	t.Cleanup(func() { drainPoll = old; draining.Store(false) })
	jit := func(context.Context, string) (*github.JITConfig, error) {
		return &github.JITConfig{EncodedJITConfig: "x"}, nil
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit})
		spawned <- err
	}()
	for !Busy() {
		time.Sleep(time.Millisecond)
	}
	release, err := Drain(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-spawned:
		if err != nil {
			t.Fatalf("the running spawn should finish: %v", err)
		}
	default:
		t.Fatal("Drain returned before the running spawn finished")
	}
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit}); !errors.Is(err, ErrDraining) {
		t.Fatalf("want ErrDraining while drained, got %v", err)
	}
	release()
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit}); err != nil {
		t.Fatalf("spawns resume after release: %v", err)
	}
}

func TestDrainGivesUpAndReleases(t *testing.T) {
	old := drainPoll
	drainPoll = time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	inFlight.Add(1)
	defer inFlight.Add(-1)
	if _, err := Drain(context.Background(), 10*time.Millisecond); err == nil {
		t.Fatal("want a timeout while a spawn never finishes")
	}
	if draining.Load() {
		t.Fatal("a failed drain must not keep spawns stopped")
	}
}

// Every file the runner needs is in the guest before its service is started, and a service that
// isn't running afterwards fails the spawn (and the guest is destroyed) instead of leaving a guest
// with no runner in it.
func TestRunnerServiceStartsOnlyAfterFilesAndMustRun(t *testing.T) {
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Pools["small"].Cache = true
	var steps []string
	res, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org,
		Step: func(vmid int, s string) { steps = append(steps, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"provisioning", "files pushed", "service started"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}
	var order []string
	for _, c := range f.Mutations() {
		switch {
		case strings.HasPrefix(c, "pct start"):
			order = append(order, "start guest")
		case strings.HasSuffix(c, "/opt/actions-runner/.env --perms 0644"):
			order = append(order, ".env")
		case strings.HasSuffix(c, "/etc/gh-runner/jitconfig --perms 0600"):
			order = append(order, "jitconfig")
		case strings.Contains(c, "systemctl start gh-runner.service"):
			order = append(order, "start service")
		}
	}
	if want := []string{"start guest", ".env", "jitconfig", "start service"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %q, want %q", order, want)
	}
	if !strings.Contains(startRunner, "systemctl start gh-runner.service || exit 1\nexec systemctl is-active gh-runner.service") {
		t.Fatalf("the start must check the unit is running: %s", startRunner)
	}

	// The unit was skipped or failed: the spawn fails and the guest goes.
	f.FailOn["pct exec 9001 -- sh -c i=0"] = errors.New("exit status 3")
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org}); err == nil ||
		!strings.Contains(err.Error(), "runner service did not start in 9001") {
		t.Fatalf("got %v", err)
	}
	if _, ok := f.Guest(9001); ok {
		t.Fatal("the guest without a running runner was left behind")
	}
	_ = res
}

// The image's unit waits for its config instead of being skipped, and is not started at boot.
func TestRunnerUnitWaitsForItsConfig(t *testing.T) {
	s := string(templateSetupScript)
	if strings.Contains(s, "ConditionPathExists") {
		t.Error("a ConditionPathExists skips the unit silently when the config is late")
	}
	if !strings.Contains(s, "ExecStartPre=+/usr/bin/timeout 60 /bin/sh -c 'until [ -s /etc/gh-runner/jitconfig ]; do sleep 1; done'") {
		t.Error("the unit must wait up to 60 s for its config")
	}
	if strings.Contains(s, "systemctl enable") || !strings.Contains(s, "systemctl disable gh-runner.service") {
		t.Error("the unit must not start at boot, before the host pushes its files")
	}
	if strings.Contains(s, "$i") {
		t.Error("systemd expands $name in unit lines; the unit must not rely on shell variables")
	}
}

// hangingClone is the host with a pct clone that never returns on its own, like the real one did.
type hangingClone struct{ *pvetest.Fake }

func (h hangingClone) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "pct" && len(args) > 0 && args[0] == "clone" {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	return h.Fake.Run(ctx, name, args...)
}

func TestSpawnStopsAHungClone(t *testing.T) {
	p, f, _ := setup(t)
	p.Cfg.Network.Isolate = new(bool)
	p.Host.R = hangingClone{f}
	old := cloneTimeout
	cloneTimeout = 50 * time.Millisecond
	t.Cleanup(func() { cloneTimeout = old })
	jit := func(context.Context, string) (*github.JITConfig, error) {
		return &github.JITConfig{EncodedJITConfig: "x"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "pct clone 9000 of template 8000 did not finish") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Spawn still waits for a hung clone")
	}
	// The next spawn isn't held up by the lock the hung one took.
	p.Host.R = f
	if _, err := p.Spawn(context.Background(), Request{Pool: "small", Target: org, JIT: jit}); err != nil {
		t.Fatal(err)
	}
}
