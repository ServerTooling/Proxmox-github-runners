// Package autoscale runs one listener per scale set (a target's pool, or a pool and repo for personal
// accounts). Each listener holds GitHub's message session for its scale set, claims queued jobs from
// private repositories, and keeps as many runners alive as there are jobs: the jobs GitHub's
// statistics say are assigned, or, when GitHub's REST API shows more jobs queued past the start grace
// than runners exist for them, that many (the statistics have been seen to drop a job that was still
// queued).
//
// Each runner goes provisioning (the guest is built and its service started) → registered (its
// runner is listening on GitHub) → busy (running a job) → done (stopped; reconcile destroys it).
// Only registered runners without a job are ever removed as surplus. A runner that hasn't registered
// within autoscale.start_grace is destroyed and, while the jobs are still there, replaced.
//
// Runner guests carry a scaleset-<id> tag, so a restarted listener counts the runners it already
// has instead of starting duplicates. GitHub refuses to unregister a runner that has a job, so a
// busy runner is never taken away.
package autoscale

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
)

// Service is the slice of the scale set client a listener needs.
type Service interface {
	EnsureScaleSet(ctx context.Context, name string, groupID int64, labels []string) (*scaleset.ScaleSet, error)
	CreateSession(ctx context.Context, scaleSetID int64, owner string) (*scaleset.Session, error)
	GenerateJIT(ctx context.Context, scaleSetID int64, name, workFolder string) (*scaleset.JITConfig, error)
	RemoveRunner(ctx context.Context, id int64) error
}

type Listener struct {
	Service  Service
	Target   config.Target
	Repo     string // personal-account targets: the repository this scale set belongs to
	PoolName string
	Pool     *config.Pool
	// GroupID is the runner group in the Actions service (scaleset.DefaultGroupID for repos). When
	// Group is set it is resolved on every start instead, so checks such as "this group refuses
	// public repositories" run again after each restart.
	GroupID int64
	Group   func(ctx context.Context) (int64, error)
	// HostID names this host as the session owner.
	HostID string
	// HostMax is proxmox.max_runners; GitHub is told the smaller of it and the pool's max.
	HostMax int
	Prov    *provision.Provisioner
	Host    *pve.Host
	// Private reports whether a repository may be served. Public ones are never acquired.
	Private func(ctx context.Context, owner, repo string) (bool, error)
	// Failed is told about spawn failures (the notify webhook); repeats of the same error are dropped.
	Failed func(ctx context.Context, msg string)
	Log    *slog.Logger
	Now    func() time.Time
	// Jobs, when set, is told which runner runs which job (usage metrics label samples with it).
	Jobs *Jobs
	// ConflictWait is how long to wait before retrying when another listener holds the session.
	ConflictWait time.Duration
	// StartGrace is autoscale.start_grace: how long a new runner may take to register (0 = 3m).
	StartGrace time.Duration
	// Queued, when set, lists the target's queued jobs from GitHub's REST API, read every
	// ReconcileEvery (0 = 45s) to correct statistics that lost a job.
	Queued         func(ctx context.Context) ([]github.QueuedJob, error)
	ReconcileEvery time.Duration

	scaleSetID int64
	lastFail   string
	// lastJob is when this scale set last had a job; warm spares are kept for pool.warm_for after it.
	lastJob time.Time
	privMu  sync.Mutex
	private map[string]privEntry

	// Runner states, touched only by the listener's own goroutine, except state, which concurrent
	// spawns also write through step (under stepMu) while that goroutine waits for them.
	stepMu     sync.Mutex
	state      map[int]string  // vmid -> last logged state
	registered map[int]bool    // vmid -> its runner has connected to GitHub (never goes back)
	busy       map[string]bool // runner name -> has a job (JobStarted until JobCompleted)
	// failedToRegister counts runners destroyed for not registering within StartGrace.
	failedToRegister int
	lastReconcile    time.Time
	queued           []github.QueuedJob // the last REST view, only jobs for this scale set
	warned           map[int64]time.Time
	// labels tracks runner labelling still in flight (pct set runs beside the message loop).
	labels sync.WaitGroup
}

// Runner states, as logged in "runner state" lines.
const (
	StateProvisioning = "provisioning"
	StateRegistered   = "registered"
	StateBusy         = "busy"
	StateDone         = "done"
)

// queuedWarnAfter is how long a job may wait for a runner before every reconcile warns about it.
const queuedWarnAfter = 5 * time.Minute

type privEntry struct {
	ok  bool
	exp time.Time
}

