// Package reconcile removes runner guests that are finished or stuck. It reads all state from the
// guests' tags, so it is safe to run after a restart or alongside a manual `spawn`.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// Grace is how long a freshly created guest may sit stopped before it counts as finished; it covers
// the gap between clone and start.
const Grace = 3 * time.Minute

type Action struct {
	VMID   int
	Reason string
}

// Plan decides what to destroy without touching anything, so `status` can show it and tests can
// check it directly.
func Plan(cfg *config.Config, runners []pve.Guest, now time.Time) []Action {
	var out []Action
	for _, g := range runners {
		created, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
		age := now.Sub(time.Unix(created, 0))
		pool := cfg.Pools[g.TagValue("pool")]
		switch {
		case g.TagValue("mode") == "ephemeral" && g.Status == "stopped" && (created == 0 || age > Grace):
			out = append(out, Action{g.VMID, "ephemeral runner finished"})
		case pool != nil && pool.MaxLifetime > 0 && created > 0 && age > pool.MaxLifetime:
			out = append(out, Action{g.VMID, fmt.Sprintf("older than max_lifetime %s", pool.MaxLifetime)})
		}
	}
	return out
}

// Once runs a single reconcile pass and returns what it destroyed.
func Once(ctx context.Context, cfg *config.Config, h *pve.Host, now time.Time) ([]Action, error) {
	runners, err := h.Runners(ctx)
	if err != nil {
		return nil, err
	}
	var done []Action
	var errs []error
	for _, a := range Plan(cfg, runners, now) {
		if err := h.Destroy(ctx, a.VMID); err != nil {
			errs = append(errs, fmt.Errorf("destroy %d: %w", a.VMID, err))
			continue
		}
		done = append(done, a)
	}
	return done, errors.Join(errs...)
}
