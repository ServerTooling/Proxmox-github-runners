// Command pve-runners manages ephemeral GitHub Actions runners on a Proxmox VE host.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/buildinfo"
	"github.com/ServerTooling/proxmox-github-runners/internal/cache"
	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/images"
	"github.com/ServerTooling/proxmox-github-runners/internal/logship"
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
  pve-runners setup [--listen :8765] [--open-firewall]
                                              first-run setup page (GitHub Apps, orgs, template);
                                              --open-firewall allows the port until setup ends
  pve-runners run                             the service: autoscaling, reconcile, image and self-update loops,
                                              and the settings page (https://<host>:8765, Proxmox login)
  pve-runners spawn --pool NAME --owner ORG [--repo NAME]
                                              create one ephemeral runner now
  pve-runners reconcile                       destroy finished or expired runners once
  pve-runners status                          show runners on this host
  pve-runners template prepare --ctid ID      install the runner into a container and make it a template
  pve-runners images list|pull|prune          runner images: show templates, fetch pools' images now, age out old ones
  pve-runners logs ship                       send new log lines to Loki now (see logs: in the config)
  pve-runners cache list|clear                shared runner caches: show their sizes, or empty the ones
                                              no running runner is using
  pve-runners config init|validate|show       write the default config (never over a non-empty one),
                                              check it, or print it (secrets are never in it)
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
		openFW := fs.Bool("open-firewall", false, "allow the setup port through the Proxmox host firewall until setup ends")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdSetup(ctx, *cfgPath, *listen, *openFW)
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
		return cmdSpawn(ctx, os.Stdout, *cfgPath, *pool, *owner, *repo)
	case "reconcile":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdReconcile(ctx, os.Stdout, *cfgPath)
	case "status":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return cmdStatus(ctx, os.Stdout, *cfgPath)
	case "template":
		if len(rest) == 0 || rest[0] != "prepare" {
			return errors.New("usage: pve-runners template prepare --ctid ID")
		}
		ctid := fs.Int("ctid", 0, "container ID to turn into a runner template")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		return cmdTemplatePrepare(ctx, os.Stdout, *cfgPath, *ctid)
	case "images":
		if len(rest) == 0 {
			return errors.New("usage: pve-runners images list|pull|prune")
		}
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		return cmdImages(ctx, os.Stdout, *cfgPath, rest[0])
	case "logs":
		if len(rest) == 0 || rest[0] != "ship" {
			return errors.New("usage: pve-runners logs ship")
		}
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		return cmdLogsShip(ctx, os.Stdout, *cfgPath)
	case "cache":
		if len(rest) == 0 || rest[0] != "list" && rest[0] != "clear" {
			return errors.New("usage: pve-runners cache list|clear")
		}
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		return cmdCache(ctx, os.Stdout, *cfgPath, rest[0] == "clear")
	case "config":
		if len(rest) == 0 {
			return errors.New("usage: pve-runners config init|validate|show")
		}
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		if rest[0] == "init" {
			wrote, err := config.Init(*cfgPath, nodeName())
			if err == nil && wrote {
				fmt.Printf("wrote %s with the default settings\n", *cfgPath)
			} else if err == nil {
				fmt.Printf("%s already has settings; left it as is\n", *cfgPath)
			}
			return err
		}
		return cmdConfig(os.Stdout, *cfgPath, rest[0] == "show")
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
		return cmdUpdate(ctx, os.Stdout, cfg, *now)
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

// logShipper returns nil when log shipping is not configured.
func logShipper(cfg *config.Config) (*logship.Shipper, error) {
	if !cfg.Logs.On() {
		return nil, nil
	}
	b, err := os.ReadFile(cfg.Logs.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read logs token file: %w", err)
	}
	return &logship.Shipper{Host: host(cfg), URL: cfg.Logs.URL, User: cfg.Logs.User, Token: strings.TrimSpace(string(b)),
		HostID: cfg.HostID, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}, nil
}

// startWeb serves the settings page until ctx ends or the returned stop is called. It needs the
// Proxmox node certificate: to serve HTTPS, and to trust the local Proxmox API that checks logins.
func startWeb(ctx context.Context, cfg *config.Config, cfgPath string, imageBusy *atomic.Bool) (stop func()) {
	if !cfg.Web.On() {
		return func() {}
	}
	cert, err := nodeCert()
	if err != nil {
		slog.Error("settings page off", "err", err)
		return func() {}
	}
	s, err := setup.NewServer(cfg, cfgPath)
	if err != nil {
		slog.Error("settings page off", "err", err)
		return func() {}
	}
	s.Service = &setup.Service{Login: setup.PVELogin("https://127.0.0.1:8006", setup.PinnedClient(cert.Certificate[0])),
		SecretDir: config.DefaultDir, FailDelay: 2 * time.Second, Saved: restartSoon, Ctx: ctx,
		Status:  func(ctx context.Context, w io.Writer) error { return cmdStatus(ctx, w, cfgPath) },
		Actions: webActions(cfgPath, imageBusy)}
	srv := &http.Server{Addr: cfg.Web.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("settings page", "err", err)
		}
	}()
	_, port, _ := net.SplitHostPort(cfg.Web.Listen)
	slog.Info("settings page", "url", "https://"+net.JoinHostPort(firstOr(preferredIPs(), nodeName()), port))
	if blocked, _ := host(cfg).HostInputBlocked(ctx); blocked {
		slog.Warn("the Proxmox firewall drops inbound connections; allow port " + port + " on this host to reach the settings page")
	}
	// stop waits for the listener to close, so a reload can bind the same port again right away.
	done, closed := make(chan struct{}), make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = srv.Close()
		close(closed)
	}()
	return func() { close(done); <-closed }
}

