// Package autoscale runs one listener per scale set (a target's pool, or a pool and repo for personal
// accounts). Each listener holds GitHub's message session for its scale set, claims queued jobs from
// private repositories, and keeps as many runners alive as GitHub says jobs are assigned.
//
// Runner guests carry a scaleset-<id> tag, so a restarted listener counts the runners it already
// has instead of starting duplicates. Runners beyond what is wanted (a cancelled job, or warm spares
// whose time is up) are unregistered and destroyed; GitHub refuses to unregister one that has a job,
// so a busy runner is never taken away.
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
	"github.com/ServerTooling/proxmox-github-runners/internal/reconcile"
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
	// ConflictWait is how long to wait before retrying when another listener holds the session.
	ConflictWait time.Duration

	scaleSetID int64
	lastFail   string
	// lastJob is when this scale set last had a job; warm spares are kept for pool.warm_for after it.
	lastJob time.Time
	privMu  sync.Mutex
	private map[string]privEntry
}

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
	if n, _, err := l.live(ctx); err == nil && n > 0 {
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
			l.log().Info("job started", "listener", l.Name(), "repo", j.OwnerName+"/"+j.RepositoryName, "runner", j.RunnerName)
		case "JobCompleted":
			l.log().Info("job completed", "listener", l.Name(), "repo", j.OwnerName+"/"+j.RepositoryName,
				"runner", j.RunnerName, "result", j.Result)
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

// Desired is the runner count GitHub's statistics call for: every assigned job gets a runner, plus
// the idle ones the pool keeps ready (min, and warm spares after a job), capped at max.
func Desired(stats scaleset.Statistics, idle, max int) int {
	return Clamp(idle+stats.TotalAssignedJobs, max)
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

// live counts this scale set's runners that are still running a job or about to. A stopped ephemeral
// runner past the start grace period has finished and is waiting for reconcile to destroy it. It also
// returns the running ones that could be removed if there are too many, newest first.
func (l *Listener) live(ctx context.Context) (int, []pve.Guest, error) {
	runners, err := l.Host.Runners(ctx)
	if err != nil {
		return 0, nil, err
	}
	tag, owner := ScaleSetTag(l.scaleSetID), provision.OwnerTag(l.Target.Owner())
	n := 0
	var spare []pve.Guest
	for _, g := range runners {
		if !g.HasTag(tag) || !g.HasTag(owner) {
			continue
		}
		created, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
		if g.Status != "stopped" || l.now().Sub(time.Unix(created, 0)) < reconcile.Grace {
			n++
		}
		// Runners from before the runnerid tag can't be unregistered by ID; max_lifetime still caps them.
		if g.Status == "running" && g.TagValue("runnerid") != "" {
			spare = append(spare, g)
		}
	}
	slices.SortFunc(spare, func(a, b pve.Guest) int { return cmp.Compare(createdAt(b), createdAt(a)) })
	return n, spare, nil
}

func createdAt(g pve.Guest) int64 {
	n, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
	return n
}

func (l *Listener) scale(ctx context.Context, stats scaleset.Statistics) {
	want := Desired(stats, l.idle(), l.capacity())
	have, spare, err := l.live(ctx)
	if err != nil {
		l.log().Error("count runners", "listener", l.Name(), "err", err)
		return
	}
	if have > want {
		l.trim(ctx, spare, have-want)
		return
	}
	for ; have < want; have++ {
		res, err := l.Prov.Spawn(ctx, provision.Request{Pool: l.PoolName, Target: l.Target, Repo: l.Repo,
			JIT: l.jit, Tags: []string{ScaleSetTag(l.scaleSetID)}})
		if errors.Is(err, provision.ErrAtCapacity) {
			// The host or pool is full; GitHub keeps the job queued until a runner frees up.
			l.log().Info("at capacity", "listener", l.Name(), "want", want, "have", have, "reason", err)
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.log().Error("spawn failed", "listener", l.Name(), "err", err)
			if msg := err.Error(); msg != l.lastFail && l.Failed != nil {
				l.Failed(ctx, l.Name()+": "+msg)
			}
			l.lastFail = err.Error()
			return
		}
		l.lastFail = ""
		l.log().Info("runner started", "listener", l.Name(), "vmid", res.VMID, "runner", res.RunnerName)
	}
}

// trim removes up to n idle runners. Each is unregistered first, so GitHub can't hand it a job while
// its container goes away; one GitHub says has a job is skipped.
func (l *Listener) trim(ctx context.Context, spare []pve.Guest, n int) {
	for _, g := range spare {
		if n == 0 || ctx.Err() != nil {
			return
		}
		id, _ := strconv.ParseInt(g.TagValue("runnerid"), 10, 64)
		if err := l.Service.RemoveRunner(ctx, id); err != nil {
			if !errors.Is(err, scaleset.ErrRunnerBusy) {
				// Already gone (its job just ended) or GitHub unreachable: reconcile or the next poll
				// takes care of it.
				l.log().Warn("unregister idle runner", "listener", l.Name(), "vmid", g.VMID, "err", err)
			}
			continue
		}
		if err := l.Host.Destroy(ctx, g.VMID); err != nil {
			// Unregistered, so it can't take a job; its runner exits and reconcile destroys it.
			l.log().Warn("destroy idle runner", "listener", l.Name(), "vmid", g.VMID, "err", err)
		} else {
			l.log().Info("removed idle runner", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name)
		}
		n--
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
