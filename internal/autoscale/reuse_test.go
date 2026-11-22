package autoscale

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset/scalesettest"
)

const dfCmd = "pct exec 9000 -- df --output=pcent /"

func reuseHarness(t *testing.T, diskUsed string) (*harness, *clock, *logBuf) {
	h := newHarness(t)
	h.l.Pool.ReuseFor = 30 * time.Minute
	h.pve.Outputs = map[string]string{dfCmd: "Use%\n " + diskUsed + "\n"}
	c := withClock(h)
	logs := withLog(h)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("log:\n%s\ncommands:\n%s", logs.String(), strings.Join(h.pve.Mutations(), "\n"))
		}
	})
	return h, c, logs
}

func job(typ string, id int64, repo, runner string) scalesettest.Job {
	return scalesettest.Job{Type: typ, RunnerRequestID: id, RepositoryName: repo, OwnerName: "example-org", RunnerName: runner}
}

func guestTags(h *harness, id int) string {
	g, _ := h.pve.Guest(id)
	return g.RawTags
}

// runOneJob runs a job of repo on a new runner 9000 and waits until it is kept after the job.
func runOneJob(t *testing.T, h *harness, repo string) {
	t.Helper()
	h.svc.Push(1, job("JobAvailable", 1, repo, ""))
	// Its spawn has finished (the runner service is started last), as before any real job.
	eventually(t, "runner", func() bool { return calls(h, "systemctl start gh-runner.service") == 1 })
	h.svc.Push(1, job("JobStarted", 1, repo, "gh-pve1-small-9000"))
	h.svc.Push(0, job("JobCompleted", 1, repo, "gh-pve1-small-9000"))
	eventually(t, "kept", func() bool { return strings.Contains(guestTags(h, 9000), "kept-") })
}

