package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
	"github.com/ServerTooling/proxmox-github-runners/internal/reconcile"
)

// dockerHost is the Docker machine of docker pools, or nil without docker.ssh.
func dockerHost(cfg *config.Config) *docker.Host {
	if !cfg.Docker.Enabled() {
		return nil
	}
	return &docker.Host{IDRange: cfg.Docker.IDRange, R: docker.SSH{Target: cfg.Docker.SSH, Port: cfg.Docker.Port,
		KeyFile: cfg.Docker.KeyFile, KnownHosts: cfg.Docker.KnownHosts, ControlPath: "/run/pve-runners-ssh-%C"}}
}

// reconcileAll cleans up runners on this host and on the Docker machine. One that can't be reached
// doesn't hold up the other.
func reconcileAll(ctx context.Context, cfg *config.Config) ([]reconcile.Action, error) {
	done, err := reconcile.Once(ctx, cfg, host(cfg), time.Now())
	if d := dockerHost(cfg); d != nil {
		ddone, derr := reconcile.Once(ctx, cfg, d, time.Now())
		done = append(done, ddone...)
		if derr != nil {
			err = errors.Join(err, fmt.Errorf("docker machine: %w", derr))
		}
	}
	return done, err
}

// Sysbox release the machine script installs; the checksum is the one its release lists.
const (
	sysboxURL    = "https://github.com/nestybox/sysbox/releases/download/v0.7.1/sysbox-ce_0.7.1.linux_amd64.deb"
	sysboxSHA256 = "9d6d5484f980d0a17f86c492c1262015c2afb66280bdb97215b79fde6a0261c5"
)

//go:embed docker-machine.sh
var machineScript string

var machineTmpl = template.Must(template.New("machine").Funcs(template.FuncMap{"q": docker.Quote}).Parse(machineScript))

// MachineScript is what to run as root on the Docker machine: Sysbox, and a user that only this
// host's key can log in as.
func MachineScript(user, pubKey string) (string, error) {
	var b strings.Builder
	err := machineTmpl.Execute(&b, map[string]string{"User": user, "Key": strings.TrimSpace(pubKey),
		"SysboxURL": sysboxURL, "SysboxSHA256": sysboxSHA256})
	return b.String(), err
}

// cmdDocker: "setup" makes this host's SSH key, pins the machine's host key and prints the script
// for the machine; "check" tests everything a docker spawn needs.
func cmdDocker(ctx context.Context, w io.Writer, cfgPath, sub string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if !cfg.Docker.Enabled() {
		return errors.New("set docker.ssh (such as runner@192.0.2.20) in the settings first")
	}
	user, addr, _ := strings.Cut(cfg.Docker.SSH, "@")
	switch sub {
	case "setup":
		if _, err := os.Stat(cfg.Docker.KeyFile); errors.Is(err, os.ErrNotExist) {
			if out, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C",
				"pve-runners@"+cfg.HostID, "-f", cfg.Docker.KeyFile).CombinedOutput(); err != nil {
				return fmt.Errorf("ssh-keygen: %w: %s", err, out)
			}
		}
		pub, err := os.ReadFile(cfg.Docker.KeyFile + ".pub")
		if err != nil {
			return err
		}
		if _, err := os.Stat(cfg.Docker.KnownHosts); errors.Is(err, os.ErrNotExist) {
			// Trust on first use, like the Proxmox certificate: the key is pinned from now on.
			keys, err := exec.CommandContext(ctx, "ssh-keyscan", "-T", "10", "-p", fmt.Sprint(cfg.Docker.Port),
				"-t", "ed25519,ecdsa,rsa", "--", addr).Output()
			if err != nil || len(keys) == 0 {
				return fmt.Errorf("could not read the SSH host key of %s (is SSH on there?): %v", addr, err)
			}
			if err := os.WriteFile(cfg.Docker.KnownHosts, keys, 0o600); err != nil {
				return err
			}
		}
		fp, _ := exec.CommandContext(ctx, "ssh-keygen", "-l", "-f", cfg.Docker.KnownHosts).Output()
		script, err := MachineScript(user, string(pub))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Pinned host key of %s:\n%s\n", addr, fp)
		fmt.Fprintf(w, "On %s, stop the containers the Docker app runs, log in over SSH as an admin, run \"sudo -i\", "+
			"paste the script below, then start the containers again and run the \"Check Docker machine\" action "+
			"(pve-runners docker check).\n\n%s", addr, script)
		return nil
	case "check":
		d := dockerHost(cfg)
		out, err := d.R.Run(ctx, nil, "docker", "version", "--format", "{{.Server.Version}}")
		if err != nil {
			return fmt.Errorf("docker on %s: %w", addr, err)
		}
		fmt.Fprintf(w, "ssh to %s works; Docker %s\n", cfg.Docker.SSH, strings.TrimSpace(string(out)))
		if err := d.CheckRuntime(ctx); err != nil {
			return err
		}
		fmt.Fprintln(w, "Sysbox runtime found")
		if err := d.EnsureNetwork(ctx); err != nil {
			return err
		}
		fmt.Fprintf(w, "runner network %s (bridge %s) ready\n", docker.Network, docker.Bridge)
		rs, err := d.Runners(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%d runner container(s) now; max %d\n", len(rs), cfg.Docker.MaxRunners)
		return nil
	}
	return fmt.Errorf("unknown docker command %q; use setup or check", sub)
}
