// Package provision turns "a job needs a runner in pool X for owner Y" into a running LXC: clone the
// pool's template, tag it, inject a single-use JIT config, and start the runner service.
package provision

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

//go:embed template-setup.sh
var templateSetupScript []byte

// inFlight counts spawns in progress; the self-updater waits for zero before restarting.
var inFlight atomic.Int32

func Busy() bool { return inFlight.Load() > 0 }

// draining stops new spawns so an update can restart between them. While jobs queue, spawns run back to
// back, so waiting for a gap alone could put an update off for as long as the queue lasts.
var draining atomic.Bool

// ErrDraining refuses a spawn while an update waits to restart; the job stays queued on GitHub and the
// restarted service spawns for it.
var ErrDraining = errors.New("not starting runners while an update installs")

// drainPoll is how often Drain checks whether the last spawn has finished.
var drainPoll = 500 * time.Millisecond

// Drain stops new spawns and waits up to timeout for running ones to finish. On success spawns stay
// stopped until release is called (or the process restarts); on failure they are released again.
func Drain(ctx context.Context, timeout time.Duration) (release func(), err error) {
	draining.Store(true)
	release = func() { draining.Store(false) }
	deadline := time.Now().Add(timeout)
	for inFlight.Load() > 0 {
		if time.Now().After(deadline) {
			release()
			return nil, fmt.Errorf("a runner was still starting after %s", timeout)
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-time.After(drainPoll):
		}
	}
	return release, nil
}

// JITSource is the slice of the GitHub client provisioning needs.
type JITSource interface {
	RunnerGroupID(ctx context.Context, inst int64, org, name string) (int64, error)
	OrgJITConfig(ctx context.Context, inst int64, org string, req github.JITRequest) (*github.JITConfig, error)
	RepoJITConfig(ctx context.Context, inst int64, owner, repo string, req github.JITRequest) (*github.JITConfig, error)
}

type Provisioner struct {
	Cfg  *config.Config
	Host *pve.Host
	// GitHub returns the client for a target's own App (one private App per org or account).
	GitHub func(config.Target) (JITSource, error)
	// Templates returns the template container for a pool's image, creating it if needed.
	Templates func(ctx context.Context, image string) (int, error)
	Now       func() time.Time
	TmpDir    string
	// NoLinkedClones is set when proxmox.storage can't hold linked clones (a directory or thick LVM
	// storage), so every pool full-clones there.
	NoLinkedClones bool

	// mu serializes spawns until the new guest is cloned and tagged, so concurrent callers can't both
	// pass the capacity checks for the last free slot or pick the same free guest ID. Proxmox locks the
	// template for the whole clone anyway, so clones of one template can't overlap; the rest of a spawn
	// (start, boot, runner service) runs outside it, alongside the next spawn's clone.
	mu sync.Mutex
}

// Proxmox tags allow only [a-z0-9_+.-], so owner names are lowercased and anything else dropped.
func tagSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Tags returns the tags a runner guest carries; reconcile and status read state back from them, and
// image pruning reads from-<template> to keep templates that runners were cloned from.
func Tags(pool, owner, mode string, template int, created time.Time) []string {
	return []string{pve.Tag, "pool-" + tagSafe(pool), OwnerTag(owner), "mode-" + mode,
		"from-" + strconv.Itoa(template), "created-" + strconv.FormatInt(created.Unix(), 10)}
}

// RunnerIDTag records the GitHub runner ID, so an idle runner can be unregistered before its guest goes.
func RunnerIDTag(id int64) string { return "runnerid-" + strconv.FormatInt(id, 10) }

// CacheDir is the host folder an owner's runners share (pools with cache: true).
func CacheDir(cfg *config.Config, owner string) string {
	return filepath.Join(cfg.Cache.Dir, tagSafe(owner))
}

// cacheEnv points the runner's tools at the mounted cache; the runner reads .env into every job's
// environment. The tool cache is where setup-node, setup-go and the like keep the versions they install.
var cacheEnv = strings.ReplaceAll(`RUNNER_TOOL_CACHE=@/toolcache
AGENT_TOOLSDIRECTORY=@/toolcache
npm_config_cache=@/npm
YARN_CACHE_FOLDER=@/yarn
GOMODCACHE=@/go/mod
GOCACHE=@/go/build
PIP_CACHE_DIR=@/pip
PLAYWRIGHT_BROWSERS_PATH=@/ms-playwright
`, "@", pve.CacheMount)

