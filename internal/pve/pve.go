// Package pve drives Proxmox guests through the host's own `pct`, `qm` and `pvesh` commands.
//
// The service runs as root on the Proxmox host, so the CLI is available and covers what the REST API
// lacks (pct push/exec). Every command is built by a function here and run through Runner, so tests
// assert the exact argv and never touch a real host.
package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Tag marks every guest this tool owns. Guests without it are never modified.
const Tag = "gh-runner"

// Runner executes one host command and returns its stdout.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs real commands on the host.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Cmd is a command line: program plus arguments.
type Cmd struct {
	Name string
	Args []string
}

func (c Cmd) String() string { return c.Name + " " + strings.Join(c.Args, " ") }

// LXCClone builds `pct clone`. Linked clones (full=false) need snapshot-capable storage and are the
// fast path; --storage is only valid for full clones.
func LXCClone(template, newID int, hostname, pool, storage string, full bool) Cmd {
	args := []string{"clone", strconv.Itoa(template), strconv.Itoa(newID), "--hostname", hostname, "--pool", pool}
	if full {
		args = append(args, "--full", "1", "--storage", storage)
	}
	return Cmd{"pct", args}
}

// LXCSpec is what a runner container gets configured with after cloning.
type LXCSpec struct {
	Cores       int
	MemoryMB    int
	Tags        []string
	Description string
	Bridge      string
	VLAN        int
	// Nameservers replaces the DNS servers the guest would otherwise copy from the host.
	Nameservers []string
}

func LXCSet(id int, s LXCSpec) Cmd {
	args := []string{"set", strconv.Itoa(id),
		"--cores", strconv.Itoa(s.Cores),
		"--memory", strconv.Itoa(s.MemoryMB),
		"--tags", strings.Join(s.Tags, ";"),
		"--onboot", "0",
	}
	if s.Description != "" {
		args = append(args, "--description", s.Description)
	}
	if s.Bridge != "" {
		net := "name=eth0,bridge=" + s.Bridge + ",ip=dhcp,firewall=1"
		if s.VLAN > 0 {
			net += ",tag=" + strconv.Itoa(s.VLAN)
		}
		args = append(args, "--net0", net)
	}
	if len(s.Nameservers) > 0 {
		args = append(args, "--nameserver", strings.Join(s.Nameservers, " "))
	}
	return Cmd{"pct", args}
}

// LXCResize grows the root disk to an absolute size. Proxmox never shrinks disks, so disk_gb must be
// at least the template's own root disk size or the spawn fails (and the clone is cleaned up).
func LXCResize(id, sizeGB int) Cmd {
	return Cmd{"pct", []string{"resize", strconv.Itoa(id), "rootfs", strconv.Itoa(sizeGB) + "G"}}
}

func LXCStart(id int) Cmd { return Cmd{"pct", []string{"start", strconv.Itoa(id)}} }

func LXCStop(id int) Cmd { return Cmd{"pct", []string{"stop", strconv.Itoa(id)}} }

// LXCDestroy also purges the guest from pools/backup jobs so nothing dangles.
func LXCDestroy(id int) Cmd {
	return Cmd{"pct", []string{"destroy", strconv.Itoa(id), "--purge", "1", "--force", "1"}}
}

func LXCPush(id int, src, dst, perms string) Cmd {
	return Cmd{"pct", []string{"push", strconv.Itoa(id), src, dst, "--perms", perms}}
}

func LXCExec(id int, command ...string) Cmd {
	return Cmd{"pct", append([]string{"exec", strconv.Itoa(id), "--"}, command...)}
}

// LXCFeatures enables what Docker-in-LXC needs; nesting also lets systemd inside the guest work fully.
func LXCFeatures(id int) Cmd {
	return Cmd{"pct", []string{"set", strconv.Itoa(id), "--features", "nesting=1,keyctl=1"}}
}

func LXCTemplate(id int) Cmd { return Cmd{"pct", []string{"template", strconv.Itoa(id)}} }

// ListGuests reads every guest in the cluster; pvesh is local and needs no credentials as root.
func ListGuestsCmd() Cmd {
	return Cmd{"pvesh", []string{"get", "/cluster/resources", "--type", "vm", "--output-format", "json"}}
}

func NodeStatusCmd(node string) Cmd {
	return Cmd{"pvesh", []string{"get", "/nodes/" + node + "/status", "--output-format", "json"}}
}

