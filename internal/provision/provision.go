// Package provision turns "a job needs a runner in pool X for owner Y" into a running LXC: clone the
// pool's template, tag it, inject a single-use JIT config, and start the runner service.
package provision

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	return []string{pve.Tag, "pool-" + tagSafe(pool), "owner-" + tagSafe(owner), "mode-" + mode,
		"from-" + strconv.Itoa(template), "created-" + strconv.FormatInt(created.Unix(), 10)}
}

type Request struct {
	Pool   string
	Target config.Target
	Repo   string // required for personal-account targets
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

	jreq := github.JITRequest{Name: name, Labels: append([]string{"self-hosted", "linux"}, pool.Labels...), WorkFolder: "_work"}
	var jit *github.JITConfig
	t := req.Target
	gh, err := p.GitHub(t)
	if err != nil {
		return nil, err
	}
	if t.Org != "" {
		if jreq.RunnerGroupID, err = gh.RunnerGroupID(ctx, t.InstallationID, t.Org, t.RunnerGroup); err != nil {
			return nil, err
		}
		jit, err = gh.OrgJITConfig(ctx, t.InstallationID, t.Org, jreq)
	} else {
		jit, err = gh.RepoJITConfig(ctx, t.InstallationID, t.User, req.Repo, jreq)
	}
	if err != nil {
		return nil, fmt.Errorf("create JIT runner config: %w", err)
	}

	if err := p.Host.Exec(ctx, pve.LXCClone(template, id, name, p.Cfg.Proxmox.Pool, p.Cfg.Proxmox.Storage, pool.FullClone)); err != nil {
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
	spec := pve.LXCSpec{Cores: pool.Cores, MemoryMB: pool.MemoryMB, Tags: Tags(req.Pool, t.Owner(), pool.Mode, template, p.now()),
		Bridge: pool.Bridge, VLAN: pool.VLAN, Description: "GitHub Actions runner managed by pve-runners"}
	if err = p.Host.Exec(ctx, pve.LXCSet(id, spec)); err != nil {
		return nil, err
	}
	if pool.DiskGB > 0 {
		if err = p.Host.Exec(ctx, pve.LXCResize(id, pool.DiskGB)); err != nil {
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
	if err = p.pushSecret(ctx, id, jit.EncodedJITConfig, "/etc/gh-runner/jitconfig"); err != nil {
		return nil, err
	}
	if err = p.Host.Exec(ctx, pve.LXCExec(id, "systemctl", "start", "gh-runner.service")); err != nil {
		return nil, err
	}
	return &Result{VMID: id, Name: name, RunnerID: jit.Runner.ID, RunnerName: jit.Runner.Name}, nil
}

// pushSecret writes a secret to a 0600 temp file, pushes it into the guest, and removes the temp
// copy. The secret never appears in a command line or in guest config.
func (p *Provisioner) pushSecret(ctx context.Context, id int, secret, dst string) error {
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
	return p.Host.Exec(ctx, pve.LXCPush(id, f.Name(), dst, "0600"))
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