// OwnerTag is the tag naming the org or account a runner serves.
func OwnerTag(owner string) string { return "owner-" + tagSafe(owner) }

type Request struct {
	Pool   string
	Target config.Target
	Repo   string // required for personal-account targets
	// JIT, when set, makes the runner's single-use config instead of the target's App (the
	// autoscaler registers runners inside its scale set this way).
	JIT func(ctx context.Context, name string) (*github.JITConfig, error)
	// Tags are added to the guest's own tags.
	Tags []string
	// Step, when set, is told each step of the start as it completes ("provisioning" once the guest
	// exists, then "files pushed" and "service started").
	Step func(vmid int, step string)
}

type Result struct {
	VMID       int
	Name       string
	RunnerID   int64
	RunnerName string
}

var ErrAtCapacity = errors.New("pool at capacity")

// ErrFirewallOff stops spawning when LAN isolation is on but can't be enforced.
var ErrFirewallOff = errors.New("the Proxmox datacenter firewall is off, so runners can't be kept off the LAN; " +
	"turn it on under Datacenter > Firewall > Options (check your own access rules first), " +
	"or set network.isolate: false to run without isolation")

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Spawn creates one ephemeral runner. Any failure after the clone destroys the half-built guest.
func (p *Provisioner) Spawn(ctx context.Context, req Request) (res *Result, err error) {
	inFlight.Add(1)
	defer inFlight.Add(-1)
	p.mu.Lock()
	locked := true
	defer func() {
		if locked {
			p.mu.Unlock()
		}
	}()
	// Checked after inFlight is raised and after waiting our turn, so Drain either sees this spawn or
	// this spawn sees Drain, including spawns that queued behind one already running.
	if draining.Load() {
		return nil, ErrDraining
	}

	pool, ok := p.Cfg.Pools[req.Pool]
	if !ok {
		return nil, fmt.Errorf("unknown pool %q", req.Pool)
	}
	if pool.Type != "lxc" {
		return nil, fmt.Errorf("pool %q is type %s; only lxc pools are supported so far", req.Pool, pool.Type)
	}
	if req.Target.User != "" && req.Repo == "" {
		return nil, fmt.Errorf("%s is a personal account; pass a repo (personal accounts only have repo-level runners)", req.Target.User)
	}

	runners, err := p.Host.Runners(ctx)
	if err != nil {
		return nil, err
	}
	if len(runners) >= p.Cfg.Proxmox.MaxRunners {
		return nil, fmt.Errorf("%w: host has %d/%d runners (proxmox.max_runners)", ErrAtCapacity, len(runners), p.Cfg.Proxmox.MaxRunners)
	}
	count := 0
	for _, g := range runners {
		if g.TagValue("pool") == tagSafe(req.Pool) {
			count++
		}
	}
	if count >= pool.Max {
		return nil, fmt.Errorf("%w: %s has %d/%d runners", ErrAtCapacity, req.Pool, count, pool.Max)
	}
	free, err := p.Host.FreeMemoryMB(ctx)
	if err != nil {
		return nil, err
	}
	if need := pool.MemoryMB + p.Cfg.Proxmox.MinFreeMemoryMB; free < need {
		return nil, fmt.Errorf("%w: node has %d MB free, needs %d MB", ErrAtCapacity, free, need)
	}

	isolate := p.Cfg.Network.Isolated()
	if isolate {
		on, err := p.Host.ClusterFirewallEnabled(ctx)
		if err != nil {
			return nil, err
		}
		if !on {
			return nil, ErrFirewallOff
		}
		if err := p.Host.EnsureEgressGroup(ctx, pve.EgressRules(p.Cfg.Network.Blocked, p.Cfg.Network.Allow)); err != nil {
			return nil, err
		}
	}

	template := pool.Template
	if pool.Image != "" {
		if p.Templates == nil {
			return nil, fmt.Errorf("pool %q uses an image but image support is not configured", req.Pool)
		}
		if template, err = p.Templates(ctx, pool.Image); err != nil {
			return nil, fmt.Errorf("image %s: %w", pool.Image, err)
		}
	}

	id, err := p.Host.NextFreeID(ctx)
	if err != nil {
		return nil, err
	}
	// The host ID keeps runner names unique when several Proxmox hosts serve the same org.
	name := fmt.Sprintf("gh-%s-%s-%d", tagSafe(p.Cfg.HostID), tagSafe(req.Pool), id)
	if p.Cfg.HostID == "" {
		name = fmt.Sprintf("gh-%s-%d", tagSafe(req.Pool), id)
	}

	t := req.Target
	jit, err := p.jit(ctx, req, name, pool.Labels)
	if err != nil {
		return nil, fmt.Errorf("create JIT runner config: %w", err)
	}

	if err := p.Host.EnsurePool(ctx, p.Cfg.Proxmox.Pool); err != nil {
		return nil, err
	}
	step := func(s string) {
		if req.Step != nil {
			req.Step(id, s)
		}
	}
	// Logged before the clone, which can take a minute or more on storage without linked clones.
	step("provisioning")
	if err := p.Host.Exec(ctx, pve.LXCClone(template, id, name, p.Cfg.Proxmox.Pool, p.Cfg.Proxmox.Storage,
		pool.FullClone || p.NoLinkedClones)); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			// Use a fresh context: the caller's may be what failed.
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if derr := p.Host.Destroy(cctx, id); derr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup of %d failed: %w", id, derr))
			}
		}
	}()
	tags := append(Tags(req.Pool, t.Owner(), pool.Mode, template, p.now()), req.Tags...)
	if jit.Runner.ID != 0 {
		tags = append(tags, RunnerIDTag(jit.Runner.ID))
	}
	spec := pve.LXCSpec{Cores: pool.Cores, MemoryMB: pool.MemoryMB, Tags: tags,
		Bridge: pool.Bridge, VLAN: pool.VLAN, Nameservers: p.Cfg.Network.DNS,
		Description: "GitHub Actions runner managed by pve-runners"}
	if pool.Cache {
		spec.CacheDir = CacheDir(p.Cfg, t.Owner())
		// The parent can be passed through but not listed. The owner's folder is open to every user
		// in the container, whose IDs are shifted on the host, with the sticky bit so files stay
		// their creator's.
		if err = p.Host.Exec(ctx, pve.MkdirCmd(p.Cfg.Cache.Dir, "0711")); err != nil {
			return nil, err
		}
		if err = p.Host.Exec(ctx, pve.MkdirCmd(spec.CacheDir, "1777")); err != nil {
			return nil, err
		}
	}
	if err = p.Host.Exec(ctx, pve.LXCSet(id, spec)); err != nil {
		return nil, err
	}
	// Tagged now, so the next spawn counts this guest and can't pick its ID.
	p.mu.Unlock()
	locked = false
	if pool.DiskGB > 0 {
		err = p.Host.Exec(ctx, pve.LXCResize(id, pool.DiskGB))
		if pve.ResizeNotNeeded(err) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	if isolate {
		for _, c := range pve.GuestFirewallCmds(p.Host.Node, id) {
			if err = p.Host.Exec(ctx, c); err != nil {
				return nil, err
			}
		}
	}
	if err = p.Host.Exec(ctx, pve.LXCStart(id)); err != nil {
		return nil, err
	}
	// Every file goes in before the runner service starts. Images before the unit was disabled at boot
	// still try it then; their ConditionPathExists skips it while no jitconfig exists, so the .env goes
	// first (the runner reads it only when it starts) and the jitconfig last.
	if pool.Cache {
		if err = p.pushFile(ctx, id, cacheEnv, "/opt/actions-runner/.env", "0644"); err != nil {
			return nil, err
		}
	}
	if err = p.pushFile(ctx, id, jit.EncodedJITConfig, "/etc/gh-runner/jitconfig", "0600"); err != nil {
		return nil, err
	}
	step("files pushed")
	if err = p.Host.Exec(ctx, pve.LXCExec(id, "sh", "-c", startRunner)); err != nil {
		return nil, fmt.Errorf("runner service did not start in %d: %w", id, err)
	}
	step("service started")
	return &Result{VMID: id, Name: name, RunnerID: jit.Runner.ID, RunnerName: jit.Runner.Name}, nil
}

