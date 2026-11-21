// Package docker runs runners as containers on a second machine with Docker and Sysbox (a NAS, say),
// which the service on the Proxmox host drives over SSH. Each runner is one Sysbox container booting
// the published runner image's systemd, so the runner unit, its one-job lifecycle and the phase checks
// are the same as in an LXC guest, and jobs can run Docker inside it without privileges on the machine.
//
// Every docker command is built by a function here and run through Runner, so tests assert the exact
// argv and never touch a real machine. Runner containers are presented as pve.Guest values (ID from
// docker.id_range, tags from a label), so reconcile and the autoscaler treat them like LXC runners.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// Runner runs one command on the Docker machine and returns its stdout; stdin may be nil.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

// Labels a runner container carries. Docker labels can't change after create, so the tags (the
// same ones an LXC runner gets) are fixed then.
const (
	IDLabel   = "pve-runners.id"
	TagsLabel = "pve-runners.tags"
)

// Network is the bridge network runners attach to, and Bridge its Linux interface, which the
// isolation rules match on.
const (
	Network = "gh-runners"
	Bridge  = "ghr0"
)

// Runtime is Sysbox's OCI runtime: systemd as PID 1 and Docker inside, with no privileges on the host.
const Runtime = "sysbox-runc"

// ImageRepo is the local repository published runner images are imported as.
const ImageRepo = "pve-runners/"

// ListCmd prints one line per runner container: ID, name, state, tags.
func ListCmd() []string {
	return []string{"docker", "ps", "-a", "--no-trunc", "--filter", "label=" + IDLabel, "--format",
		`{{.Label "` + IDLabel + `"}}	{{.Names}}	{{.State}}	{{.Label "` + TagsLabel + `"}}`}
}

// Spec is what a runner container is created with.
type Spec struct {
	ID       int
	Name     string
	Image    string
	Cores    int
	MemoryMB int
	Tags     []string
	DNS      []string
}

// RunCmd creates and starts a runner container. The image's entrypoint is systemd (ImportCmd); the
// runner unit starts only once its config is pushed. Nothing secret goes on this command line.
func RunCmd(s Spec) []string {
	args := []string{"docker", "run", "-d", "--name", s.Name, "--hostname", s.Name, "--runtime", Runtime,
		"--label", IDLabel + "=" + strconv.Itoa(s.ID), "--label", TagsLabel + "=" + strings.Join(s.Tags, ";"),
		"--cpus", strconv.Itoa(s.Cores), "--memory", strconv.Itoa(s.MemoryMB) + "m",
		"--network", Network, "--restart", "no"}
	for _, d := range s.DNS {
		args = append(args, "--dns", d)
	}
	return append(args, s.Image)
}

// ExecCmd runs a command inside a runner container.
func ExecCmd(name string, command ...string) []string {
	return append([]string{"docker", "exec", name}, command...)
}

// PushCmd writes stdin to dst inside a container, so a secret never appears in a command line.
func PushCmd(name, dst, perms string) []string {
	// -i: without it docker exec gives the command no stdin and writes an empty file.
	return []string{"docker", "exec", "-i", name, "sh", "-c",
		`umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1" && chmod "$2" "$1"`, "sh", dst, perms}
}

// RemoveCmd stops and removes a container.
func RemoveCmd(name string) []string { return []string{"docker", "rm", "-f", name} }

// ImageExistsCmd fails unless the image is on the machine.
func ImageExistsCmd(tag string) []string {
	return []string{"docker", "image", "inspect", "--format", "{{.Id}}", tag}
}

// ImportCmd turns a runner image archive (an uncompressed rootfs tar on stdin) into a Docker image
// that boots systemd.
func ImportCmd(tag string) []string {
	return []string{"docker", "import", "--change", `ENTRYPOINT ["/sbin/init"]`, "--change",
		"STOPSIGNAL SIGRTMIN+3", "-", tag}
}

// ImageTagsCmd lists the tags of an image repository, newest first (Docker's order).
func ImageTagsCmd(repo string) []string {
	return []string{"docker", "image", "ls", "--format", "{{.Tag}}", repo}
}