func calls(h *harness, sub string) int {
	n := 0
	for _, c := range h.pve.Mutations() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestRunnerIsKeptAndReusedForItsRepo(t *testing.T) {
	h, _, logs := reuseHarness(t, "42%")
	h.start(t)
	runOneJob(t, h, "app")
	tags := guestTags(h, 9000)
	if !strings.Contains(tags, pve.ReuseTag("example-org", "app")) {
		t.Errorf("kept runner lacks its repo tag: %s", tags)
	}
	if calls(h, "docker volume prune") != 1 {
		t.Error("the kept runner was not cleaned after its job")
	}
	time.Sleep(30 * time.Millisecond) // several polls: nothing removes it or starts another
	if len(h.runners(t)) != 1 || len(h.svc.JITNames()) != 1 {
		t.Fatalf("runners %d, registrations %v", len(h.runners(t)), h.svc.JITNames())
	}

	// The same repo's next job goes to the kept container: a new registration, no new clone.
	h.svc.Push(1, job("JobAvailable", 2, "App", ""))
	eventually(t, "re-registered", func() bool { return len(h.svc.JITNames()) == 2 })
	eventually(t, "armed", func() bool { return calls(h, "systemctl start gh-runner.service") == 2 })
	if names := h.svc.JITNames(); names[1] != "gh-pve1-small-9000" {
		t.Errorf("registrations %v", names)
	}
	if len(h.runners(t)) != 1 || calls(h, "pct clone") != 1 {
		t.Fatalf("a new runner was cloned: %v", h.pve.Mutations())
	}
	tags = guestTags(h, 9000)
	if strings.Contains(tags, "kept-") || !strings.Contains(tags, "armed-") || !strings.Contains(tags, "runnerid-") {
		t.Errorf("tags after arming: %s", tags)
	}
	h.svc.Push(1, job("JobStarted", 2, "App", "gh-pve1-small-9000"))
	eventually(t, "label drops armed", func() bool { return !strings.Contains(guestTags(h, 9000), "armed-") })
	if !strings.Contains(logs.String(), "runner reused") {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestOtherRepoGetsAFreshRunnerAndKeptOneStays(t *testing.T) {
	h, _, _ := reuseHarness(t, "42%")
	h.start(t)
	runOneJob(t, h, "app")
	h.svc.Push(1, job("JobAvailable", 2, "web", ""))
	eventually(t, "fresh runner", func() bool { return hasVMID(h, t, 9001) })
	time.Sleep(30 * time.Millisecond)
	if !strings.Contains(guestTags(h, 9000), "kept-") || calls(h, "systemctl start gh-runner.service") != 2 {
		t.Fatalf("the kept runner was used for another repo: %s %v", guestTags(h, 9000), h.pve.Mutations())
	}
}

func TestKeptRunnerGivesWayWhenThePoolIsFull(t *testing.T) {
	h, _, _ := reuseHarness(t, "42%")
	h.l.Pool.Max = 1
	h.start(t)
	runOneJob(t, h, "app")
	h.svc.Push(1, job("JobAvailable", 2, "web", ""))
	// The freed ID is the lowest free one, so the fresh runner is 9000 again.
	eventually(t, "fresh runner in its slot", func() bool { return calls(h, "pct destroy 9000") == 1 && calls(h, "pct clone") == 2 })
}

func TestFullDiskIsNotKept(t *testing.T) {
	h, _, logs := reuseHarness(t, "80%")
	h.start(t)
	h.svc.Push(1, job("JobAvailable", 1, "app", ""))
	eventually(t, "runner", func() bool { return calls(h, "systemctl start gh-runner.service") == 1 })
	h.svc.Push(1, job("JobStarted", 1, "app", "gh-pve1-small-9000"))
	h.svc.Push(0, job("JobCompleted", 1, "app", "gh-pve1-small-9000"))
	eventually(t, "destroyed", func() bool { return !hasVMID(h, t, 9000) })
	if !strings.Contains(logs.String(), "disk is 80% full") {
		t.Errorf("log:\n%s", logs.String())
	}
	h.svc.Push(1, job("JobAvailable", 2, "app", ""))
	eventually(t, "fresh runner", func() bool { return calls(h, "pct clone") == 2 })
}

func TestKeptRunnerGoesAfterReuseFor(t *testing.T) {
	h, c, _ := reuseHarness(t, "42%")
	h.start(t)
	runOneJob(t, h, "app")
	c.advance(31 * time.Minute)
	eventually(t, "destroyed", func() bool { return !hasVMID(h, t, 9000) })
}

// While a kept runner waits for its repo's job, another repo's jobs are not acquired: GitHub would hand
// one to whichever runner asks first, the kept one included.
func TestOtherReposJobsWaitWhileAKeptRunnerIsArmed(t *testing.T) {
	h, _, _ := reuseHarness(t, "42%")
	h.pve.SetRegistered(9000, false)
	h.start(t)
	runOneJob(t, h, "app")
	h.svc.Push(1, job("JobAvailable", 2, "app", ""))
	eventually(t, "armed", func() bool { return strings.Contains(guestTags(h, 9000), "armed-") })
	h.svc.Push(1, job("JobAvailable", 3, "web", ""))
	eventually(t, "message ack", func() bool { return h.svc.Pending() == 0 })
	time.Sleep(30 * time.Millisecond)
	if slices.Contains(h.svc.Acquired(), 3) {
		t.Fatalf("web's job was acquired while app's kept runner waited: %v", h.svc.Acquired())
	}
	h.svc.Push(1, job("JobStarted", 2, "app", "gh-pve1-small-9000"))
	eventually(t, "held-back job acquired", func() bool { return slices.Contains(h.svc.Acquired(), 3) })
}

// A runner that ran another repo's job than the one it was kept for (impossible by design, but a
// restart loses what was pending) is not kept again.
func TestRunnerOfAnotherRepoIsNotKept(t *testing.T) {
	h, _, _ := reuseHarness(t, "42%")
	h.start(t)
	runOneJob(t, h, "app")
	h.svc.Push(1, job("JobAvailable", 2, "app", ""))
	eventually(t, "armed", func() bool { return strings.Contains(guestTags(h, 9000), "armed-") })
	h.svc.Push(1, job("JobStarted", 2, "web", "gh-pve1-small-9000"))
	h.svc.Push(0, job("JobCompleted", 2, "web", "gh-pve1-small-9000"))
	eventually(t, "destroyed", func() bool { return !hasVMID(h, t, 9000) })
}

// GitHub assigns most jobs to the scale set itself (JobAssigned, no JobAvailable to acquire). In a
// workflow the repo's next job is assigned the moment the one before it ends, while that runner is
// still being cleaned: it waits for that runner instead of getting a fresh one.
func TestNextJobAssignedWhileRunnerIsCleanedReusesIt(t *testing.T) {
	h, _, _ := reuseHarness(t, "42%")
	release := make(chan struct{})
	var once sync.Once
	var cleaning atomic.Bool
	h.pve.Before = func(line string) {
		if strings.Contains(line, "docker volume prune") {
			cleaning.Store(true)
			<-release
		}
	}
	h.start(t)
	// After start's cleanup in the order cleanups run, so a failing test doesn't hang stopping it.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	h.svc.Push(1, job("JobAssigned", 1, "app", ""))
	eventually(t, "runner", func() bool { return calls(h, "systemctl start gh-runner.service") == 1 })
	h.svc.Push(1, job("JobStarted", 1, "app", "gh-pve1-small-9000"))
	h.svc.Push(1, job("JobCompleted", 1, "app", "gh-pve1-small-9000"), job("JobAssigned", 2, "app", ""))
	eventually(t, "cleaning", cleaning.Load)
	time.Sleep(30 * time.Millisecond) // several polls while it is cleaned
	if calls(h, "pct clone") != 1 {
		t.Fatalf("a fresh runner started while the repo's runner was cleaned: %v", h.pve.Mutations())
	}
	once.Do(func() { close(release) })
	eventually(t, "armed", func() bool { return calls(h, "systemctl start gh-runner.service") == 2 })
	if calls(h, "pct clone") != 1 || h.svc.JITNames()[1] != "gh-pve1-small-9000" {
		t.Fatalf("registrations %v, commands %v", h.svc.JITNames(), h.pve.Mutations())
	}
}