// startRunner waits for the guest's systemd to finish booting first. Right after "pct start" systemd
// may not even answer on its bus yet ("Failed to connect to bus"), and a start queued while boot jobs
// (such as systemd-sysusers) are still settling is refused as a destructive transaction, so it polls
// until the system is running. A degraded or slow boot still goes ahead after the wait, and the runner
// unit reports its own failures. The unit must be running afterwards: an older image's unit is
// skipped without an error when its condition fails, and that must fail the spawn, not leave a guest
// with no runner in it.
const startRunner = `i=0
until s=$(systemctl is-system-running 2>/dev/null); [ "$s" = running ] || [ "$s" = degraded ]; do
  i=$((i+1)); [ "$i" -ge 180 ] && break; sleep 1
done
systemctl start gh-runner.service || exit 1
exec systemctl is-active gh-runner.service`

func (p *Provisioner) jit(ctx context.Context, req Request, name string, labels []string) (*github.JITConfig, error) {
	if req.JIT != nil {
		return req.JIT(ctx, name)
	}
	jreq := github.JITRequest{Name: name, Labels: append([]string{"self-hosted", "linux"}, labels...), WorkFolder: "_work"}
	t := req.Target
	gh, err := p.GitHub(t)
	if err != nil {
		return nil, err
	}
	if t.Org == "" {
		return gh.RepoJITConfig(ctx, t.InstallationID, t.User, req.Repo, jreq)
	}
	if jreq.RunnerGroupID, err = gh.RunnerGroupID(ctx, t.InstallationID, t.Org, t.RunnerGroup); err != nil {
		return nil, err
	}
	return gh.OrgJITConfig(ctx, t.InstallationID, t.Org, jreq)
}

