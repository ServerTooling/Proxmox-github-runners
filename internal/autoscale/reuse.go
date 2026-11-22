package autoscale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
)

// Reuse (pool.reuse_for) keeps a runner's container after its job and starts its runner again, with a
// new single-use registration, for the next job of the same repository, so Docker images, package
// caches and the checkout are still there.
//
// GitHub hands an acquired job to whichever idle runner of the scale set asks first; it can't be
// steered. So a kept container is only started again ("armed") when every job this listener has
// acquired and not seen start is from its repo, and while it waits, jobs of other repos are not
// acquired (deferred) until it has begun its job or given up after the start grace. That way a kept
// container can only ever be handed a job of the repo it was kept for. A job whose repo doesn't match
// the container's anyway (this listener restarted mid-way, say) ends its reuse: it is destroyed after.
//
// A kept container is also destroyed instead of kept when its disk is maxDiskPercent full, when
// cleaning it fails, when its reuse_for passes, or when a fresh runner needs its slot.

// maxDiskPercent is how full a runner's disk may be and still be kept for another job.
const maxDiskPercent = 80

// pendingExpiry drops a pending job never seen to start (cancelled, or lost by the session), so it
// doesn't keep containers from being reused forever.
const pendingExpiry = time.Hour

type pendingJob struct {
	repo string // pve.ReuseTag of the job's repo
	at   time.Time
}

func (l *Listener) reuse() bool { return l.Pool.ReuseFor > 0 && l.Prov != nil }

// reuseTagOf returns the guest's reuse-<hash> tag, or "".
func reuseTagOf(g pve.Guest) string {
	if v := g.TagValue("reuse"); v != "" {
		return "reuse-" + v
	}
	return ""
}

// holdBack defers a job of another repo while a kept container waits for its own repo's job.
func (l *Listener) holdBack(j scaleset.Job) bool {
	if !l.reuse() || l.armedFor == "" {
		return false
	}
	tag := pve.ReuseTag(j.OwnerName, j.RepositoryName)
	if tag == l.armedFor {
		return false
	}
	if l.deferred == nil {
		l.deferred = map[int64]string{}
	}
	if _, ok := l.deferred[j.RunnerRequestID]; !ok {
		l.log().Info("holding job back while a reused runner waits for its repo's job", "listener", l.Name(),
			"repo", j.OwnerName+"/"+j.RepositoryName, "job", j.JobDisplayName)
	}
	l.deferred[j.RunnerRequestID] = tag
	return true
}

func (l *Listener) addPending(got []int64, repos map[int64]string) {
	if !l.reuse() {
		return
	}
	if l.pending == nil {
		l.pending = map[int64]pendingJob{}
	}
	for _, id := range got {
		l.pending[id] = pendingJob{repo: repos[id], at: l.now()}
	}
}

func (l *Listener) prunePending() {
	for id, p := range l.pending {
		if l.now().Sub(p.at) > pendingExpiry {
			delete(l.pending, id)
		}
	}
}

// releaseDeferred acquires the held-back jobs once no kept container waits any more.
func (l *Listener) releaseDeferred(ctx context.Context, sess *scaleset.Session) {
	if l.armedFor != "" || len(l.deferred) == 0 {
		return
	}
	ids := make([]int64, 0, len(l.deferred))
	for id := range l.deferred {
		ids = append(ids, id)
	}
	got, err := sess.AcquireJobs(ctx, ids)
	if err != nil {
		// They stay queued on GitHub; the next poll tries again.
		l.log().Warn("acquire held-back jobs", "listener", l.Name(), "err", err)
		return
	}
	l.addPending(got, l.deferred)
	l.deferred = nil
	l.log().Info("acquired held-back jobs", "listener", l.Name(), "requested", len(ids), "acquired", len(got))
}