// Name describes the listener in logs.
func (l *Listener) Name() string {
	s := l.Target.Owner()
	if l.Repo != "" {
		s += "/" + l.Repo
	}
	return s + " " + l.Pool.ScaleSetName()
}

func (l *Listener) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Listener) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// ScaleSetTag marks runners registered in a scale set (IDs are only unique per org, so the owner
// tag is checked too).
func ScaleSetTag(id int64) string { return "scaleset-" + strconv.FormatInt(id, 10) }

// Run serves the scale set until ctx ends or the session fails; the caller restarts it with backoff.
func (l *Listener) Run(ctx context.Context) error {
	defer l.labels.Wait()
	err := l.run(ctx)
	if ctx.Err() != nil {
		// Shutting down mid-request (opening a session, handling a message) is not a failure.
		return nil
	}
	return err
}

func (l *Listener) run(ctx context.Context) error {
	if l.Group != nil {
		id, err := l.Group(ctx)
		if err != nil {
			return err
		}
		l.GroupID = id
	}
	ss, err := l.Service.EnsureScaleSet(ctx, l.Pool.ScaleSetName(), l.GroupID, l.Pool.Labels)
	if err != nil {
		return fmt.Errorf("scale set %s: %w", l.Pool.ScaleSetName(), err)
	}
	l.scaleSetID = ss.ID

	sess, err := l.openSession(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Deleting the session lets the next start (or another host) take over without waiting
		// for GitHub to expire it, so it gets its own context when ours is already done.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sess.Close(cctx); err != nil {
			l.log().Warn("close message session", "listener", l.Name(), "err", err)
		}
	}()
	l.log().Info("listening for jobs", "listener", l.Name(), "scale_set_id", ss.ID)
	// After a restart (a self-update, a saved setting) runners still up mean jobs came recently, so
	// warm spares get a fresh window instead of being dropped at once.
	if f, err := l.survey(ctx); err == nil && f.live > 0 {
		l.lastJob = l.now()
	}

	stats := sess.Statistics()
	l.scale(ctx, stats)
	var lastID int64
	for {
		msg, err := sess.GetMessage(ctx, lastID, l.capacity())
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
		if msg != nil {
			lastID = msg.ID
			if err := l.handle(ctx, sess, msg); err != nil {
				return err
			}
			if msg.Statistics != nil {
				stats = *msg.Statistics
			}
		}
		// Also on empty polls: replaces runners whose spawn failed or that ended without a job.
		l.scale(ctx, stats)
	}
}

