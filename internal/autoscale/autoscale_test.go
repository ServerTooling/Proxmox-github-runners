package autoscale

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset/scalesettest"
)

var org = config.Target{Org: "example-org", InstallationID: 5, Pools: []string{"small"}}

type harness struct {
	l      *Listener
	svc    *scalesettest.Fake
	pve    *pvetest.Fake
	mu     sync.Mutex
	alerts []string
	public map[string]bool
}

func newHarness(t *testing.T, guests ...pve.Guest) *harness {
	t.Helper()
	svc := scalesettest.New()
	t.Cleanup(svc.Close)
	f := pvetest.New(append([]pve.Guest{{VMID: 8000, Type: "lxc", Template: 1, Node: "pve1"}}, guests...)...)
	f.ReadSrc = func(p string) (string, error) { b, err := os.ReadFile(p); return string(b), err }
	cfg := config.Default()
	cfg.HostID = "pve1"
	cfg.Proxmox.VMIDRange = [2]int{9000, 9010}
	cfg.Network.Isolate = new(bool)
	pool := &config.Pool{Type: "lxc", Template: 8000, Mode: "ephemeral", Labels: []string{"proxmox"},
		Cores: 2, MemoryMB: 2048, Max: 2, Bridge: "vmbr4"}
	cfg.Pools["small"] = pool
	host := &pve.Host{R: f, Node: "pve1", VMIDRange: cfg.Proxmox.VMIDRange}
	client := &scaleset.Client{APIBase: svc.URL, ConfigURL: "https://github.com/example-org", HTTP: svc.Client(),
		RegToken: func(context.Context) (string, error) { return svc.RegToken, nil }}
	h := &harness{svc: svc, pve: f, public: map[string]bool{}}
	h.l = &Listener{Service: client, Target: org, PoolName: "small", Pool: pool, GroupID: 1, HostID: "pve1",
		HostMax: cfg.Proxmox.MaxRunners, Host: host,
		Prov: &provision.Provisioner{Cfg: cfg, Host: host, TmpDir: t.TempDir(), Now: time.Now},
		Private: func(_ context.Context, owner, repo string) (bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return !h.public[owner+"/"+repo], nil
		},
		Failed: func(_ context.Context, msg string) {
			h.mu.Lock()
			h.alerts = append(h.alerts, msg)
			h.mu.Unlock()
		},
		ConflictWait: 10 * time.Millisecond}
	return h
}

// start runs the listener in the background and returns a function that stops it and waits.
func (h *harness) start(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.l.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
	t.Cleanup(stop)
	return stop
}