// onlyRepo returns the reuse tag of the one repo every acquired, not yet started job is from, or ""
// when there are none, several, or jobs this listener doesn't know about (from before a restart).
func (l *Listener) onlyRepo(stats scaleset.Statistics, f fleet) string {
	if len(l.pending) == 0 || stats.TotalAssignedJobs > f.busy+len(l.pending) {
		return ""
	}
	repo := ""
	for _, p := range l.pending {
		if p.repo == "" || repo != "" && p.repo != repo {
			return ""
		}
		repo = p.repo
	}
	return repo
}

// useKept arms kept containers for up to n runners that are needed and makes room among the others
// for the fresh runners still to start; it returns how many fresh ones to start.
func (l *Listener) useKept(ctx context.Context, f fleet, stats scaleset.Statistics, n int) int {
	var rest []pve.Guest
	repo := l.onlyRepo(stats, f)
	if l.armedFor != "" && l.armedFor != repo {
		repo = "" // never two repos' containers waiting at once
	}
	for _, g := range f.kept {
		if n > 0 && repo != "" && reuseTagOf(g) == repo {
			l.arm(ctx, g, repo)
			n--
			continue
		}
		rest = append(rest, g)
	}
	l.stepMu.Lock()
	need := l.needRoom
	l.stepMu.Unlock()
	over := f.live + len(f.kept) + n - l.capacity()
	if need && n > 0 {
		over = max(over, 1)
	}
	l.makeRoom(ctx, rest, over)
	return n
}

// makeRoom destroys up to n kept containers, those with the least time left first.
func (l *Listener) makeRoom(ctx context.Context, kept []pve.Guest, n int) {
	l.stepMu.Lock()
	l.needRoom = false
	l.stepMu.Unlock()
	for i := len(kept) - 1; i >= 0 && n > 0; i-- {
		l.destroyKept(ctx, kept[i], "slot needed for a new runner")
		n--
	}
}

func (l *Listener) destroyKept(ctx context.Context, g pve.Guest, reason string) {
	// Its runner already left GitHub when its job ended, so there is nothing to unregister.
	if err := l.Host.Destroy(ctx, g.VMID); err != nil {
		l.log().Warn("destroy kept runner", "listener", l.Name(), "vmid", g.VMID, "err", err)
		return
	}
	l.setState(g.VMID, g.Name, "removed", "reason", reason)
}

// withoutReuseTags drops the tags that change between a reused runner's jobs.
func withoutReuseTags(tags []string, prefixes ...string) []string {
	out := tags[:0:0]
	for _, t := range tags {
		drop := false
		for _, p := range prefixes {
			drop = drop || strings.HasPrefix(t, p)
		}
		if !drop {
			out = append(out, t)
		}
	}
	return out
}

// arm starts a kept container's runner again in the background; it counts as a spawn meanwhile.
func (l *Listener) arm(ctx context.Context, g pve.Guest, repo string) {
	l.stepMu.Lock()
	l.spawning++
	if l.starting == nil {
		l.starting = map[int]bool{}
	}
	if l.arming == nil {
		l.arming = map[int]string{}
	}
	l.starting[g.VMID] = true
	l.arming[g.VMID] = repo
	l.stepMu.Unlock()
	l.armedFor = repo
	delete(l.registered, g.VMID)
	l.spawns.Add(1)
	setID := l.scaleSetID
	go func() {
		defer l.spawns.Done()
		err := l.rearm(ctx, g, setID)
		l.stepMu.Lock()
		l.spawning--
		delete(l.starting, g.VMID)
		delete(l.arming, g.VMID)
		l.stepMu.Unlock()
		if err != nil {
			if ctx.Err() == nil {
				l.log().Error("starting a kept runner again failed; destroying it", "listener", l.Name(), "vmid", g.VMID, "err", err)
			}
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if g2, ok := l.guest(cctx, g.Name); ok {
				l.remove(cctx, g2, "could not be started again", true)
			}
			return
		}
		l.log().Info("runner reused", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name)
	}()
}