func RemoveImageCmd(tag string) []string { return []string{"docker", "image", "rm", tag} }

// NetworkBridgeCmd prints the Linux bridge behind the runner network; it fails if there's none.
func NetworkBridgeCmd() []string {
	return []string{"docker", "network", "inspect", "--format",
		`{{index .Options "com.docker.network.bridge.name"}}`, Network}
}

// NetworkCreateCmd creates the runner network on a fixed bridge name, with runners unable to reach
// each other.
func NetworkCreateCmd() []string {
	return []string{"docker", "network", "create", "--driver", "bridge",
		"--opt", "com.docker.network.bridge.name=" + Bridge,
		"--opt", "com.docker.network.bridge.enable_icc=false",
		"--label", "pve-runners=1", Network}
}

// RuntimesCmd prints the runtimes the machine's Docker knows.
func RuntimesCmd() []string {
	return []string{"docker", "info", "--format", "{{range $k, $v := .Runtimes}}{{$k}} {{end}}"}
}

// Host wraps a Runner with the safety rules every mutation must pass.
type Host struct {
	R       Runner
	IDRange [2]int
}

func (h *Host) run(ctx context.Context, stdin io.Reader, args []string) ([]byte, error) {
	return h.R.Run(ctx, stdin, args...)
}

func (h *Host) InRange(id int) bool { return id >= h.IDRange[0] && id <= h.IDRange[1] }

// Owned is the hard check before removing a container: an ID in docker.id_range AND the gh-runner tag.
func (h *Host) Owned(g pve.Guest) bool { return h.InRange(g.VMID) && g.HasTag(pve.Tag) }

// containers lists every container with an ID label, owned or not.
func (h *Host) containers(ctx context.Context) ([]pve.Guest, error) {
	out, err := h.run(ctx, nil, ListCmd())
	if err != nil {
		return nil, err
	}
	return ParseList(out), nil
}

// ParseList reads ListCmd's output. A container that has exited counts as stopped, like a guest that
// powered off after its job.
func ParseList(out []byte) []pve.Guest {
	var gs []pve.Guest
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		status := "stopped"
		if f[2] == "running" || f[2] == "restarting" || f[2] == "paused" {
			status = "running"
		}
		gs = append(gs, pve.Guest{VMID: id, Name: f[1], Type: "docker", Status: status, RawTags: f[3]})
	}
	return gs
}

// Runners returns the runner containers this tool owns.
func (h *Host) Runners(ctx context.Context) ([]pve.Guest, error) {
	gs, err := h.containers(ctx)
	if err != nil {
		return nil, err
	}
	var out []pve.Guest
	for _, g := range gs {
		if h.Owned(g) {
			out = append(out, g)
		}
	}
	return out, nil
}

// NextFreeID picks the lowest ID in docker.id_range no container uses.
func (h *Host) NextFreeID(ctx context.Context) (int, error) {
	gs, err := h.containers(ctx)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, g := range gs {
		used[g.VMID] = true
	}
	for id := h.IDRange[0]; id <= h.IDRange[1]; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free runner ID left in docker.id_range %d-%d", h.IDRange[0], h.IDRange[1])
}

// Destroy removes an owned runner container. It re-reads the list first so a stale view can never
// remove something that has since been reused.
func (h *Host) Destroy(ctx context.Context, id int) error {
	gs, err := h.containers(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.VMID != id {
			continue
		}
		if !h.Owned(g) {
			return fmt.Errorf("%w: %d (%s)", pve.ErrNotOwned, id, g.Name)
		}
		_, err := h.run(ctx, nil, RemoveCmd(g.Name))
		return err
	}
	return nil // already gone
}

