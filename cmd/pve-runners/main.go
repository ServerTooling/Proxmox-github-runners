// Command pve-runners manages ephemeral GitHub Actions runners on a Proxmox VE host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/buildinfo"
	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/notify"
	"github.com/ServerTooling/proxmox-github-runners/internal/provision"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/reconcile"
	"github.com/ServerTooling/proxmox-github-runners/internal/runnerpkg"
	"github.com/ServerTooling/proxmox-github-runners/internal/setup"
	"github.com/ServerTooling/proxmox-github-runners/internal/update"
)

const (
	binPath  = "/usr/local/bin/pve-runners"
	stateDir = "/var/lib/pve-runners"
)

const usage = `pve-runners: ephemeral GitHub Actions runners on Proxmox VE

Usage:
  pve-runners setup [--listen :8765]          first-run setup page (GitHub Apps, orgs, template)
  pve-runners run                             the service: reconcile + self-update loops
  pve-runners spawn --pool NAME --owner ORG [--repo NAME]
                                              create one ephemeral runner now
  pve-runners reconcile                       destroy finished or expired runners once
  pve-runners status                          show runners on this host
  pve-runners template prepare --ctid ID      install the runner into a container and make it a template
  pve-runners config validate|show            check or print the config (secrets are never in it)
  pve-runners update [--now]                  check for (and install) a newer release
  pve-runners version

Flags common to all commands:
  --config PATH   (default /etc/pve-runners/config.yaml)
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath, "config file")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version":
		fmt.Printf("pve-runners %s (actions/runner %s, %s/%s)\n", buildinfo.Version, buildinfo.RunnerVersion, runtime.GOOS, runtime.GOARCH)
		return nil
	case "setup":
		listen := fs.String("listen", ":8765", "address for the setup page")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdSetup(ctx, *cfgPath, *listen)
	case "run":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdRun(ctx, *cfgPath)
	case "spawn":
		pool := fs.String("pool", "default", "pool name")
		owner := fs.String("owner", "", "org or user from config targets")
		repo := fs.String("repo", "", "repository (personal accounts only)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdSpawn(ctx, *cfgPath, *pool, *owner, *repo)
	case "reconcile":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		done, err := reconcile.Once(ctx, cfg, host(cfg), time.Now())
		for _, a := range done {
			fmt.Printf("destroyed %d: %s\n", a.VMID, a.Reason)
		}
		return err
	case "status":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdStatus(ctx, *cfgPath)
	case "template":
		if len(rest) == 0 || rest[0] != "prepare" {
			return errors.New("usage: pve-runners template prepare --ctid ID")
		}
		ctid := fs.Int("ctid", 0, "container ID to turn into a runner template")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		return cmdTemplatePrepare(ctx, *cfgPath, *ctid)
	case "config":
		if len(rest) == 0 {
			return errors.New("usage: pve-runners config validate|show")
		}
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		data, err := os.ReadFile(*cfgPath)
		if err != nil {
			return err
		}
		cfg, _, err := config.Parse(data)
		if err != nil {
			return err
		}
		if rest[0] == "show" {
			os.Stdout.Write(data)
			return nil
		}
		fmt.Printf("%s is valid: %d targets, %d pools\n", *cfgPath, len(cfg.Targets), len(cfg.Pools))
		return nil
	case "update":
		now := fs.Bool("now", false, "install a newer release if there is one")
		if len(rest) > 0 && rest[0] == "verify" {
			// Started by systemd-run from the previous binary after an update.
			u := updater(nil)
			tag, err := u.Verify(ctx)
			if tag != "" {
				notifier(nil).Send(ctx, notify.UpdateRollback, fmt.Sprintf("%s did not come up healthy; rolled back", tag))
			}
			return err
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		cfg, _ := config.Load(*cfgPath)
		return cmdUpdate(ctx, cfg, *now)
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func host(cfg *config.Config) *pve.Host {
	return &pve.Host{R: pve.ExecRunner{}, Node: cfg.Proxmox.Node, VMIDRange: cfg.Proxmox.VMIDRange}
}

func notifier(cfg *config.Config) *notify.Notifier {
	if cfg == nil {
		if c, err := config.Load(config.DefaultPath); err == nil {
			cfg = c
		} else {
			return nil
		}
	}
	n, err := notify.FromFile(cfg.Notify.WebhookURLFile, cfg.Notify.Format, cfg.HostID)
	if err != nil {
		slog.Error("notify disabled", "err", err)
		return nil
	}
	return n
}

func updater(cfg *config.Config) *update.Updater {
	channel, repo := "edge", config.Default().Update.Repo
	if cfg != nil {
		channel, repo = cfg.Update.Channel, cfg.Update.Repo
	}
	return &update.Updater{Repo: repo, APIBase: github.DefaultAPI, HTTP: &http.Client{Timeout: 5 * time.Minute},
		Channel: channel, Version: buildinfo.Version, GOARCH: runtime.GOARCH, PublicKey: buildinfo.UpdatePublicKey,
		BinPath: binPath, StateDir: stateDir, Now: time.Now,
		Exec: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
			}
			return nil
		}}
}

// githubFor builds the App client for a target from its own key file.
func githubFor(cfg *config.Config) func(config.Target) (provision.JITSource, error) {
	return func(t config.Target) (provision.JITSource, error) {
		pem, err := os.ReadFile(t.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("App key for %s: %w", t.Owner(), err)
		}
		key, err := github.ParsePrivateKey(pem)
		if err != nil {
			return nil, err
		}
		return github.NewClient(cfg.GitHub.APIURL, t.AppID, key), nil
	}
}

func provisioner(cfg *config.Config) *provision.Provisioner {
	return &provision.Provisioner{Cfg: cfg, Host: host(cfg), GitHub: githubFor(cfg), Now: time.Now, TmpDir: os.TempDir()}
}

func cmdSetup(ctx context.Context, cfgPath, listen string) error {
	cfg := config.Default()
	if existing, err := config.Load(cfgPath); err == nil {
		cfg = existing // re-running setup adds to the current config
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfg.Proxmox.Node == "" {
		cfg.Proxmox.Node, _ = os.Hostname()
	}
	s, err := setup.NewServer(cfg, cfgPath)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Println("Open this link in a browser on your network to finish setup:")
	for _, ip := range hostIPs() {
		fmt.Printf("  http://%s:%d/?t=%s\n", ip, port, s.Token)
	}
	fmt.Println("The page closes itself when setup is done (Ctrl+C to abort).")
	select {
	case <-s.Done:
		time.Sleep(time.Second) // let the "done" page reach the browser
	case <-ctx.Done():
	}
	return srv.Shutdown(context.Background())
}

// hostIPs lists this host's non-loopback IPv4 addresses for the setup link. They are only printed to
// the local terminal, never stored or sent anywhere.
func hostIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			out = append(out, n.IP.String())
		}
	}
	if len(out) == 0 {
		out = []string{"<this-host>"}
	}
	return out
}

func findTarget(cfg *config.Config, owner string) (config.Target, error) {
	for _, t := range cfg.Targets {
		if strings.EqualFold(t.Owner(), owner) {
			return t, nil
		}
	}
	if owner == "" && len(cfg.Targets) == 1 {
		return cfg.Targets[0], nil
	}
	return config.Target{}, fmt.Errorf("no target %q in config; run `pve-runners setup` to add it", owner)
}

func cmdSpawn(ctx context.Context, cfgPath, pool, owner, repo string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	t, err := findTarget(cfg, owner)
	if err != nil {
		return err
	}
	res, err := provisioner(cfg).Spawn(ctx, provision.Request{Pool: pool, Target: t, Repo: repo})
	if err != nil {
		notifier(cfg).Send(ctx, notify.SpawnFailed, fmt.Sprintf("pool %s for %s: %v", pool, t.Owner(), err))
		return err
	}
	fmt.Printf("runner %s started in container %d; it will take one job from %s and then be destroyed\n", res.RunnerName, res.VMID, t.Owner())
	return nil
}

func cmdStatus(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	h := host(cfg)
	runners, err := h.Runners(ctx)
	if err != nil {
		return err
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].VMID < runners[j].VMID })
	fmt.Printf("pve-runners %s on %s: %d/%d runners\n", buildinfo.Version, cfg.HostID, len(runners), cfg.Proxmox.MaxRunners)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tPOOL\tOWNER\tAGE")
	for _, g := range runners {
		age := "?"
		var sec int64
		if _, err := fmt.Sscan(g.TagValue("created"), &sec); err == nil && sec > 0 {
			age = time.Since(time.Unix(sec, 0)).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", g.VMID, g.Name, g.Status, g.TagValue("pool"), g.TagValue("owner"), age)
	}
	w.Flush()
	for _, a := range reconcile.Plan(cfg, runners, time.Now()) {
		fmt.Printf("pending cleanup: %d (%s)\n", a.VMID, a.Reason)
	}
	if on, err := h.ClusterFirewallEnabled(ctx); err == nil && !on && cfg.Network.Isolated() {
		fmt.Println("warning: the datacenter firewall is off; runners cannot be isolated from the LAN, so spawning is refused")
	}
	return nil
}

func cmdTemplatePrepare(ctx context.Context, cfgPath string, ctid int) error {
	if ctid == 0 {
		return errors.New("--ctid is required")
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		cfg = config.Default() // template prep can run before setup
		cfg.Proxmox.Node, _ = os.Hostname()
	} else if err != nil {
		return err
	}
	tarball, err := runnerpkg.NewFetcher(stateDir+"/runner").Fetch(ctx, cfg.GitHub.RunnerVersion, cfg.GitHub.RunnerSHA256)
	if err != nil {
		return err
	}
	if err := provision.PrepareTemplate(ctx, host(cfg), ctid, tarball, os.TempDir()); err != nil {
		return err
	}
	fmt.Printf("container %d is now a runner template; use it as the pool template in setup\n", ctid)
	return nil
}

func cmdUpdate(ctx context.Context, cfg *config.Config, install bool) error {
	u := updater(cfg)
	rel, err := u.Latest(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		fmt.Printf("pve-runners %s is current\n", buildinfo.Version)
		return nil
	}
	if !install {
		fmt.Printf("%s is available (running %s); run `pve-runners update --now` to install\n", rel.Tag, buildinfo.Version)
		return nil
	}
	bin, err := u.Download(ctx, rel)
	if err != nil {
		return err
	}
	return u.Install(ctx, rel.Tag, bin)
}

// selfCheck is what a freshly updated binary must pass before it confirms the update.
func selfCheck(ctx context.Context, cfg *config.Config) error {
	if _, err := host(cfg).Guests(ctx); err != nil {
		return fmt.Errorf("cannot list guests: %w", err)
	}
	for _, t := range cfg.Targets {
		if _, err := githubFor(cfg)(t); err != nil {
			return err
		}
	}
	return nil
}

func cmdRun(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	n := notifier(cfg)
	if err := selfCheck(ctx, cfg); err != nil {
		return fmt.Errorf("self-check failed: %w", err)
	}
	u := updater(cfg)
	if err := u.Confirm(buildinfo.Version); err != nil {
		slog.Error("confirm update", "err", err)
	}
	slog.Info("started", "version", buildinfo.Version, "host", cfg.HostID, "targets", len(cfg.Targets))

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	reconcileTick := time.NewTicker(30 * time.Second)
	updateTick := time.NewTicker(cfg.Update.Interval)
	defer reconcileTick.Stop()
	defer updateTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reload:
			c, err := config.Load(cfgPath)
			if err != nil {
				slog.Error("reload failed; keeping the old config", "err", err)
				continue
			}
			cfg = c
			slog.Info("config reloaded")
		case <-reconcileTick.C:
			done, err := reconcile.Once(ctx, cfg, host(cfg), time.Now())
			for _, a := range done {
				slog.Info("destroyed runner", "vmid", a.VMID, "reason", a.Reason)
			}
			if err != nil {
				slog.Error("reconcile", "err", err)
			}
		case <-updateTick.C:
			if !cfg.Update.Enabled || u.Pending() || provision.Busy() {
				continue
			}
			if err := cmdUpdate(ctx, cfg, true); err != nil {
				slog.Error("self-update", "err", err)
				n.Send(ctx, notify.UpdateFailed, err.Error())
			}
		}
	}
}