func (l *Listener) openSession(ctx context.Context) (*scaleset.Session, error) {
	wait := l.ConflictWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	for attempt := 1; ; attempt++ {
		sess, err := l.Service.CreateSession(ctx, l.scaleSetID, l.HostID)
		if !errors.Is(err, scaleset.ErrSessionConflict) || attempt >= 10 {
			if errors.Is(err, scaleset.ErrSessionConflict) {
				return nil, fmt.Errorf("%w; is another host or pve-runners process serving scale set %q? Give each host its own scale_set",
					err, l.Pool.ScaleSetName())
			}
			return sess, err
		}
		// A previous run that died without closing its session holds it until GitHub expires it.
		l.log().Info("message session busy, retrying", "listener", l.Name(), "attempt", attempt)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (l *Listener) capacity() int {
	if l.HostMax > 0 && l.HostMax < l.Pool.Max {
		return l.HostMax
	}
	return l.Pool.Max
}

// handle claims the message's available jobs from repositories this target serves, then
// acknowledges the message.
func (l *Listener) handle(ctx context.Context, sess *scaleset.Session, msg *scaleset.Message) error {
	jobs, err := msg.Jobs()
	if err != nil {
		l.log().Warn("skipping message", "listener", l.Name(), "err", err)
	}
	var claim []int64
	for _, j := range jobs {
		l.Jobs.record(j)
		switch j.Type {
		case "JobAvailable":
			if l.serves(ctx, j) {
				claim = append(claim, j.RunnerRequestID)
				l.lastJob = l.now()
			}
		case "JobAssigned":
			l.lastJob = l.now()
		case "JobStarted":
			l.lastJob = l.now()
			l.setBusy(j.RunnerName, true)
			l.dropQueued(j)
			l.labels.Add(1)
			go func() {
				defer l.labels.Done()
				l.label(ctx, j)
			}()
			l.log().Info("job started", "listener", l.Name(), "repo", j.OwnerName+"/"+j.RepositoryName, "runner", j.RunnerName,
				"job", j.JobDisplayName, "run_id", j.WorkflowRunID)
		case "JobCompleted":
			if j.RunnerName == "" {
				// The job ended (cancelled, usually) before any runner took it. It says nothing about the
				// other jobs or about any runner, so nothing changes here.
				l.log().Info("job completed without a runner", "listener", l.Name(), "repo", j.OwnerName+"/"+j.RepositoryName,
					"job", j.JobDisplayName, "run_id", j.WorkflowRunID, "result", j.Result)
				continue
			}
			l.setBusy(j.RunnerName, false)
			l.log().Info("job completed", "listener", l.Name(), "repo", j.OwnerName+"/"+j.RepositoryName,
				"runner", j.RunnerName, "job", j.JobDisplayName, "run_id", j.WorkflowRunID, "result", j.Result)
		}
	}
	if len(claim) > 0 {
		got, err := sess.AcquireJobs(ctx, claim)
		if err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
		l.log().Info("acquired jobs", "listener", l.Name(), "requested", len(claim), "acquired", len(got))
	}
	if err := sess.DeleteMessage(ctx, msg.ID); err != nil {
		return fmt.Errorf("delete message %d: %w", msg.ID, err)
	}
	return nil
}

// serves is the autoscaler's own public-repository check. The runner group already refuses public
// repos; this catches a group whose setting is changed later, and honors the target's repos list.
func (l *Listener) serves(ctx context.Context, j scaleset.Job) bool {
	repo := j.OwnerName + "/" + j.RepositoryName
	if !strings.EqualFold(j.OwnerName, l.Target.Owner()) {
		l.log().Warn("ignoring job from another owner", "listener", l.Name(), "repo", repo)
		return false
	}
	if len(l.Target.Repos) > 0 && !slices.ContainsFunc(l.Target.Repos, func(r string) bool { return strings.EqualFold(r, j.RepositoryName) }) {
		l.log().Info("ignoring job from a repo not in this target's repos", "listener", l.Name(), "repo", repo)
		return false
	}
	ok, err := l.isPrivate(ctx, j.OwnerName, j.RepositoryName)
	if err != nil {
		l.log().Error("repository visibility check failed; job not taken", "listener", l.Name(), "repo", repo, "err", err)
		return false
	}
	if !ok {
		l.log().Warn("refusing job from a public repository", "listener", l.Name(), "repo", repo)
	}
	return ok
}

func (l *Listener) isPrivate(ctx context.Context, owner, repo string) (bool, error) {
	key := strings.ToLower(owner + "/" + repo)
	l.privMu.Lock()
	e, hit := l.private[key]
	l.privMu.Unlock()
	if hit && l.now().Before(e.exp) {
		return e.ok, nil
	}
	ok, err := l.Private(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	l.privMu.Lock()
	if l.private == nil {
		l.private = map[string]privEntry{}
	}
	// Visibility rarely changes; a short cache keeps a busy repo from costing an API call per job.
	l.private[key] = privEntry{ok: ok, exp: l.now().Add(10 * time.Minute)}
	l.privMu.Unlock()
	return ok, nil
}

// Desired is the runner count for jobs that need one plus the idle ones the pool keeps ready (min,
// and warm spares after a job), capped at max.
func Desired(jobs, idle, max int) int {
	return Clamp(idle+jobs, max)
}

// Demand is how many jobs need a runner: what GitHub's statistics say is assigned to the scale set,
// or the runners busy with a job plus the jobs the REST API shows queued past the start grace,
// whichever is more. The statistics alone have dropped a job that stayed queued.
func Demand(stats scaleset.Statistics, busy, staleQueued int) int {
	return max(stats.TotalAssignedJobs, busy+staleQueued)
}

// idle is how many spare runners to keep: always min, plus warm until warm_for has passed with no job.
func (l *Listener) idle() int {
	n := l.Pool.Min
	if l.Pool.Warm > 0 && !l.lastJob.IsZero() && l.now().Sub(l.lastJob) < l.Pool.WarmFor {
		n += l.Pool.Warm
	}
	return n
}

func Clamp(n, max int) int {
	if n > max {
		return max
	}
	return n
}

func (l *Listener) startGrace() time.Duration {
	if l.StartGrace > 0 {
		return l.StartGrace
	}
	return 3 * time.Minute
}

// fleet is this scale set's runners as one survey saw them.
type fleet struct {
	live         int // running: provisioning, registered or busy
	provisioning int
	busy         int
	// spare are registered runners without a job that could be removed, newest first.
	spare []pve.Guest
}

func (l *Listener) setBusy(runner string, busy bool) {
	if runner == "" {
		return
	}
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	if busy {
		l.busy[runner] = true
	} else {
		delete(l.busy, runner)
	}
}

// setState logs a runner's state when it changes.
func (l *Listener) setState(vmid int, runner, state string, attrs ...any) {
	if l.state == nil {
		l.state = map[int]string{}
	}
	if l.state[vmid] == state {
		return
	}
	l.state[vmid] = state
	l.log().Info("runner state", append([]any{"listener", l.Name(), "vmid", vmid, "runner", runner, "state", state}, attrs...)...)
}

// step logs a start step of a runner still provisioning (provision.Request.Step).
func (l *Listener) step(vmid int, step string) {
	l.stepMu.Lock()
	defer l.stepMu.Unlock()
	if l.state == nil {
		l.state = map[int]string{}
	}
	l.state[vmid] = StateProvisioning
	l.log().Info("runner state", "listener", l.Name(), "vmid", vmid, "state", StateProvisioning, "step", step)
}

// survey reads this scale set's runners and puts each in its state. A stopped one has finished
// (scale waits for its spawns, so none of ours is mid-start while this runs) and waits for
// reconcile to destroy it. A running one that hasn't registered within the start grace is destroyed
// here, so the caller's count goes down and it starts a replacement while the jobs are still there.
func (l *Listener) survey(ctx context.Context) (fleet, error) {
	var f fleet
	runners, err := l.Host.Runners(ctx)
	if err != nil {
		return f, err
	}
	tag, owner := ScaleSetTag(l.scaleSetID), provision.OwnerTag(l.Target.Owner())
	if l.registered == nil {
		l.registered = map[int]bool{}
	}
	seen, names := map[int]bool{}, map[string]bool{}
	for _, g := range runners {
		if !g.HasTag(tag) || !g.HasTag(owner) {
			continue
		}
		seen[g.VMID], names[g.Name] = true, true
		if g.Status != "running" {
			l.setState(g.VMID, g.Name, StateDone)
			continue
		}
		if l.busy[g.Name] {
			l.registered[g.VMID] = true
			l.setState(g.VMID, g.Name, StateBusy)
			f.live++
			f.busy++
			continue
		}
		if !l.registered[g.VMID] {
			phase, err := l.Host.RunnerPhase(ctx, g.VMID)
			if err != nil {
				// Can't tell (the guest is still booting, or stopping): it counts as not registered yet.
				l.log().Warn("check runner registration", "listener", l.Name(), "vmid", g.VMID, "err", err)
			}
			l.registered[g.VMID] = phase != pve.PhaseStarting
			if phase == pve.PhaseBusy {
				// Mid-job across a restart of this service: the JobStarted message came before it.
				l.setBusy(g.Name, true)
				l.setState(g.VMID, g.Name, StateBusy)
				f.live++
				f.busy++
				continue
			}
		}
		if l.registered[g.VMID] {
			l.setState(g.VMID, g.Name, StateRegistered)
			f.live++
			// Runners from before the runnerid tag can't be unregistered by ID; max_lifetime still caps them.
			if g.TagValue("runnerid") != "" {
				f.spare = append(f.spare, g)
			}
			continue
		}
		if age := l.now().Sub(time.Unix(createdAt(g), 0)); age < l.startGrace() {
			l.setState(g.VMID, g.Name, StateProvisioning)
			f.live++
			f.provisioning++
			continue
		}
		unit, _ := l.Host.RunnerUnitState(ctx, g.VMID)
		l.failedToRegister++
		l.log().Warn("runner did not register", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name,
			"grace", l.startGrace(), "unit", unit, "runners_failed_to_register_total", l.failedToRegister)
		if !l.remove(ctx, g, "did not register within start grace", true) {
			// GitHub says it has a job after all, so it did register: keep it.
			l.registered[g.VMID] = true
			l.setState(g.VMID, g.Name, StateBusy)
			f.live++
			f.busy++
		}
	}
	for name := range l.busy {
		if !names[name] {
			delete(l.busy, name)
		}
	}
	for id := range l.state {
		if !seen[id] {
			delete(l.state, id)
			delete(l.registered, id)
		}
	}
	slices.SortFunc(f.spare, func(a, b pve.Guest) int { return cmp.Compare(createdAt(b), createdAt(a)) })
	return f, nil
}

func createdAt(g pve.Guest) int64 {
	n, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
	return n
}

// remove unregisters a runner that has no job and destroys its guest. It reports false, and leaves
// the guest alone, when GitHub says the runner has a job, or (unless force) when unregistering failed:
// an idle runner GitHub can't be told about might still be handed a job.
func (l *Listener) remove(ctx context.Context, g pve.Guest, reason string, force bool) bool {
	if id, _ := strconv.ParseInt(g.TagValue("runnerid"), 10, 64); id != 0 {
		if err := l.Service.RemoveRunner(ctx, id); err != nil {
			if errors.Is(err, scaleset.ErrRunnerBusy) {
				return false
			}
			// Already gone (its job just ended) or GitHub unreachable: reconcile or the next poll
			// takes care of it. A runner that never connected has no job to lose, so it goes anyway.
			l.log().Warn("unregister runner", "listener", l.Name(), "vmid", g.VMID, "err", err)
			if !force {
				return false
			}
		}
	}
	if err := l.Host.Destroy(ctx, g.VMID); err != nil {
		// Unregistered, so it can't take a job; its runner exits and reconcile destroys it.
		l.log().Warn("destroy runner", "listener", l.Name(), "vmid", g.VMID, "err", err)
		return true
	}
	l.setState(g.VMID, g.Name, "removed", "reason", reason)
	return true
}

// reconcile reads the queued jobs from GitHub's REST API every ReconcileEvery and returns how many
// have waited past the start grace, warning about each one waiting longer than queuedWarnAfter.
func (l *Listener) reconcile(ctx context.Context) int {
	every := l.ReconcileEvery
	if every == 0 {
		every = 45 * time.Second
	}
	if l.Queued != nil && l.now().Sub(l.lastReconcile) >= every {
		l.lastReconcile = l.now()
		jobs, err := l.Queued(ctx)
		if err != nil {
			l.log().Warn("reconcile: list queued jobs", "listener", l.Name(), "err", err)
		} else {
			l.queued = l.queued[:0]
			for _, j := range jobs {
				// Scale sets take the jobs whose runs-on is exactly the scale set's name.
				if len(j.Labels) == 1 && strings.EqualFold(j.Labels[0], l.Pool.ScaleSetName()) {
					l.queued = append(l.queued, j)
				}
			}
			l.warnQueued()
		}
	}
	stale := 0
	for _, j := range l.queued {
		if l.now().Sub(j.CreatedAt) >= l.startGrace() {
			stale++
		}
	}
	return stale
}

// dropQueued takes a job that just started off the last REST view, which is up to ReconcileEvery
// old: until the next read it would count twice, once as queued and once as its busy runner.
func (l *Listener) dropQueued(j scaleset.Job) {
	i := slices.IndexFunc(l.queued, func(q github.QueuedJob) bool { return q.RunID == j.WorkflowRunID && q.Name == j.JobDisplayName })
	if i < 0 {
		i = slices.IndexFunc(l.queued, func(q github.QueuedJob) bool { return q.RunID == j.WorkflowRunID })
	}
	if i >= 0 {
		l.queued = slices.Delete(l.queued, i, i+1)
	}
}

func (l *Listener) warnQueued() {
	if l.warned == nil {
		l.warned = map[int64]time.Time{}
	}
	keep := map[int64]time.Time{}
	for _, j := range l.queued {
		wait := l.now().Sub(j.CreatedAt)
		last, ok := l.warned[j.ID]
		if ok {
			keep[j.ID] = last
		}
		if wait >= queuedWarnAfter && (!ok || l.now().Sub(last) >= queuedWarnAfter) {
			l.log().Warn("job queued too long", "listener", l.Name(), "repo", j.Repo, "job", j.Name, "job_id", j.ID,
				"run_id", j.RunID, "queued_seconds", int(wait.Seconds()))
			keep[j.ID] = l.now()
		}
	}
	l.warned = keep
}

func (l *Listener) scale(ctx context.Context, stats scaleset.Statistics) {
	tick := l.Queued != nil && l.now().Sub(l.lastReconcile) >= cmp.Or(l.ReconcileEvery, 45*time.Second)
	stale := l.reconcile(ctx)
	f, err := l.survey(ctx)
	if err != nil {
		l.log().Error("count runners", "listener", l.Name(), "err", err)
		return
	}
	jobs := Demand(stats, f.busy, stale)
	want := Desired(jobs, l.idle(), l.capacity())
	have := f.live
	if tick {
		l.logState(f, stats, stale, want)
	}
	if have > want {
		l.trim(ctx, f.spare, have-want)
		return
	}
	if have >= want {
		return
	}
	// All at once: the provisioner takes them one clone at a time, and each one's start and boot
	// overlap the next one's clone. This goroutine waits for all of them, so survey never sees one
	// of ours mid-start.
	type spawned struct {
		res *provision.Result
		err error
	}
	out := make([]spawned, want-have)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].res, out[i].err = l.Prov.Spawn(ctx, provision.Request{Pool: l.PoolName, Target: l.Target, Repo: l.Repo,
				JIT: l.jit, Tags: []string{ScaleSetTag(l.scaleSetID)},
				Step: l.step})
		}()
	}
	wg.Wait()
	for _, o := range out {
		switch err := o.err; {
		case errors.Is(err, provision.ErrAtCapacity):
			// The host or pool is full; GitHub keeps the job queued until a runner frees up.
			l.log().Info("at capacity", "listener", l.Name(), "want", want, "have", have, "reason", err)
		case errors.Is(err, provision.ErrDraining):
			// the service restarts for an update and spawns again after
		case err != nil:
			if ctx.Err() != nil {
				continue
			}
			l.log().Error("spawn failed", "listener", l.Name(), "err", err)
			if msg := err.Error(); msg != l.lastFail && l.Failed != nil {
				l.Failed(ctx, l.Name()+": "+msg)
			}
			l.lastFail = err.Error()
		default:
			l.lastFail = ""
			have++
			l.log().Info("runner started", "listener", l.Name(), "vmid", o.res.VMID, "runner", o.res.RunnerName)
		}
	}
}