// Guest is one LXC or VM as pvesh reports it.
type Guest struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Type     string `json:"type"` // lxc | qemu
	Status   string `json:"status"`
	Template int    `json:"template"`
	Pool     string `json:"pool"`
	RawTags  string `json:"tags"`
}

func (g Guest) Tags() []string {
	if g.RawTags == "" {
		return nil
	}
	return strings.FieldsFunc(g.RawTags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

func (g Guest) HasTag(t string) bool { return slices.Contains(g.Tags(), t) }

// TagValue returns the value of a "key-value" tag (Proxmox tags can't contain '=' or ':').
func (g Guest) TagValue(key string) string {
	for _, t := range g.Tags() {
		if v, ok := strings.CutPrefix(t, key+"-"); ok {
			return v
		}
	}
	return ""
}

func ParseGuests(data []byte) ([]Guest, error) {
	var gs []Guest
	if err := json.Unmarshal(data, &gs); err != nil {
		return nil, fmt.Errorf("parse pvesh guest list: %w", err)
	}
	return gs, nil
}

// Host wraps a Runner with the safety rules every mutation must pass.
type Host struct {
	R         Runner
	Node      string
	VMIDRange [2]int
}

var ErrNotOwned = errors.New("refusing to touch a guest this tool does not own")

func (h *Host) run(ctx context.Context, c Cmd) ([]byte, error) {
	return h.R.Run(ctx, c.Name, c.Args...)
}

func (h *Host) Exec(ctx context.Context, c Cmd) error {
	_, err := h.run(ctx, c)
	return err
}

func (h *Host) Guests(ctx context.Context) ([]Guest, error) {
	out, err := h.run(ctx, ListGuestsCmd())
	if err != nil {
		return nil, err
	}
	return ParseGuests(out)
}

func (h *Host) InRange(id int) bool { return id >= h.VMIDRange[0] && id <= h.VMIDRange[1] }

// Owned is the hard check before any destructive call: in the reserved range AND tagged. Root can
// `pct destroy` anything, so this is enforced in code rather than left to convention.
func (h *Host) Owned(g Guest) bool { return h.InRange(g.VMID) && g.HasTag(Tag) && g.Template == 0 }

// Runners returns the guests this tool owns.
func (h *Host) Runners(ctx context.Context) ([]Guest, error) {
	gs, err := h.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []Guest
	for _, g := range gs {
		if h.Owned(g) {
			out = append(out, g)
		}
	}
	return out, nil
}

// PoolsGetCmd lists the cluster's resource pools.
func PoolsGetCmd() Cmd { return Cmd{"pvesh", []string{"get", "/pools", "--output-format", "json"}} }

// PoolCreateCmd creates a resource pool; pct refuses --pool for one that doesn't exist.
func PoolCreateCmd(pool string) Cmd {
	return Cmd{"pvesh", []string{"create", "/pools", "--poolid", pool, "--comment", "GitHub runners managed by pve-runners"}}
}

// EnsurePool creates the resource pool runners and templates go in, unless it already exists.
func (h *Host) EnsurePool(ctx context.Context, pool string) error {
	if pool == "" {
		return nil
	}
	out, err := h.run(ctx, PoolsGetCmd())
	if err != nil {
		return err
	}
	var pools []struct {
		PoolID string `json:"poolid"`
	}
	if err := json.Unmarshal(out, &pools); err != nil {
		return fmt.Errorf("parse pool list: %w", err)
	}
	for _, p := range pools {
		if p.PoolID == pool {
			return nil
		}
	}
	if err := h.Exec(ctx, PoolCreateCmd(pool)); err != nil {
		return fmt.Errorf("create resource pool %s: %w", pool, err)
	}
	return nil
}

// NextFreeID picks the lowest unused ID in the reserved range, looking at every guest in the cluster.
func (h *Host) NextFreeID(ctx context.Context) (int, error) { return h.NextFreeIDIn(ctx, h.VMIDRange) }

// NextFreeIDIn picks the lowest ID in rng that no guest in the cluster uses.
func (h *Host) NextFreeIDIn(ctx context.Context, rng [2]int) (int, error) {
	gs, err := h.Guests(ctx)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, g := range gs {
		used[g.VMID] = true
	}
	for id := rng[0]; id <= rng[1]; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free guest ID left in %d-%d", rng[0], rng[1])
}

// Destroy stops (if needed) and destroys an owned LXC. It re-reads the guest first so a stale list
// can never lead to destroying something that has since been reused.
func (h *Host) Destroy(ctx context.Context, id int) error {
	gs, err := h.Guests(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.VMID != id {
			continue
		}
		if !h.Owned(g) || g.Type != "lxc" {
			return fmt.Errorf("%w: %d (%s)", ErrNotOwned, id, g.Name)
		}
		if g.Status == "running" {
			if err := h.Exec(ctx, LXCStop(id)); err != nil {
				return err
			}
		}
		return h.Exec(ctx, LXCDestroy(id))
	}
	return nil // already gone
}

// FreeMemoryMB reports the node's free memory, used to avoid starving the host.
func (h *Host) FreeMemoryMB(ctx context.Context) (int, error) {
	out, err := h.run(ctx, NodeStatusCmd(h.Node))
	if err != nil {
		return 0, err
	}
	var st struct {
		Memory struct {
			Free  int64 `json:"free"`
			Total int64 `json:"total"`
			Used  int64 `json:"used"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("parse node status: %w", err)
	}
	free := st.Memory.Free
	if free == 0 {
		free = st.Memory.Total - st.Memory.Used
	}
	return int(free / (1 << 20)), nil
}

// TemplateTag marks template containers this tool created from downloaded images. They live in their
// own ID range (images.vmid_range), apart from runners, so reconcile never sees them.
const TemplateTag = "gh-template"

// CreateSpec describes a template container made from a downloaded image archive.
type CreateSpec struct {
	Archive     string // volume ID, e.g. local:vztmpl/file.tar.zst
	Hostname    string
	Pool        string
	Storage     string
	RootfsGB    int
	Bridge      string
	Tags        []string
	Description string
}

// LXCCreate builds `pct create` for a runner template. Unprivileged with nesting and keyctl is what
// Docker inside the runner needs; net0 has the Proxmox firewall on so clones inherit isolation.
func LXCCreate(id int, s CreateSpec) Cmd {
	return Cmd{"pct", []string{"create", strconv.Itoa(id), s.Archive,
		"--hostname", s.Hostname,
		"--pool", s.Pool,
		"--rootfs", s.Storage + ":" + strconv.Itoa(s.RootfsGB),
		"--unprivileged", "1",
		"--features", "nesting=1,keyctl=1",
		"--cores", "2",
		"--memory", "2048",
		"--net0", "name=eth0,bridge=" + s.Bridge + ",ip=dhcp,firewall=1",
		"--onboot", "0",
		"--tags", strings.Join(s.Tags, ";"),
		"--description", s.Description,
	}}
}

// OwnedTemplate is the guard for destroying image templates: inside the template range AND tagged.
func OwnedTemplate(g Guest, rng [2]int) bool {
	return g.Type == "lxc" && g.VMID >= rng[0] && g.VMID <= rng[1] && g.HasTag(TemplateTag)
}

// Templates returns the image templates this tool owns in rng.
func (h *Host) Templates(ctx context.Context, rng [2]int) ([]Guest, error) {
	gs, err := h.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []Guest
	for _, g := range gs {
		if OwnedTemplate(g, rng) {
			out = append(out, g)
		}
	}
	return out, nil
}

// DestroyTemplate removes an owned image template, re-reading the guest list first like Destroy.
// Proxmox itself refuses while linked clones of it still exist.
func (h *Host) DestroyTemplate(ctx context.Context, id int, rng [2]int) error {
	gs, err := h.Guests(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.VMID != id {
			continue
		}
		if !OwnedTemplate(g, rng) {
			return fmt.Errorf("%w: %d (%s)", ErrNotOwned, id, g.Name)
		}
		return h.Exec(ctx, LXCDestroy(id))
	}
	return nil
}

// VolumePathCmd asks Proxmox where a volume lives on disk; it works for volumes that don't exist yet,
// which is how the image cache finds a storage's vztmpl directory.
func VolumePathCmd(volid string) Cmd { return Cmd{"pvesm", []string{"path", volid}} }

func (h *Host) VolumePath(ctx context.Context, volid string) (string, error) {
	out, err := h.run(ctx, VolumePathCmd(volid))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
