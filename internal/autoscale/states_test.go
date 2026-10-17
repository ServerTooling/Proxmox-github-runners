package autoscale

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset/scalesettest"
)

// logBuf collects the listener's log lines for assertions.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withClock makes the listener and the provisioner (which writes the created tag) share a test clock.
func withClock(h *harness) *clock {
	c := &clock{}
	h.l.Now = c.now
	h.l.Prov.Now = c.now
	return c
}

func withLog(h *harness) *logBuf {
	b := &logBuf{}
	h.l.Log = slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return b
}

// queue is a fake REST view of queued jobs.
type queue struct {
	mu   sync.Mutex
	jobs []github.QueuedJob
}

func (q *queue) set(jobs ...github.QueuedJob) {
	q.mu.Lock()
	q.jobs = jobs
	q.mu.Unlock()
}

func (q *queue) list(context.Context) ([]github.QueuedJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]github.QueuedJob(nil), q.jobs...), nil
}

func withQueue(h *harness) *queue {
	q := &queue{}
	h.l.Queued = q.list
	h.l.ReconcileEvery = time.Millisecond
	return q
}

func hasVMID(h *harness, t *testing.T, id int) bool {
	for _, g := range h.runners(t) {
		if g.VMID == id {
			return true
		}
	}
	return false
}