// logState writes the reconcile line and the gauges Grafana reads (one line per reconcile).
func (l *Listener) logState(f fleet, stats scaleset.Statistics, stale, want int) {
	oldest := 0
	for _, j := range l.queued {
		oldest = max(oldest, int(l.now().Sub(j.CreatedAt).Seconds()))
	}
	start := max(want-f.live, 0)
	attrs := []any{"listener", l.Name(), "queued", len(l.queued), "runners", f.live, "starting", start,
		"assigned", stats.TotalAssignedJobs, "queued_past_grace", stale}
	if f.busy+stale > stats.TotalAssignedJobs && start > 0 {
		// GitHub's statistics count fewer jobs than are waiting: the REST view is what starts runners.
		l.log().Warn(fmt.Sprintf("reconcile: %d queued, %d runners, starting %d", len(l.queued), f.live, start), attrs...)
	} else if len(l.queued) > 0 || start > 0 {
		l.log().Info(fmt.Sprintf("reconcile: %d queued, %d runners, starting %d", len(l.queued), f.live, start), attrs...)
	}
	l.log().Info("autoscale state", "listener", l.Name(), "queued_jobs", len(l.queued), "oldest_queued_seconds", oldest,
		"runners_provisioning", f.provisioning, "runners_registered", f.live-f.provisioning-f.busy,
		"runners_busy", f.busy, "runners_failed_to_register_total", l.failedToRegister)
}

