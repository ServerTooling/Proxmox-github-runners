// Package cache keeps the shared runner cache folders (pools with cache: true) from growing without
// bound. Each org or account has its own folder; one is only emptied while none of that owner's
// runners are running, so a job never sees its cache disappear under it.
package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

type Result struct {
	Owner   string
	Dir     string
	Bytes   int64
	Cleared bool
	// Skipped says why a folder that should be emptied was left alone.
	Skipped string
}

// Owners are the targets with at least one pool that uses the cache.
func Owners(cfg *config.Config) []string {
	var out []string
	for _, t := range cfg.Targets {
		for _, pn := range t.Pools {
			if p := cfg.Pools[pn]; p != nil && p.Cache {
				out = append(out, t.Owner())
				break
			}
		}
	}
	return out
}

// Mode says which folders Run empties.
type Mode int

const (
	List  Mode = iota // none: only measure
	Trim              // the ones over cache.max_gb (the service does this with each image check)
	Clear             // all of them
)

// Run measures each owner's folder and empties the ones mode picks, unless runners are using them.
func Run(ctx context.Context, cfg *config.Config, h *pve.Host, mode Mode) ([]Result, error) {
	runners, err := h.Runners(ctx)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, owner := range Owners(cfg) {
		r := Result{Owner: owner, Dir: provision.CacheDir(cfg, owner)}
		du, err := h.Output(ctx, pve.DiskUsageCmd(r.Dir))
		if err != nil {
			// No folder yet: none of this owner's runners have started since the cache was turned on.
			out = append(out, r)
			continue
		}
		r.Bytes, _ = strconv.ParseInt(strings.Fields(string(du) + " 0")[0], 10, 64)
		if mode == List || mode == Trim && r.Bytes <= int64(cfg.Cache.MaxGB)<<30 {
			out = append(out, r)
			continue
		}
		switch {
		case inUse(runners, owner):
			r.Skipped = "runners are using it"
		case provision.Busy():
			// A runner being created right now may be mounting it.
			r.Skipped = "a runner is starting"
		default:
			if err := h.Exec(ctx, pve.EmptyDirCmd(r.Dir)); err != nil {
				return out, fmt.Errorf("empty %s: %w", r.Dir, err)
			}
			r.Cleared = true
		}
		out = append(out, r)
	}
	return out, nil
}

// inUse reports whether any of the owner's runners has its guest up (and so the folder mounted).
func inUse(runners []pve.Guest, owner string) bool {
	tag := provision.OwnerTag(owner)
	for _, g := range runners {
		if g.HasTag(tag) && g.Status != "stopped" {
			return true
		}
	}
	return false
}