// The incident of 2026-10-14: two jobs come at once; the first runner finishes its job and powers off
// while the second is still connecting. The finished one must not count, and the one still starting
// must not be taken for idle and removed.
func TestTwoJobsAtOnceNeitherStarves(t *testing.T) {
	h := newHarness(t)
	withClock(h)
	logs := withLog(h)
	h.pve.SetRegistered(9001, false) // the second runner is slow to connect
	h.start(t)
	h.svc.Push(2,
		scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"},
		scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 2, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "two runners", func() bool { return len(h.runners(t)) == 2 })

	// 9000 runs the first job and powers off; GitHub now counts only the second job.
	h.svc.Push(2, scalesettest.Job{Type: "JobStarted", RepositoryName: "app", OwnerName: "example-org", RunnerName: "gh-pve1-small-9000"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	h.pve.Stop(9000)
	h.svc.Push(1, scalesettest.Job{Type: "JobCompleted", RepositoryName: "app", OwnerName: "example-org",
		RunnerName: "gh-pve1-small-9000", Result: "succeeded"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	time.Sleep(50 * time.Millisecond) // several polls
	if !hasVMID(h, t, 9001) || len(h.svc.Removed()) != 0 {
		t.Fatalf("the runner for the second job was removed while it was still connecting: removed %v", h.svc.Removed())
	}

	// It connects and takes the job; nothing else starts for it.
	h.pve.SetRegistered(9001, true)
	h.svc.Push(1, scalesettest.Job{Type: "JobStarted", RepositoryName: "app", OwnerName: "example-org", RunnerName: "gh-pve1-small-9001"})
	eventually(t, "busy", func() bool { return strings.Contains(logs.String(), "vmid=9001 runner=gh-pve1-small-9001 state=busy") })
	if n := len(h.svc.JITNames()); n != 2 {
		t.Errorf("%d runners registered for two jobs", n)
	}
	for _, want := range []string{
		`vmid=9001 state=provisioning step=provisioning`, `vmid=9001 state=provisioning step="files pushed"`,
		`vmid=9001 state=provisioning step="service started"`, `vmid=9000 runner=gh-pve1-small-9000 state=busy`,
		`vmid=9000 runner=gh-pve1-small-9000 state=done`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// (a) The runner's service starts late (its config arrived late, or boot is slow): it registers a while
// after the spawn, inside the grace period, and is kept and used.
func TestLateRegistrationIsKept(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	h.pve.SetRegistered(9000, false)
	h.start(t)
	h.svc.Push(1, scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "runner", func() bool { return len(h.runners(t)) == 1 })
	// GitHub's count drops (as it did on 2026-10-14) while the runner is still starting.
	h.svc.Push(0)
	c.advance(2 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if !hasVMID(h, t, 9000) {
		t.Fatal("a runner still inside its start grace was removed")
	}
	h.pve.SetRegistered(9000, true)
	h.svc.Push(1)
	c.advance(5 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if !hasVMID(h, t, 9000) || len(h.svc.Removed()) != 0 {
		t.Fatalf("registered runner for an assigned job was removed: %v", h.svc.Removed())
	}
}

// (b) A runner that never registers is destroyed after the grace period and replaced while the job
// still waits; the replacement takes the job.
func TestRunnerThatNeverRegistersIsReplaced(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	logs := withLog(h)
	h.pve.SetRegistered(9000, false)
	h.start(t)
	h.svc.Push(1, scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "runner", func() bool { return len(h.runners(t)) == 1 })

	c.advance(4 * time.Minute)
	eventually(t, "the dead runner replaced", func() bool {
		return strings.Contains(logs.String(), "runner did not register") && len(h.svc.JITNames()) == 2
	})
	if got := h.svc.Removed(); len(got) != 1 || got[0] != 101 {
		t.Errorf("unregistered %v, want the dead runner 101", got)
	}
	if !strings.Contains(logs.String(), "level=WARN msg=\"runner did not register\"") ||
		!strings.Contains(logs.String(), "runners_failed_to_register_total=1") {
		t.Errorf("no WARN for the dead runner:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `reason="did not register within start grace"`) {
		t.Errorf("removal reason not logged:\n%s", logs.String())
	}
	// The replacement (it gets the freed ID) connects and is kept for the job.
	h.pve.SetRegistered(9000, true)
	c.advance(4 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 1 || len(h.svc.Removed()) != 1 {
		t.Fatalf("%d runners, removed %v; the replacement must stay for the job", n, h.svc.Removed())
	}
}

// (d) A job that ends without ever getting a runner (cancelled) doesn't take a runner away from
// another job that GitHub's REST API still shows queued.
func TestCancelledJobWithoutRunnerKeepsDemand(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	q := withQueue(h)
	h.start(t)
	h.svc.Push(2, scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"},
		scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 2, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "two runners", func() bool { return len(h.runners(t)) == 2 })
	q.set(github.QueuedJob{ID: 7, Name: "build", Labels: []string{"proxmox"}, CreatedAt: c.now(), Repo: "example-org/app"})
	c.advance(4 * time.Minute)
	// The other job is cancelled before any runner took it, and GitHub's statistics drop both.
	h.svc.Push(0, scalesettest.Job{Type: "JobCompleted", RepositoryName: "app", OwnerName: "example-org", Result: "canceled"})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	eventually(t, "one surplus runner removed", func() bool { return len(h.runners(t)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 1 {
		t.Fatalf("%d runners; the queued job still needs one", n)
	}
}

// (e) GitHub's statistics show no job, but the REST API shows one queued past the grace period with
// no runner for it: the reconcile starts one. A job younger than the grace, or for other labels,
// starts nothing.
func TestReconcileStartsRunnerForQueuedJob(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	logs := withLog(h)
	q := withQueue(h)
	q.set(github.QueuedJob{ID: 1, Name: "build", Labels: []string{"proxmox"}, CreatedAt: c.now(), Repo: "example-org/app"},
		github.QueuedJob{ID: 2, Name: "gpu", Labels: []string{"self-hosted", "gpu"}, CreatedAt: c.now().Add(-time.Hour), Repo: "example-org/app"})
	h.start(t)
	eventually(t, "session", func() bool { return h.svc.Sessions() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 0 {
		t.Fatalf("%d runners for a job still inside the grace period", n)
	}
	c.advance(6 * time.Minute)
	eventually(t, "a runner for the stale job", func() bool { return len(h.runners(t)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 1 {
		t.Fatalf("%d runners for one queued job", n)
	}
	for _, want := range []string{"reconcile: 1 queued, 0 runners, starting 1", `level=WARN msg="job queued too long"`,
		"queued_seconds=360", "msg=\"autoscale state\"", "queued_jobs=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestDemand(t *testing.T) {
	for _, c := range []struct{ assigned, busy, stale, want int }{
		{0, 0, 0, 0}, {2, 0, 0, 2}, {0, 0, 1, 1}, {1, 1, 1, 2}, {3, 1, 1, 3},
	} {
		st := scaleset.Statistics{TotalAssignedJobs: c.assigned}
		if got := Demand(st, c.busy, c.stale); got != c.want {
			t.Errorf("Demand(assigned %d, busy %d, stale %d) = %d, want %d", c.assigned, c.busy, c.stale, got, c.want)
		}
	}
}

// A job the REST view still lists as queued starts on the runner started for it: until the next
// REST read it must not count twice (once queued, once as its busy runner) and start a second runner.
func TestStartedJobLeavesTheQueuedView(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	q := withQueue(h)
	h.l.ReconcileEvery = time.Hour // keep the stale REST view
	q.set(github.QueuedJob{ID: 1, RunID: 42, Name: "build", Labels: []string{"proxmox"}, CreatedAt: c.now().Add(-time.Hour), Repo: "example-org/app"})
	h.start(t)
	eventually(t, "a runner for the stale job", func() bool { return len(h.runners(t)) == 1 })
	h.svc.Push(1, scalesettest.Job{Type: "JobStarted", RepositoryName: "app", OwnerName: "example-org",
		RunnerName: "gh-pve1-small-9000", JobDisplayName: "build", WorkflowRunID: 42})
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.runners(t)); n != 1 {
		t.Fatalf("%d runners for one job", n)
	}
}

// After a restart of the service a runner mid-job is seen as busy from its journal (its JobStarted
// came before the restart), so it is never offered for removal, and the state says so.
func TestRestartSeesBusyRunnerFromItsJournal(t *testing.T) {
	now := time.Now()
	h := newHarness(t, pve.Guest{VMID: 9003, Name: "gh-pve1-small-9003", Type: "lxc", Status: "running",
		RawTags: strings.Join(append(provision.Tags("small", "example-org", "ephemeral", 8000, now.Add(-time.Hour)),
			"scaleset-1", "runnerid-7"), ";")})
	logs := withLog(h)
	h.svc.AddRunner("gh-pve1-small-9003", 7)
	h.pve.RunningJob = map[int]bool{9003: true}
	h.start(t)
	eventually(t, "a few polls", func() bool { return len(h.svc.CallsTo("GET", "/queue/*")) > 3 })
	if len(h.svc.Removed()) != 0 || len(h.runners(t)) != 1 {
		t.Fatalf("removed %v; the runner is running a job", h.svc.Removed())
	}
	if !strings.Contains(logs.String(), "vmid=9003 runner=gh-pve1-small-9003 state=busy") {
		t.Errorf("not logged busy:\n%s", logs.String())
	}
}

// Two jobs at once start their runners together: the second runner's clone doesn't wait for the
// first runner to boot (on 2026-10-14 the second job waited a whole extra spawn).
func TestTwoJobsSpawnTogether(t *testing.T) {
	h := newHarness(t)
	cloned := make(chan struct{})
	var once sync.Once
	h.pve.Before = func(line string) {
		switch {
		case strings.HasPrefix(line, "pct clone 8000 9001 "):
			once.Do(func() { close(cloned) })
		case line == "pct start 9000":
			select {
			case <-cloned:
			case <-time.After(5 * time.Second):
				t.Error("the second runner's spawn waited for the first runner to start")
			}
		}
	}
	h.start(t)
	h.svc.Push(2,
		scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 1, RepositoryName: "app", OwnerName: "example-org"},
		scalesettest.Job{Type: "JobAssigned", RunnerRequestID: 2, RepositoryName: "app", OwnerName: "example-org"})
	eventually(t, "two runners", func() bool { return len(h.runners(t)) == 2 })
	if n := len(h.svc.JITNames()); n != 2 {
		t.Errorf("%d runners registered for two jobs", n)
	}
}

// The incident of 2026-10-17: the jobs stay queued past the grace while registered runners sit idle
// beside them, because the message session lost them. The listener claims the waiting jobs again,
// and when there are none to claim it opens a new session.
func TestLostJobsAreClaimedAgain(t *testing.T) {
	h := newHarness(t)
	c := withClock(h)
	logs := withLog(h)
	q := withQueue(h)
	q.set(github.QueuedJob{ID: 1, Name: "build", Labels: []string{"proxmox"}, CreatedAt: c.now().Add(-10 * time.Minute),
		Repo: "example-org/app"})
	h.start(t)
	eventually(t, "an idle registered runner", func() bool { return strings.Contains(logs.String(), "state=registered") })
	time.Sleep(20 * time.Millisecond)
	if len(h.svc.Acquired()) != 0 {
		t.Fatalf("claimed before the jobs had waited beside an idle runner: %v", h.svc.Acquired())
	}

	h.svc.SetWaiting(scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 77, RepositoryName: "app", OwnerName: "example-org"})
	c.advance(unclaimedAfter + time.Second)
	eventually(t, "the job claimed again", func() bool { return slices.Equal(h.svc.Acquired(), []int64{77}) })
	sessions := "/svc/_apis/runtime/runnerscalesets/1/sessions"
	time.Sleep(20 * time.Millisecond)
	if n := len(h.svc.CallsTo("POST", sessions)); n != 1 {
		t.Fatalf("%d sessions opened although the claim worked", n)
	}

	// Still queued, and now nothing is claimable: the session is replaced, at most every reclaimEvery.
	h.svc.SetWaiting()
	c.advance(time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := len(h.svc.CallsTo("POST", sessions)); n != 1 {
		t.Fatalf("session replaced %d times within reclaimEvery", n-1)
	}
	c.advance(reclaimEvery)
	eventually(t, "a new session", func() bool { return len(h.svc.CallsTo("POST", sessions)) == 2 })
	eventually(t, "the old session closed", func() bool { return h.svc.Sessions() == 1 })
	if !strings.Contains(logs.String(), "no job could be claimed; opening a new message session") {
		t.Errorf("log lacks the renewal:\n%s", logs.String())
	}
}