// trim removes up to n registered runners without a job. Each is unregistered first, so GitHub can't
// hand it a job while its container goes away; one GitHub says has a job is skipped. Runners still
// provisioning are never in spare, so a runner is never taken away before it could take its job.
func (l *Listener) trim(ctx context.Context, spare []pve.Guest, n int) {
	for _, g := range spare {
		if n == 0 || ctx.Err() != nil {
			return
		}
		if l.remove(ctx, g, "idle", false) {
			l.log().Info("removed idle runner", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name)
			n--
		}
	}
}

func (l *Listener) jit(ctx context.Context, name string) (*github.JITConfig, error) {
	j, err := l.Service.GenerateJIT(ctx, l.scaleSetID, name, "_work")
	if err != nil {
		return nil, err
	}
	out := &github.JITConfig{EncodedJITConfig: j.EncodedJITConfig}
	out.Runner.ID, out.Runner.Name = j.Runner.ID, j.Runner.Name
	return out, nil
}

// Supervise runs the listener until ctx ends, restarting it after failures with exponential backoff
// (reset once a run has lasted a while, so a long-lived session that drops restarts quickly).
func Supervise(ctx context.Context, l *Listener, minWait, maxWait time.Duration) {
	wait := minWait
	for ctx.Err() == nil {
		start := l.now()
		err := l.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if l.now().Sub(start) > 5*maxWait {
			wait = minWait
		}
		l.log().Error("listener stopped; restarting", "listener", l.Name(), "err", err, "in", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, maxWait)
	}
}
