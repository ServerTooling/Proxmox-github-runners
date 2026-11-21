package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// ErrNoDocker refuses a docker pool's spawn on a host without docker.ssh.
var ErrNoDocker = errors.New("docker pools need docker.ssh set")

// spawnDocker creates one ephemeral runner container on the Docker machine. Any failure after the
// container exists removes it again.
func (p *Provisioner) spawnDocker(ctx context.Context, req Request, pool *config.Pool) (res *Result, err error) {
	if p.Docker == nil || p.DockerImage == nil {
		return nil, ErrNoDocker
	}
	if err := checkRepo(req); err != nil {
		return nil, err
	}
	// Outside the lock: the first import of a release copies gigabytes and can take minutes, and
	// DockerImage serializes imports itself.
	image, err := p.DockerImage(ctx, pool.Image)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", pool.Image, err)
	}

	p.dmu.Lock()
	locked := true
	defer func() {
		if locked {
			p.dmu.Unlock()
		}
	}()
	if draining.Load() {
		return nil, ErrDraining
	}
	d := p.Docker
	if err := d.CheckRuntime(ctx); err != nil {
		return nil, err
	}
	runners, err := d.Runners(ctx)
	if err != nil {
		return nil, err
	}
	if len(runners) >= p.Cfg.Docker.MaxRunners {
		return nil, fmt.Errorf("%w: docker machine has %d/%d runners (docker.max_runners)", ErrAtCapacity,
			len(runners), p.Cfg.Docker.MaxRunners)
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
	if err := d.EnsureNetwork(ctx); err != nil {
		return nil, err
	}
	if p.Cfg.Network.Isolated() {
		if err := d.Isolate(ctx, image, pve.EgressRules(p.Cfg.Network.Blocked, p.Cfg.Network.Allow)); err != nil {
			return nil, err
		}
	}
	id, err := d.NextFreeID(ctx)
	if err != nil {
		return nil, err
	}
	name := p.runnerName(req.Pool, id)
	jit, err := p.jit(ctx, req, name, pool.Labels)
	if err != nil {
		return nil, fmt.Errorf("create JIT runner config: %w", err)
	}
	tags := append(Tags(req.Pool, req.Target.Owner(), pool.Mode, 0, p.now()), req.Tags...)
	if jit.Runner.ID != 0 {
		tags = append(tags, RunnerIDTag(jit.Runner.ID))
	}
	if req.Step != nil {
		req.Step(id, "provisioning")
	}
	err = d.Create(ctx, docker.Spec{ID: id, Name: name, Image: image, Cores: pool.Cores, MemoryMB: pool.MemoryMB,
		Tags: tags, DNS: p.Cfg.Network.DNS})
	// Removed on any failure, including a create that failed halfway and left the container behind.
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if derr := d.Destroy(cctx, id); derr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup of %d failed: %w", id, derr))
			}
		}
	}()
	if err != nil {
		return nil, err
	}
	// Listed now, so the next spawn counts this container and can't pick its ID.
	p.dmu.Unlock()
	locked = false
	if err = d.Push(ctx, name, jit.EncodedJITConfig, "/etc/gh-runner/jitconfig", "0600"); err != nil {
		return nil, err
	}
	if req.Step != nil {
		req.Step(id, "files pushed")
	}
	if err = d.Exec(ctx, name, "sh", "-c", startRunner); err != nil {
		return nil, fmt.Errorf("runner service did not start in %d: %w", id, err)
	}
	if req.Step != nil {
		req.Step(id, "service started")
	}
	return &Result{VMID: id, Name: name, RunnerID: jit.Runner.ID, RunnerName: jit.Runner.Name}, nil
}