func (h *harness) runners(t *testing.T) []pve.Guest {
	t.Helper()
	rs, err := h.l.Host.Runners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestQueuedJobGetsARunner(t *testing.T) {
	h := newHarness(t)
	stop := h.start(t)
	eventually(t, "session", func() bool { return h.svc.Sessions() == 1 })
	if ss := h.svc.ScaleSets(); len(ss) != 1 || ss[0].Name != "proxmox" || ss[0].RunnerGroupID != 1 {
		t.Fatalf("scale sets = %+v", ss)
	}
	if len(h.runners(t)) != 0 {
		t.Fatal("no job yet, but a runner was started")
	}

	h.svc.Push(1, scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 77, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "runner", func() bool { return len(h.runners(t)) == 1 })
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	stop()

	if got := h.svc.Acquired(); len(got) != 1 || got[0] != 77 {
		t.Fatalf("acquired = %v", got)
	}
	g := h.runners(t)[0]
	if !g.HasTag("scaleset-1") || !g.HasTag("owner-example-org") || !g.HasTag("pool-small") {
		t.Errorf("tags = %q", g.RawTags)
	}
	if names := h.svc.JITNames(); len(names) != 1 || names[0] != "gh-pve1-small-9000" {
		t.Fatalf("JIT names = %v", names)
	}
	if got := h.pve.Pushed["9000:/etc/gh-runner/jitconfig"]; got != "jit-gh-pve1-small-9000" {
		t.Errorf("pushed JIT config = %q", got)
	}
	if h.svc.Sessions() != 0 {
		t.Error("session not deleted on shutdown")
	}
	// Steady state: one assigned job, one live runner, so empty polls start nothing more.
	if n := len(h.svc.JITNames()); n != 1 {
		t.Errorf("%d runners registered", n)
	}
}

func TestPublicRepoJobIsNotAcquired(t *testing.T) {
	h := newHarness(t)
	h.public["example-org/open-source"] = true
	stop := h.start(t)
	h.svc.Push(0, scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 1, RepositoryName: "open-source", OwnerName: "example-org"},
		scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 2, RepositoryName: "other", OwnerName: "someone-else"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	stop()
	if got := h.svc.Acquired(); len(got) != 0 {
		t.Fatalf("acquired %v", got)
	}
}

func TestReposListLimitsJobs(t *testing.T) {
	h := newHarness(t)
	h.l.Target.Repos = []string{"App"}
	stop := h.start(t)
	h.svc.Push(0, scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"},
		scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 2, RepositoryName: "tools", OwnerName: "example-org"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	stop()
	if got := h.svc.Acquired(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("acquired %v", got)
	}
}

func TestRestartAdoptsExistingRunners(t *testing.T) {
	now := time.Now()
	h := newHarness(t,
		// Still running its job.
		pve.Guest{VMID: 9003, Type: "lxc", Status: "running", RawTags: strings.Join(append(
			provision.Tags("small", "example-org", "ephemeral", 8000, now.Add(-time.Hour)), "scaleset-1"), ";")},
		// Finished: reconcile will destroy it, so it doesn't count.
		pve.Guest{VMID: 9004, Type: "lxc", Status: "stopped", RawTags: strings.Join(append(
			provision.Tags("small", "example-org", "ephemeral", 8000, now.Add(-time.Hour)), "scaleset-1"), ";")},
		// Another org's scale set 1.
		pve.Guest{VMID: 9005, Type: "lxc", Status: "running", RawTags: strings.Join(append(
			provision.Tags("small", "other-org", "ephemeral", 8000, now.Add(-time.Hour)), "scaleset-1"), ";")})
	h.svc.Assigned = 1
	h.start(t)
	eventually(t, "session", func() bool { return h.svc.Sessions() == 1 })
	time.Sleep(100 * time.Millisecond) // several empty polls
	if n := len(h.svc.JITNames()); n != 0 {
		t.Fatalf("started %d runners; the running one already covers the assigned job", n)
	}
}

func TestCapacityCapsRunners(t *testing.T) {
	h := newHarness(t)
	h.l.Pool.Min = 1
	stop := h.start(t)
	eventually(t, "min idle runner", func() bool { return len(h.runners(t)) == 1 })
	h.svc.Push(5)
	eventually(t, "runners at pool max", func() bool { return len(h.runners(t)) == 2 })
	time.Sleep(100 * time.Millisecond)
	stop()
	if n := len(h.runners(t)); n != 2 {
		t.Fatalf("%d runners, pool max is 2", n)
	}
	if len(h.alerts) != 0 {
		t.Errorf("capacity is not a failure: %v", h.alerts)
	}
	polls := h.svc.CallsTo("GET", "/queue/*")
	if c := polls[0].Header.Get("X-ScaleSetMaxCapacity"); c != "2" {
		t.Errorf("max capacity header = %q", c)
	}
}

func TestSpawnFailureAlertsOnce(t *testing.T) {
	h := newHarness(t)
	h.pve.FailOn["pct clone"] = errors.New("storage full")
	h.svc.Assigned = 1
	h.start(t)
	eventually(t, "a few retries", func() bool { return len(h.svc.CallsTo("GET", "/queue/*")) > 3 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "storage full") || !strings.HasPrefix(h.alerts[0], "example-org proxmox: ") {
		t.Fatalf("alerts = %q", h.alerts)
	}
}

func TestSessionConflictRetried(t *testing.T) {
	h := newHarness(t)
	h.svc.Conflict(2)
	h.start(t)
	eventually(t, "session", func() bool { return h.svc.Sessions() == 1 })
	if n := len(h.svc.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets/1/sessions")); n != 3 {
		t.Fatalf("%d session attempts", n)
	}
}

func TestDesired(t *testing.T) {
	for _, c := range []struct{ assigned, min, max, want int }{
		{0, 0, 4, 0}, {3, 0, 4, 3}, {9, 0, 4, 4}, {1, 1, 4, 2}, {0, 2, 4, 2},
	} {
		if got := Desired(scaleset.Statistics{TotalAssignedJobs: c.assigned}, c.min, c.max); got != c.want {
			t.Errorf("Desired(%d, min %d, max %d) = %d, want %d", c.assigned, c.min, c.max, got, c.want)
		}
	}
}