// webActions are the CLI commands offered as buttons on the settings page. Each loads the config from
// disk like its command does, so it acts on what was last saved. Image work and updates share the run
// loop's guard: two syncs would download the same image twice, and an update restarts the service.
func webActions(cfgPath string, imageBusy *atomic.Bool) []setup.Action {
	images := func(sub string) func(ctx context.Context, w io.Writer, _ map[string]string) error {
		return func(ctx context.Context, w io.Writer, _ map[string]string) error {
			if sub != "list" {
				if !imageBusy.CompareAndSwap(false, true) {
					return errors.New("an image sync is already running; try again when it ends")
				}
				defer imageBusy.Store(false)
			}
			return cmdImages(ctx, w, cfgPath, sub)
		}
	}
	update := func(install bool) func(ctx context.Context, w io.Writer, _ map[string]string) error {
		return func(ctx context.Context, w io.Writer, _ map[string]string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			// Same guards as the automatic update: a restart mid-spawn would strand a half-made runner.
			if install && (updater(cfg).Pending() || imageBusy.Load() || provision.Busy()) {
				return errors.New("a runner is starting, an image sync is running, or an update is still being verified; try again shortly")
			}
			return cmdUpdate(ctx, w, cfg, install)
		}
	}
	return []setup.Action{
		{Name: "reconcile", Label: "Clean up runners now", Help: "Destroy finished or expired runners (pve-runners reconcile).",
			Run: func(ctx context.Context, w io.Writer, _ map[string]string) error {
				return cmdReconcile(ctx, w, cfgPath)
			}},
		{Name: "spawn", Label: "Start a runner", Help: "One ephemeral runner now; repo is only for personal accounts.",
			Args: []string{"pool", "owner", "repo"},
			Run: func(ctx context.Context, w io.Writer, a map[string]string) error {
				pool := a["pool"]
				if pool == "" {
					pool = "default"
				}
				return cmdSpawn(ctx, w, cfgPath, pool, a["owner"], a["repo"])
			}},
		{Name: "images-list", Label: "List images", Help: "Runner image templates on this host.", Run: images("list")},
		{Name: "images-pull", Label: "Pull latest images", Help: "Fetch every pool's newest image now.",
			Run: images("pull")},
		{Name: "images-prune", Label: "Prune old images", Help: "Age out templates and downloads no pool uses.",
			Confirm: "Delete old image templates and downloads?", Run: images("prune")},
		{Name: "update-check", Label: "Check for updates", Run: update(false)},
		{Name: "update-now", Label: "Update now", Help: "Installs the newest release and restarts the service.",
			Confirm: "Install the update and restart pve-runners?", Run: update(true)},
		{Name: "logs-ship", Label: "Ship logs now", Help: "Send new log lines to Loki.",
			Run: func(ctx context.Context, w io.Writer, _ map[string]string) error { return cmdLogsShip(ctx, w, cfgPath) }},
		{Name: "cache-list", Label: "Show cache sizes", Help: "Each org's shared runner cache folder.",
			Run: func(ctx context.Context, w io.Writer, _ map[string]string) error {
				return cmdCache(ctx, w, cfgPath, false)
			}},
		{Name: "cache-clear", Label: "Clear caches", Help: "Empty the shared caches no running runner is using.",
			Confirm: "Empty the shared runner caches? The next jobs download their tools again.",
			Run: func(ctx context.Context, w io.Writer, _ map[string]string) error {
				return cmdCache(ctx, w, cfgPath, true)
			}},
		{Name: "config-show", Label: "Show config file", Help: "The file as saved; secrets are never in it.",
			Run: func(_ context.Context, w io.Writer, _ map[string]string) error { return cmdConfig(w, cfgPath, true) }},
		{Name: "template-prepare", Label: "Prepare a template", Help: "Install the runner into a container and make it a template.",
			Args: []string{"ctid"}, Confirm: "Turn this container into a runner template?",
			Run: func(ctx context.Context, w io.Writer, a map[string]string) error {
				id, err := strconv.Atoi(a["ctid"])
				if err != nil {
					return fmt.Errorf("ctid %q is not a container ID", a["ctid"])
				}
				return cmdTemplatePrepare(ctx, w, cfgPath, id)
			}},
	}
}

