package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/autoscale"
	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/notify"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
)

// listeners builds one listener per scale set: each org target's pools, and each pool of a personal
// account per listed repo (personal accounts only have repo-level runners).
func listeners(cfg *config.Config, prov *provision.Provisioner, n *notify.Notifier) ([]*autoscale.Listener, error) {
	// GitHub answers a message poll within about 50s; the timeout only catches a dead connection.
	hc := &http.Client{Timeout: 2 * time.Minute}
	var out []*autoscale.Listener
	for _, t := range cfg.Targets {
		gh, err := appClient(cfg, t)
		if err != nil {
			return nil, err
		}
		repos := []string{""}
		if t.User != "" {
			repos = t.Repos
		}
		for _, repo := range repos {
			configURL := scaleset.GitHubURL(cfg.GitHub.APIURL) + "/" + t.Owner()
			if repo != "" {
				configURL += "/" + repo
			}
			client := &scaleset.Client{APIBase: cfg.GitHub.APIURL, ConfigURL: configURL, HTTP: hc,
				RegToken: func(ctx context.Context) (string, error) {
					return gh.RegistrationToken(ctx, t.InstallationID, t.Owner(), repo)
				}}
			for _, pn := range t.Pools {
				l := &autoscale.Listener{Service: client, Target: t, Repo: repo, PoolName: pn, Pool: cfg.Pools[pn],
					HostID: cfg.HostID, HostMax: cfg.Proxmox.MaxRunners, Prov: prov, Host: prov.Host,
					Private: func(ctx context.Context, owner, r string) (bool, error) {
						return gh.RepoPrivate(ctx, t.InstallationID, owner, r)
					},
					Failed: func(ctx context.Context, msg string) { n.Send(ctx, notify.SpawnFailed, msg) }}
				l.Group = func(ctx context.Context) (int64, error) {
					if repo != "" {
						private, err := gh.RepoPrivate(ctx, t.InstallationID, t.User, repo)
						if err != nil {
							return 0, err
						}
						if !private {
							return 0, fmt.Errorf("%w: %s/%s is public", github.ErrPublicRepo, t.User, repo)
						}
						return scaleset.DefaultGroupID, nil
					}
					// The REST lookup refuses groups that allow public repositories.
					if _, err := gh.RunnerGroupID(ctx, t.InstallationID, t.Org, t.RunnerGroup); err != nil {
						return 0, err
					}
					return client.RunnerGroupID(ctx, t.RunnerGroup)
				}
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// autoscaler runs the listeners for one config; a reload stops it and starts a new one.
type autoscaler struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startAutoscale(ctx context.Context, cfg *config.Config, n *notify.Notifier) (*autoscaler, error) {
	if !cfg.Autoscale.Enabled || len(cfg.Targets) == 0 {
		return &autoscaler{cancel: func() {}}, nil
	}
	prov, err := provisioner(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ls, err := listeners(cfg, prov, n)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	a := &autoscaler{cancel: cancel}
	names := make([]string, len(ls))
	for i, l := range ls {
		names[i] = l.Name()
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			autoscale.Supervise(ctx, l, 5*time.Second, 5*time.Minute)
		}()
	}
	slog.Info("autoscaling", "listeners", strings.Join(names, ", "))
	return a, nil
}

// stop ends every listener and waits for them to delete their message sessions.
func (a *autoscaler) stop() {
	if a == nil {
		return
	}
	a.cancel()
	a.wg.Wait()
}
