package autoscale

import (
	"context"
	"errors"
	"os"
	"slices"
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
		if got := Desired(Demand(scaleset.Statistics{TotalAssignedJobs: c.assigned}, 0, 0), c.min, c.max); got != c.want {
			t.Errorf("Desired(%d, min %d, max %d) = %d, want %d", c.assigned, c.min, c.max, got, c.want)
		}
	}
}

// clock is a listener clock tests can move forward.
type clock struct {
	mu  sync.Mutex
	add time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.add)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.add += d
	c.mu.Unlock()
}

func TestWarmSpareStaysAfterAJobThenLeaves(t *testing.T) {
	h := newHarness(t)
	h.l.Pool.Warm, h.l.Pool.WarmFor = 1, 30*time.Minute
	var c clock
	h.l.Now = c.now
	h.start(t)
	eventually(t, "session", func() bool { return h.svc.Sessions() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 0 {
		t.Fatalf("%d runners before any job; warm spares only follow a job", n)
	}

	h.svc.Push(1, scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "a runner for the job and a warm spare", func() bool { return len(h.runners(t)) == 2 })
	// The newer runner (9001, GitHub ID 102) took the job; the older one is the spare.
	h.svc.SetBusy(102, true)
	h.svc.Push(1, scalesettest.Job{Type: "JobStarted", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	c.advance(29 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 2 {
		t.Fatalf("%d runners inside warm_for, want 2", n)
	}

	c.advance(2 * time.Minute)
	eventually(t, "the spare removed", func() bool { return len(h.runners(t)) == 1 })
	if got := h.runners(t)[0].VMID; got != 9001 {
		t.Errorf("kept %d; the runner with the job must stay", got)
	}
	if got := h.svc.Removed(); len(got) != 1 || got[0] != 101 {
		t.Errorf("unregistered %v, want only the spare (101)", got)
	}
}

func TestBusyRunnerIsNeverRemoved(t *testing.T) {
	now := time.Now()
	tags := func(created time.Time, id string) string {
		return strings.Join(append(provision.Tags("small", "example-org", "ephemeral", 8000, created),
			"scaleset-1", "runnerid-"+id), ";")
	}
	h := newHarness(t,
		pve.Guest{VMID: 9003, Name: "gh-pve1-small-9003", Type: "lxc", Status: "running", RawTags: tags(now.Add(-time.Hour), "7")},
		pve.Guest{VMID: 9004, Name: "gh-pve1-small-9004", Type: "lxc", Status: "running", RawTags: tags(now.Add(-time.Minute), "8")})
	h.svc.AddRunner("gh-pve1-small-9003", 7)
	h.svc.AddRunner("gh-pve1-small-9004", 8)
	h.svc.SetBusy(7, true)
	h.svc.SetBusy(8, true)
	// GitHub's statistics lag behind: no assigned jobs, so both look surplus.
	h.start(t)
	eventually(t, "a few polls", func() bool { return len(h.svc.CallsTo("GET", "/queue/*")) > 3 })
	if n := len(h.runners(t)); n != 2 || len(h.svc.Removed()) != 0 {
		t.Fatalf("%d runners left, removed %v; GitHub said both have jobs", n, h.svc.Removed())
	}
	// One finishes its job without the container stopping yet, then it may go.
	h.svc.SetBusy(8, false)
	eventually(t, "the idle one removed", func() bool { return len(h.runners(t)) == 1 })
	if h.runners(t)[0].VMID != 9003 {
		t.Errorf("removed the busy runner")
	}
}

func TestIdle(t *testing.T) {
	var c clock
	l := &Listener{Pool: &config.Pool{Min: 1, Warm: 2, WarmFor: 30 * time.Minute}, Now: c.now}
	if n := l.idle(); n != 1 {
		t.Errorf("no job yet: %d, want min 1", n)
	}
	l.lastJob = c.now()
	c.advance(10 * time.Minute)
	if n := l.idle(); n != 3 {
		t.Errorf("10m after a job: %d, want 3", n)
	}
	c.advance(25 * time.Minute)
	if n := l.idle(); n != 1 {
		t.Errorf("35m after a job: %d, want 1", n)
	}
}

func TestJobsAreRememberedPerRunner(t *testing.T) {
	h := newHarness(t)
	h.l.Jobs = &Jobs{}
	stop := h.start(t)
	h.svc.Push(0, scalesettest.Job{Type: "JobStarted", RepositoryName: "app", OwnerName: "example-org",
		RunnerName: "gh-pve1-small-9000", JobDisplayName: "unit (2, 4)", JobWorkflowRef: "example-org/app/.github/workflows/ci.yml@refs/heads/main",
		WorkflowRunID: 42})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	got, ok := h.l.Jobs.Of("gh-pve1-small-9000")
	want := JobInfo{Repo: "example-org/app", Job: "unit (2, 4)", Workflow: "example-org/app/.github/workflows/ci.yml@refs/heads/main", RunID: 42}
	if !ok || got != want {
		t.Fatalf("got %+v %v", got, ok)
	}
	h.svc.Push(0, scalesettest.Job{Type: "JobCompleted", RepositoryName: "app", OwnerName: "example-org", RunnerName: "gh-pve1-small-9000"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	stop()
	if _, ok := h.l.Jobs.Of("gh-pve1-small-9000"); ok {
		t.Error("a finished job is still remembered")
	}
}

func TestJobTags(t *testing.T) {
	job := func(ref string) scaleset.Job {
		return scaleset.Job{OwnerName: "Example-Org", RepositoryName: "App", JobWorkflowRef: ref}
	}
	cases := []struct {
		ref  string
		want []string
	}{
		{"example-org/app/.github/workflows/ci.yml@refs/heads/main", []string{"repo-app", "branch-main"}},
		{"Example-Org/App/.github/workflows/ci.yml@refs/heads/feature/Login_v2", []string{"repo-app", "branch-feature-login_v2"}},
		{"example-org/app/.github/workflows/ci.yml@refs/pull/12/merge", []string{"repo-app", "pr-12"}},
		{"example-org/app/.github/workflows/release.yml@refs/tags/v1.2", []string{"repo-app", "tag-v1.2"}},
		// A reusable workflow from another repository names its own ref, not the job's.
		{"example-org/shared/.github/workflows/build.yml@refs/heads/main", []string{"repo-app"}},
		{"", []string{"repo-app"}},
	}
	for _, c := range cases {
		if got := JobTags(job(c.ref)); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.ref, got, c.want)
		}
	}
}

// A started job tags its runner's guest with the repository and branch and puts the details in its
// notes; a persistent runner's next job replaces them.
func TestStartedJobLabelsRunner(t *testing.T) {
	h := newHarness(t, pve.Guest{VMID: 9003, Type: "lxc", Name: "gh-pve1-small-9003", Node: "pve1", Status: "running",
		RawTags: "gh-runner;owner-example-org;repo-old;pr-7"})
	stop := h.start(t)
	h.svc.Push(0, scalesettest.Job{Type: "JobStarted", RepositoryName: "app", OwnerName: "example-org",
		RunnerName: "gh-pve1-small-9003", JobDisplayName: "unit (2, 4)",
		JobWorkflowRef: "example-org/app/.github/workflows/ci.yml@refs/heads/develop", WorkflowRunID: 42})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	stop()
	g, _ := h.pve.Guest(9003)
	if want := "gh-runner;owner-example-org;repo-app;branch-develop"; g.RawTags != want {
		t.Errorf("tags %q, want %q", g.RawTags, want)
	}
	want := "pct set 9003 --tags gh-runner;owner-example-org;repo-app;branch-develop --description " +
		"GitHub Actions runner managed by pve-runners\n\nRunning example-org/app, job \"unit (2, 4)\"\n" +
		"Workflow: example-org/app/.github/workflows/ci.yml@refs/heads/develop\n" +
		"Run: https://github.com/example-org/app/actions/runs/42"
	if !slices.Contains(h.pve.Mutations(), want) {
		t.Errorf("no labelling call in %q", h.pve.Mutations())
	}
}

// A job that became available before this session (its message went to the session of the process
// that restarted) is listed by acquirablejobs and claimed at start; a public repo's job is not.
func TestClaimsJobsWaitingFromBeforeTheSession(t *testing.T) {
	h := newHarness(t)
	h.public["example-org/site"] = true
	h.svc.Waiting = []scalesettest.Job{
		{Type: "JobAvailable", RunnerRequestID: 91, RepositoryName: "app", OwnerName: "example-org"},
		{Type: "JobAvailable", RunnerRequestID: 92, RepositoryName: "site", OwnerName: "example-org"},
	}
	h.start(t)
	eventually(t, "claim", func() bool { return len(h.svc.Acquired()) > 0 })
	if got := h.svc.Acquired(); !slices.Equal(got, []int64{91}) {
		t.Fatalf("acquired = %v", got)
	}
	if calls := h.svc.CallsTo("GET", "/svc/_apis/runtime/runnerscalesets/1/acquirablejobs"); len(calls) != 1 ||
		calls[0].Query != "api-version=6.0-preview" || calls[0].Auth == "Bearer "+scalesettest.QueueToken {
		t.Errorf("acquirablejobs calls = %+v", calls)
	}
}