func firstOr(xs []string, def string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return def
}

// nodeCert is the certificate the Proxmox UI serves: a custom or ACME one when set, else the node's own.
func nodeCert() (tls.Certificate, error) {
	for _, base := range []string{"/etc/pve/local/pveproxy-ssl", "/etc/pve/local/pve-ssl"} {
		if c, err := tls.LoadX509KeyPair(base+".pem", base+".key"); err == nil {
			return c, nil
		}
	}
	return tls.Certificate{}, errors.New("no Proxmox node certificate in /etc/pve/local")
}

// restartSoon restarts the service after a save, once the "saved" page has reached the browser.
func restartSoon() {
	go func() {
		time.Sleep(time.Second)
		out, err := exec.Command("systemctl", "restart", "--no-block", "pve-runners.service").CombinedOutput()
		// The restart stops this service's whole cgroup, systemctl included, often before it exits; being
		// terminated that way means the restart is underway, not that it failed.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM {
				return
			}
		}
		if err != nil {
			slog.Error("restart after saving settings", "err", err, "output", strings.TrimSpace(string(out)))
		}
	}()
}

// appClient builds the App client for a target from its own key file.
func appClient(cfg *config.Config, t config.Target) (*github.Client, error) {
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

func githubFor(cfg *config.Config) func(config.Target) (provision.JITSource, error) {
	return func(t config.Target) (provision.JITSource, error) { return appClient(cfg, t) }
}

// withStorage checks proxmox.storage can hold containers, or picks one when it is empty, so a bad
// setting fails here naming the storages that would work instead of deep inside pct. It returns a
// copy: the image sync and the autoscaler resolve concurrently from the same loaded config.
func withStorage(ctx context.Context, cfg *config.Config) (*config.Config, error) {
	s, err := host(cfg).RootfsStorage(ctx, cfg.Proxmox.Storage)
	if err != nil {
		return nil, err
	}
	c := *cfg
	c.Proxmox.Storage = s
	return &c, nil
}

func imageManager(ctx context.Context, cfg *config.Config) (*images.Manager, error) {
	cfg, err := withStorage(ctx, cfg)
	if err != nil {
		return nil, err
	}
	h := host(cfg)
	probe, err := h.VolumePath(ctx, cfg.Images.CacheStorage+":vztmpl/probe.tar.zst")
	if err != nil {
		return nil, fmt.Errorf("images.cache_storage %q: %w", cfg.Images.CacheStorage, err)
	}
	return &images.Manager{Repo: cfg.Images.Repo, APIBase: github.DefaultAPI, HTTP: &http.Client{Timeout: 30 * time.Minute},
		PublicKey: buildinfo.UpdatePublicKey, GOARCH: runtime.GOARCH, Host: h, Range: cfg.Images.VMIDRange,
		CacheDir: filepath.Dir(probe), CacheStorage: cfg.Images.CacheStorage, Storage: cfg.Proxmox.Storage,
		Pool: cfg.Proxmox.Pool, Bridge: cfg.Proxmox.Bridge, RootfsGB: cfg.Images.RootfsGB,
		Retention: time.Duration(cfg.Images.RetentionDays) * 24 * time.Hour, Now: time.Now}, nil
}

// imageRefs lists the distinct images the pools use, in a stable order.
func imageRefs(cfg *config.Config) []images.Ref {
	var refs []images.Ref
	seen := map[string]bool{}
	for _, n := range cfg.PoolNames() {
		if img := cfg.Pools[n].Image; img != "" && !seen[img] {
			seen[img] = true
			refs = append(refs, images.ParseRef(img))
		}
	}
	return refs
}

func provisioner(ctx context.Context, cfg *config.Config) (*provision.Provisioner, error) {
	cfg, err := withStorage(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &provision.Provisioner{Cfg: cfg, Host: host(cfg), GitHub: githubFor(cfg), Now: time.Now, TmpDir: os.TempDir()}
	if len(imageRefs(cfg)) > 0 {
		m, err := imageManager(ctx, cfg)
		if err != nil {
			return nil, err
		}
		p.Templates = func(ctx context.Context, image string) (int, error) { return m.Ensure(ctx, images.ParseRef(image)) }
	}
	return p, nil
}

// syncImages makes sure every pool's image has a template (pull), then ages out old templates and
// archives (prune).
func syncImages(ctx context.Context, w io.Writer, cfg *config.Config, m *images.Manager, pull, prune bool) error {
	var errs []error
	refs := imageRefs(cfg)
	for _, r := range refs {
		if !pull {
			break
		}
		id, err := m.Ensure(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("image %s: %w", r, err))
			continue
		}
		slog.Info("image ready", "image", r.String(), "template", id)
		fmt.Fprintf(w, "image %s ready as template %d\n", r, id)
	}
	if !prune {
		return errors.Join(errs...)
	}
	runners, err := host(cfg).Runners(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	removed, err := m.Prune(ctx, refs, images.InUse(runners))
	for _, r := range removed {
		slog.Info("image pruned", "what", r.What, "reason", r.Reason)
		fmt.Fprintf(w, "pruned %s (%s)\n", r.What, r.Reason)
	}
	return errors.Join(append(errs, err)...)
}

func cmdImages(ctx context.Context, out io.Writer, cfgPath, sub string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	m, err := imageManager(ctx, cfg)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		tpls, err := m.Host.Templates(ctx, m.Range)
		if err != nil {
			return err
		}
		sort.Slice(tpls, func(i, j int) bool { return tpls[i].VMID < tpls[j].VMID })
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tIMAGE\tRELEASE\tCREATED")
		for _, g := range tpls {
			when := "incomplete"
			var sec int64
			if _, err := fmt.Sscan(g.TagValue("created"), &sec); err == nil && g.Template == 1 {
				when = time.Unix(sec, 0).Format(time.DateOnly)
			}
			fmt.Fprintf(w, "%d\t%s\t%s%s\t%s\n", g.VMID, g.TagValue("image"), images.TagPrefix, g.TagValue("rel"), when)
		}
		return w.Flush()
	case "pull", "prune":
		return syncImages(ctx, out, cfg, m, sub == "pull", sub == "prune")
	}
	return fmt.Errorf("unknown images command %q; use list, pull or prune", sub)
}

// nodeName is this host's Proxmox node name, which Proxmox takes from the short hostname.
func nodeName() string {
	h, _ := os.Hostname()
	return strings.SplitN(h, ".", 2)[0]
}

func cmdSetup(ctx context.Context, cfgPath, listen string, openFirewall bool) error {
	// A missing or empty config starts from the defaults; re-running setup adds to an existing one.
	if _, err := config.Init(cfgPath, nodeName()); err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Proxmox.Node == "" {
		cfg.Proxmox.Node = nodeName()
	}
	s, err := setup.NewServer(cfg, cfgPath)
	if err != nil {
		return err
	}
	if list, err := host(cfg).RootdirStorages(ctx); err == nil {
		for _, st := range list {
			s.Storages = append(s.Storages, st.Name)
		}
	}
	ln, err := net.Listen("tcp", listen)
	if errors.Is(err, syscall.EADDRINUSE) && cfg.Web.On() {
		return fmt.Errorf("%w: the service already serves its settings page there; open https://<this host>%s and log in with your Proxmox account", err, listen)
	}
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port

	h := host(cfg)
	inputBlocked, _ := h.HostInputBlocked(ctx)
	if openFirewall {
		// Clear a rule left by a run that was killed, then open the port for this run only.
		_ = h.CloseSetupPort(ctx)
		closeFn, err := h.OpenSetupPort(ctx, port)
		if err != nil {
			return fmt.Errorf("open port %d in the Proxmox firewall: %w", port, err)
		}
		defer func() {
			if err := closeFn(context.Background()); err != nil {
				fmt.Fprintf(os.Stderr, "remove the temporary firewall rule by hand (comment %q): %v\n", pve.SetupRuleComment, err)
			}
		}()
	}

	ips := []string{addr.IP.String()}
	if addr.IP.IsUnspecified() {
		ips = setup.OrderAddrs(hostIPs(), preferredIPs())
	}
	link := func(ip string) string {
		return fmt.Sprintf("http://%s/?t=%s", net.JoinHostPort(ip, strconv.Itoa(port)), s.Token)
	}
	fmt.Printf("Open the setup page in a browser (it listens on port %d):\n", port)
	note := ""
	if pref := preferredIPs(); len(pref) > 0 && pref[0] == ips[0] {
		note = "   <- recommended: the address the Proxmox web UI is on"
	}
	fmt.Printf("  %s%s\n", link(ips[0]), note)
	if len(ips) > 1 {
		fmt.Println("Other addresses of this host, only if that one doesn't load from your computer:")
		for _, ip := range ips[1:] {
			fmt.Printf("  %s\n", link(ip))
		}
	}
	switch {
	case openFirewall:
		fmt.Printf("Port %d is allowed through the Proxmox firewall until setup ends.\n", port)
	case inputBlocked:
		fmt.Printf("The Proxmox firewall drops inbound connections (input policy DROP), so port %d is blocked. Either:\n", port)
		fmt.Printf("  - stop this (Ctrl+C) and run: pve-runners setup --open-firewall   (allows the port until setup ends), or\n")
		fmt.Printf("  - from your computer run: ssh -L %d:127.0.0.1:%d root@%s   then open %s\n", port, port, ips[0], link("127.0.0.1"))
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

// preferredIPs are the addresses most likely to be reachable from the owner's computer: the one the
// node name resolves to (Proxmox requires that to be the management address the web UI is on),
// then the source address of the default route.
func preferredIPs() []string {
	var out []string
	if addrs, err := net.LookupHost(nodeName()); err == nil {
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				out = append(out, a)
			}
		}
	}
	// A UDP "connection" sends nothing; it only asks the kernel which source address it would use.
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		out = append(out, c.LocalAddr().(*net.UDPAddr).IP.String())
		c.Close()
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

func cmdReconcile(ctx context.Context, w io.Writer, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	done, err := reconcile.Once(ctx, cfg, host(cfg), time.Now())
	for _, a := range done {
		fmt.Fprintf(w, "destroyed %d: %s\n", a.VMID, a.Reason)
	}
	if err == nil && len(done) == 0 {
		fmt.Fprintln(w, "nothing to clean up")
	}
	return err
}

func cmdLogsShip(ctx context.Context, w io.Writer, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	sh, err := logShipper(cfg)
	if err != nil {
		return err
	}
	if sh == nil {
		return errors.New("logs.url and logs.token_file are not set in the config")
	}
	if err := sh.Once(ctx); err != nil {
		return err
	}
	fmt.Fprintln(w, "logs shipped to", cfg.Logs.URL)
	return nil
}

func cmdCache(ctx context.Context, w io.Writer, cfgPath string, clear bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	mode := cache.List
	if clear {
		mode = cache.Clear
	}
	res, err := cache.Run(ctx, cfg, host(cfg), mode)
	if len(res) == 0 && err == nil {
		fmt.Fprintln(w, "no pool has cache: true")
	}
	for _, r := range res {
		line := fmt.Sprintf("%s  %.1f GB  %s", r.Owner, float64(r.Bytes)/(1<<30), r.Dir)
		switch {
		case r.Cleared:
			line += "  emptied"
		case r.Skipped != "":
			line += "  left alone: " + r.Skipped
		}
		fmt.Fprintln(w, line)
	}
	return err
}

// cmdConfig validates the config file, and with show prints it as it is on disk.
func cmdConfig(w io.Writer, cfgPath string, show bool) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	cfg, _, err := config.Parse(data)
	if err != nil {
		return err
	}
	if show {
		_, err = w.Write(data)
		return err
	}
	fmt.Fprintf(w, "%s is valid: %d targets, %d pools\n", cfgPath, len(cfg.Targets), len(cfg.Pools))
	return nil
}

func cmdSpawn(ctx context.Context, w io.Writer, cfgPath, pool, owner, repo string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	t, err := findTarget(cfg, owner)
	if err != nil {
		return err
	}
	p, err := provisioner(ctx, cfg)
	if err != nil {
		return err
	}
	res, err := p.Spawn(ctx, provision.Request{Pool: pool, Target: t, Repo: repo})
	if err != nil {
		notifier(cfg).Send(ctx, notify.SpawnFailed, fmt.Sprintf("pool %s for %s: %v", pool, t.Owner(), err))
		return err
	}
	fmt.Fprintf(w, "runner %s started in container %d; it will take one job from %s and then be destroyed\n", res.RunnerName, res.VMID, t.Owner())
	return nil
}

func cmdStatus(ctx context.Context, out io.Writer, cfgPath string) error {
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
	fmt.Fprintf(out, "pve-runners %s on %s: %d/%d runners\n", buildinfo.Version, cfg.HostID, len(runners), cfg.Proxmox.MaxRunners)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
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
		fmt.Fprintf(out, "pending cleanup: %d (%s)\n", a.VMID, a.Reason)
	}
	if st, err := h.RootfsStorage(ctx, cfg.Proxmox.Storage); err != nil {
		fmt.Fprintln(out, "warning:", err)
	} else {
		fmt.Fprintln(out, "container storage:", st)
	}
	if on, err := h.ClusterFirewallEnabled(ctx); err == nil && !on && cfg.Network.Isolated() {
		fmt.Fprintln(out, "warning: the datacenter firewall is off; runners cannot be isolated from the LAN, so spawning is refused")
	}
	return nil
}

