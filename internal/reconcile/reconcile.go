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

// StartGrace is how long a running ephemeral guest may go without its runner unit active before it
// counts as dead. It covers boot, the wait for boot to settle, and pushing the config.
const StartGrace = 10 * time.Minute

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
	actions := Plan(cfg, runners, now)
	actions = append(actions, deadRunners(ctx, h, runners, actions, now)...)
	var done []Action
	var errs []error
	for _, a := range actions {
		if err := h.Destroy(ctx, a.VMID); err != nil {
			errs = append(errs, fmt.Errorf("destroy %d: %w", a.VMID, err))
			continue
		}
		done = append(done, a)
	}
	return done, errors.Join(errs...)
}

// deadRunners finds running ephemeral guests whose runner unit has ended without the guest powering
// off: the unit stops the guest when the runner exits, but a guest that was started again (by hand,
// or by a host restart) just sits there with nothing to run. Guests whose state can't be read are
// left alone; the next pass tries again.
func deadRunners(ctx context.Context, h *pve.Host, runners []pve.Guest, planned []Action, now time.Time) []Action {
	skip := map[int]bool{}
	for _, a := range planned {
		skip[a.VMID] = true
	}
	var out []Action
	for _, g := range runners {
		created, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
		if skip[g.VMID] || g.Status != "running" || g.TagValue("mode") != "ephemeral" ||
			created == 0 || now.Sub(time.Unix(created, 0)) < StartGrace {
			continue
		}
		state, err := h.RunnerUnitState(ctx, g.VMID)
		if err != nil {
			continue
		}
		if state == "inactive" || state == "failed" {
			out = append(out, Action{g.VMID, "runner service " + state})
		}
	}
	return out
}