func (l *Listener) rearm(ctx context.Context, g pve.Guest, setID int64) error {
	l.step(g.VMID, "reusing")
	jit, err := l.jit(ctx, setID, g.Name)
	if err != nil {
		return err
	}
	// Armed before the runner starts, so cleanup never takes it for a dead runner meanwhile.
	add := []string{pve.ArmedTag(l.now())}
	if jit.Runner.ID != 0 {
		add = append(add, provision.RunnerIDTag(jit.Runner.ID))
	}
	if err := l.retag(ctx, g.Name, add, "kept-", "armed-", "runnerid-"); err != nil {
		return err
	}
	if err := l.Prov.Rearm(ctx, g.VMID, jit); err != nil {
		return err
	}
	l.step(g.VMID, "service started")
	return nil
}

// retag re-reads a guest's tags, drops those with the given prefixes and adds add, under tagMu.
func (l *Listener) retag(ctx context.Context, name string, add []string, drop ...string) error {
	l.tagMu.Lock()
	defer l.tagMu.Unlock()
	g, ok := l.guest(ctx, name)
	if !ok {
		return fmt.Errorf("runner %s is gone", name)
	}
	return l.Prov.Host.SetTags(ctx, g.VMID, append(withoutReuseTags(g.Tags(), drop...), add...))
}

func (l *Listener) guest(ctx context.Context, name string) (pve.Guest, bool) {
	gs, err := l.Host.Runners(ctx)
	if err != nil {
		return pve.Guest{}, false
	}
	for _, g := range gs {
		if g.Name == name {
			return g, true
		}
	}
	return pve.Guest{}, false
}

// startKeep cleans a runner whose job just ended and keeps it for its repo's next job.
func (l *Listener) startKeep(ctx context.Context, j scaleset.Job) {
	l.stepMu.Lock()
	if l.keeping == nil {
		l.keeping = map[string]bool{}
	}
	l.keeping[j.RunnerName] = true
	l.stepMu.Unlock()
	l.labels.Add(1)
	go func() {
		defer l.labels.Done()
		defer func() {
			l.stepMu.Lock()
			delete(l.keeping, j.RunnerName)
			l.stepMu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		l.keep(cctx, j)
	}()
}

func (l *Listener) keep(ctx context.Context, j scaleset.Job) {
	g, ok := l.guest(ctx, j.RunnerName)
	if !ok || g.Status != "running" {
		return
	}
	repo := pve.ReuseTag(j.OwnerName, j.RepositoryName)
	until := l.now().Add(l.Pool.ReuseFor)
	if lt := l.Pool.MaxLifetime; lt > 0 && createdAt(g) > 0 {
		if end := time.Unix(createdAt(g), 0).Add(lt); end.Before(until) {
			until = end
		}
	}
	err := func() error {
		if have := reuseTagOf(g); have != "" && have != repo {
			return errors.New("it ran a job of another repo than the one it was kept for")
		}
		if !until.After(l.now().Add(time.Minute)) {
			return errors.New("max_lifetime is up")
		}
		// Tagged kept first: its runner unit has stopped, and cleanup (reconcile) destroys a running guest
		// with a stopped unit unless it is kept. The listener doesn't arm it while keeping is set.
		if err := l.retag(ctx, g.Name, []string{repo, pve.KeptTag(until)}, "kept-", "armed-", "reuse-"); err != nil {
			return err
		}
		used, err := l.Prov.DiskUsedPercent(ctx, g.VMID)
		if err != nil {
			return err
		}
		if used >= maxDiskPercent {
			return fmt.Errorf("its disk is %d%% full (limit %d%%)", used, maxDiskPercent)
		}
		return l.Prov.Clean(ctx, g.VMID)
	}()
	if err != nil {
		l.log().Info("runner not kept for reuse; destroying it", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name,
			"repo", j.OwnerName+"/"+j.RepositoryName, "reason", err.Error())
		l.destroyKept(ctx, g, "not kept: "+err.Error())
		return
	}
	l.log().Info("runner kept for reuse", "listener", l.Name(), "vmid", g.VMID, "runner", g.Name,
		"repo", j.OwnerName+"/"+j.RepositoryName, "until", until.UTC().Format(time.RFC3339))
}