func cmdTemplatePrepare(ctx context.Context, w io.Writer, cfgPath string, ctid int) error {
	if ctid == 0 {
		return errors.New("--ctid is required")
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		cfg = config.Initial(nodeName()) // template prep can run before setup
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
	fmt.Fprintf(w, "container %d is now a runner template; use it as the pool template in setup\n", ctid)
	return nil
}

func cmdUpdate(ctx context.Context, w io.Writer, cfg *config.Config, install bool) error {
	u := updater(cfg)
	rel, err := u.Latest(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		fmt.Fprintf(w, "pve-runners %s is current\n", buildinfo.Version)
		return nil
	}
	if !install {
		fmt.Fprintf(w, "%s is available (running %s); run `pve-runners update --now` to install\n", rel.Tag, buildinfo.Version)
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
	logsTick := time.NewTicker(cfg.Logs.Interval)
	defer logsTick.Stop()
	updateTick := time.NewTicker(cfg.Update.Interval)
	imageTick := time.NewTicker(cfg.Images.CheckInterval)
	defer reconcileTick.Stop()
	defer updateTick.Stop()
	defer imageTick.Stop()

	// Image downloads can take minutes, so they run beside the loop, one sync at a time. The first
	// one starts now so a new image is ready before the first job needs it.
	var imageBusy atomic.Bool
	syncImagesAsync := func(cfg *config.Config) {
		if len(imageRefs(cfg)) == 0 || !imageBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer imageBusy.Store(false)
			m, err := imageManager(ctx, cfg)
			if err == nil {
				err = syncImages(ctx, io.Discard, cfg, m, true, true)
			}
			if err != nil && ctx.Err() == nil {
				slog.Error("images", "err", err)
				n.Send(ctx, notify.ImageFailed, err.Error())
			}
		}()
	}
	syncImagesAsync(cfg)

	// Measuring a big cache folder takes a while, so it runs beside the loop too.
	var cacheBusy atomic.Bool
	trimCaches := func(cfg *config.Config) {
		if len(cache.Owners(cfg)) == 0 || !cacheBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer cacheBusy.Store(false)
			res, err := cache.Run(ctx, cfg, host(cfg), cache.Trim)
			for _, r := range res {
				if r.Cleared {
					slog.Info("emptied runner cache over cache.max_gb", "owner", r.Owner, "bytes", r.Bytes)
				} else if r.Skipped != "" {
					slog.Warn("runner cache over cache.max_gb", "owner", r.Owner, "bytes", r.Bytes, "left_alone", r.Skipped)
				}
			}
			if err != nil && ctx.Err() == nil {
				slog.Error("runner cache", "err", err)
			}
		}()
	}

	// Shipping runs beside the loop so a slow log endpoint never delays reconcile. Repeated failures
	// are logged once per distinct error: each logged line would itself be shipped next round.
	var shipper *logship.Shipper
	var logsBusy atomic.Bool
	var lastShipErr string
	setShipper := func(cfg *config.Config) {
		var err error
		if shipper, err = logShipper(cfg); err != nil {
			slog.Error("log shipping disabled", "err", err)
		}
		logsTick.Reset(cfg.Logs.Interval)
	}
	shipLogs := func() {
		sh := shipper
		if sh == nil || !logsBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer logsBusy.Store(false)
			msg := ""
			if err := sh.Once(ctx); err != nil && ctx.Err() == nil {
				msg = err.Error()
			}
			if msg != lastShipErr {
				if msg != "" {
					slog.Error("log shipping", "err", msg)
				} else {
					slog.Info("log shipping recovered")
				}
				lastShipErr = msg
			}
		}()
	}
	setShipper(cfg)
	stopWeb := startWeb(ctx, cfg, cfgPath, &imageBusy)
	defer func() { stopWeb() }()

	// Listeners that failed to start (say, a missing key file) are retried on the reconcile tick.
	var scaler *autoscaler
	startScaler := func() {
		if scaler != nil {
			return
		}
		var err error
		if scaler, err = startAutoscale(ctx, cfg, n); err != nil {
			slog.Error("autoscale", "err", err)
		}
	}
	startScaler()
	defer func() { scaler.stop() }()
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
			setShipper(cfg)
			stopWeb()
			stopWeb = startWeb(ctx, cfg, cfgPath, &imageBusy)
			syncImagesAsync(cfg)
			scaler.stop()
			scaler = nil
			startScaler()
		case <-imageTick.C:
			syncImagesAsync(cfg)
			trimCaches(cfg)
		case <-logsTick.C:
			shipLogs()
		case <-reconcileTick.C:
			startScaler()
			done, err := reconcile.Once(ctx, cfg, host(cfg), time.Now())
			for _, a := range done {
				slog.Info("destroyed runner", "vmid", a.VMID, "reason", a.Reason)
			}
			if err != nil {
				slog.Error("reconcile", "err", err)
			}
		case <-updateTick.C:
			// A restart mid-download would only waste the download, but there's no reason to.
			if !cfg.Update.Enabled || u.Pending() || provision.Busy() || imageBusy.Load() {
				continue
			}
			if err := cmdUpdate(ctx, io.Discard, cfg, true); err != nil {
				slog.Error("self-update", "err", err)
				n.Send(ctx, notify.UpdateFailed, err.Error())
			}
		}
	}
}