func (h *Host) name(ctx context.Context, id int) (string, error) {
	gs, err := h.Runners(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range gs {
		if g.VMID == id {
			return g.Name, nil
		}
	}
	return "", fmt.Errorf("no runner container %d", id)
}

// RunnerUnitState returns the runner unit's state inside a container (see pve.RunnerUnitStateCmd).
func (h *Host) RunnerUnitState(ctx context.Context, id int) (string, error) {
	n, err := h.name(ctx, id)
	if err != nil {
		return "", err
	}
	return pve.ParseUnitState(h.run(ctx, nil, ExecCmd(n, "systemctl", "is-active", "gh-runner.service")))
}

// RunnerPhase reports how far a container's runner has got (see pve.RunnerPhaseCmd).
func (h *Host) RunnerPhase(ctx context.Context, id int) (string, error) {
	n, err := h.name(ctx, id)
	if err != nil {
		return pve.PhaseStarting, err
	}
	out, err := h.run(ctx, nil, ExecCmd(n, "sh", "-c", pve.PhaseScript))
	if err != nil {
		return pve.PhaseStarting, err
	}
	return pve.ParsePhase(out), nil
}

// Label does nothing: Docker labels are fixed when a container is created, and the job a runner took
// is in the service's log.
func (h *Host) Label(context.Context, int, []string, string) error { return nil }

// Create starts a runner container.
func (h *Host) Create(ctx context.Context, s Spec) error {
	_, err := h.run(ctx, nil, RunCmd(s))
	return err
}

// Exec runs a command in a runner container.
func (h *Host) Exec(ctx context.Context, name string, command ...string) error {
	_, err := h.run(ctx, nil, ExecCmd(name, command...))
	return err
}

// Push writes content to dst inside a runner container through stdin.
func (h *Host) Push(ctx context.Context, name, content, dst, perms string) error {
	_, err := h.run(ctx, strings.NewReader(content), PushCmd(name, dst, perms))
	return err
}

// ErrNoSysbox refuses to start runners on a machine without the Sysbox runtime: without it a runner
// can't boot systemd, and the only way to give jobs Docker would be a privileged container.
var ErrNoSysbox = errors.New("Docker on the machine has no " + Runtime + " runtime; install Sysbox there " +
	"(see \"pve-runners docker setup\")")

// CheckRuntime makes sure the machine's Docker has Sysbox.
func (h *Host) CheckRuntime(ctx context.Context) error {
	out, err := h.run(ctx, nil, RuntimesCmd())
	if err != nil {
		return err
	}
	for _, r := range strings.Fields(string(out)) {
		if r == Runtime {
			return nil
		}
	}
	return ErrNoSysbox
}

// EnsureNetwork creates the runner network unless it exists, and refuses one on another bridge name,
// since the isolation rules match the bridge.
func (h *Host) EnsureNetwork(ctx context.Context) error {
	out, err := h.run(ctx, nil, NetworkBridgeCmd())
	if err != nil {
		if msg := err.Error(); !strings.Contains(msg, "not found") && !strings.Contains(msg, "No such network") {
			return err
		}
		if _, err := h.run(ctx, nil, NetworkCreateCmd()); err != nil {
			return fmt.Errorf("create docker network %s: %w", Network, err)
		}
		return nil
	}
	if b := strings.TrimSpace(string(out)); b != Bridge {
		return fmt.Errorf("docker network %s exists on bridge %q, not %s; remove it (docker network rm %s) so it can be "+
			"made again", Network, b, Bridge, Network)
	}
	return nil
}

// HasImage reports whether an image is on the machine.
func (h *Host) HasImage(ctx context.Context, tag string) bool {
	_, err := h.run(ctx, nil, ImageExistsCmd(tag))
	return err == nil
}

// Import loads an uncompressed rootfs tar as a runner image.
func (h *Host) Import(ctx context.Context, tag string, tar io.Reader) error {
	_, err := h.run(ctx, tar, ImportCmd(tag))
	return err
}

// ImageTags lists the tags of a repository, newest first.
func (h *Host) ImageTags(ctx context.Context, repo string) ([]string, error) {
	out, err := h.run(ctx, nil, ImageTagsCmd(repo))
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, t := range strings.Fields(string(out)) {
		if t != "<none>" {
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// RemoveImage deletes an image; Docker refuses one a container still uses.
func (h *Host) RemoveImage(ctx context.Context, tag string) error {
	_, err := h.run(ctx, nil, RemoveImageCmd(tag))
	return err
}