// pushFile writes content to a 0600 temp file, pushes it into the guest, and removes the temp copy.
// A secret pushed this way never appears in a command line or in guest config.
func (p *Provisioner) pushFile(ctx context.Context, id int, secret, dst, perms string) error {
	f, err := os.CreateTemp(p.TmpDir, "pve-runners-secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(secret); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return p.Host.Exec(ctx, pve.LXCPush(id, f.Name(), dst, perms))
}

// PrepareTemplate installs the runner into an existing, running container and converts it to a
// template. The container must sit outside the runner ID range so reconcile never touches it.
func PrepareTemplate(ctx context.Context, h *pve.Host, ctid int, tarball, tmpDir string) error {
	if h.InRange(ctid) {
		return fmt.Errorf("container %d is inside the runner ID range %d-%d; templates must live outside it", ctid, h.VMIDRange[0], h.VMIDRange[1])
	}
	gs, err := h.Guests(ctx)
	if err != nil {
		return err
	}
	var found *pve.Guest
	for i := range gs {
		if gs[i].VMID == ctid {
			found = &gs[i]
		}
	}
	if found == nil || found.Type != "lxc" {
		return fmt.Errorf("no container %d on this cluster", ctid)
	}
	if found.Template == 1 {
		return fmt.Errorf("container %d is already a template", ctid)
	}
	if found.Status != "running" {
		if err := h.Exec(ctx, pve.LXCFeatures(ctid)); err != nil {
			return err
		}
		if err := h.Exec(ctx, pve.LXCStart(ctid)); err != nil {
			return err
		}
	}
	script, err := os.CreateTemp(tmpDir, "template-setup-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(script.Name())
	if _, err := script.Write(templateSetupScript); err != nil {
		script.Close()
		return err
	}
	script.Close()
	steps := []pve.Cmd{
		pve.LXCPush(ctid, tarball, "/tmp/actions-runner.tar.gz", "0644"),
		pve.LXCPush(ctid, script.Name(), "/tmp/template-setup.sh", "0700"),
		pve.LXCExec(ctid, "/bin/sh", "/tmp/template-setup.sh"),
		pve.LXCExec(ctid, "rm", "-f", "/tmp/template-setup.sh"),
		pve.LXCStop(ctid),
		pve.LXCTemplate(ctid),
	}
	for _, c := range steps {
		if err := h.Exec(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
