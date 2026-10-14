// Package pvetest provides a fake Proxmox host that records commands and keeps a guest list in memory,
// so tests can assert exact argv and observe state changes without a real host.
package pvetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

type Fake struct {
	mu      sync.Mutex
	Calls   []string
	Guests  []pve.Guest
	FreeMB  int
	Pushed  map[string]string // "<id>:<dst>" -> contents of src at push time
	FailOn  map[string]error  // command prefix -> error
	ReadSrc func(path string) (string, error)
	// FirewallOn is the datacenter firewall switch; Group is the egress group's rules (nil = no group).
	FirewallOn bool
	// PolicyIn is the datacenter input policy ("" reports nothing, like an unset default of DROP).
	PolicyIn string
	Group    []map[string]any
	// NodeRules are the host firewall rules (pos, comment, dport).
	NodeRules []map[string]any
	// ResourcePools are the cluster's resource pools; New starts with gh-runners.
	ResourcePools []string
	// Storages is the node's storage list; New starts with local (no containers) and local-lvm.
	Storages []pve.Storage
	// UnitState maps a guest ID to what `systemctl is-active gh-runner.service` prints in it
	// (default "active").
	UnitState map[int]string
	// NotRegistered lists guests whose runner never connects to GitHub (RunnerPhaseCmd prints
	// nothing for them; every other guest reports "registered").
	NotRegistered map[int]bool
	// RunningJob lists guests whose runner's journal shows it running a job.
	RunningJob map[int]bool
	// Outputs maps a full command line to what it prints, for read-only commands like journalctl.
	Outputs map[string]string
	// Delay makes every command take this long, so concurrent callers interleave like on a real host.
	Delay time.Duration
}

func New(guests ...pve.Guest) *Fake {
	return &Fake{Guests: guests, Storages: []pve.Storage{
		{Name: "local", Type: "dir", Content: "iso,vztmpl,backup", Active: 1, Avail: 50 << 30},
		{Name: "local-lvm", Type: "lvmthin", Content: "rootdir,images", Active: 1, Avail: 400 << 30},
	}, ResourcePools: []string{"gh-runners"}, FreeMB: 32768, FirewallOn: true, Pushed: map[string]string{}, FailOn: map[string]error{}}
}

func (f *Fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	time.Sleep(f.Delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.Calls = append(f.Calls, line)
	for prefix, err := range f.FailOn {
		if strings.HasPrefix(line, prefix) {
			return nil, err
		}
	}
	if out, ok := f.Outputs[line]; ok {
		return []byte(out), nil
	}
	switch {
	case name == "pct" && args[0] == "exec" && strings.HasSuffix(line, "-- systemctl is-active gh-runner.service"):
		id, _ := strconv.Atoi(args[1])
		state, ok := f.UnitState[id]
		if !ok {
			state = "active"
		}
		if state == "active" {
			return []byte(state + "\n"), nil
		}
		return []byte(state + "\n"), fmt.Errorf("exit status 3")
	case name == "pct" && args[0] == "exec" && strings.Contains(line, "grep -e 'Listening for Jobs'"):
		id, _ := strconv.Atoi(args[1])
		switch {
		case f.NotRegistered[id]:
			return nil, nil
		case f.RunningJob[id]:
			return []byte("2026-01-02 03:04:05Z: Running job: build\n"), nil
		}
		return []byte("2026-01-02 03:04:05Z: Listening for Jobs\n"), nil
	case name == "pvesh" && args[0] == "get" && args[1] == "/cluster/firewall/options":
		on := 0
		if f.FirewallOn {
			on = 1
		}
		if f.PolicyIn != "" {
			return []byte(fmt.Sprintf(`{"enable":%d,"policy_in":%q}`, on, f.PolicyIn)), nil
		}
		return []byte(fmt.Sprintf(`{"enable":%d}`, on)), nil
	case name == "pvesh" && args[0] == "get" && strings.HasPrefix(args[1], "/cluster/firewall/groups/"):
		if f.Group == nil {
			return nil, fmt.Errorf("no such security group")
		}
		return json.Marshal(f.Group)
	case name == "pvesh" && args[0] == "create" && args[1] == "/cluster/firewall/groups":
		f.Group = []map[string]any{}
		return nil, nil
	case name == "pvesh" && args[0] == "delete" && strings.HasPrefix(args[1], "/cluster/firewall/groups/"):
		parts := strings.Split(args[1], "/")
		pos, _ := strconv.Atoi(parts[len(parts)-1])
		var kept []map[string]any
		for _, r := range f.Group {
			if p := int(r["pos"].(float64)); p != pos {
				if p > pos {
					r["pos"] = float64(p - 1)
				}
				kept = append(kept, r)
			}
		}
		f.Group = append([]map[string]any{}, kept...)
		return nil, nil
	case name == "pvesh" && args[0] == "create" && strings.HasPrefix(args[1], "/cluster/firewall/groups/"):
		r := map[string]any{}
		for i := 2; i+1 < len(args); i += 2 {
			r[strings.TrimPrefix(args[i], "--")] = args[i+1]
		}
		pos, _ := strconv.Atoi(r["pos"].(string))
		r["pos"] = float64(pos)
		f.Group = append(f.Group, r)
		return nil, nil
	case name == "pvesh" && strings.HasPrefix(args[1], "/nodes/") && strings.HasSuffix(args[1], "/firewall/rules") &&
		!strings.Contains(args[1], "/lxc/"):
		switch args[0] {
		case "get":
			if f.NodeRules == nil {
				return []byte("[]"), nil
			}
			return json.Marshal(f.NodeRules)
		case "create":
			r := map[string]any{"pos": float64(0)}
			for i := 2; i+1 < len(args); i += 2 {
				r[strings.TrimPrefix(args[i], "--")] = args[i+1]
			}
			// New rules go to the top, like Proxmox does.
			for _, o := range f.NodeRules {
				o["pos"] = o["pos"].(float64) + 1
			}
			f.NodeRules = append([]map[string]any{r}, f.NodeRules...)
		}
		return nil, nil
	case name == "pvesh" && args[0] == "delete" && strings.HasPrefix(args[1], "/nodes/") &&
		strings.Contains(args[1], "/firewall/rules/") && !strings.Contains(args[1], "/lxc/"):
		parts := strings.Split(args[1], "/")
		pos, _ := strconv.Atoi(parts[len(parts)-1])
		var kept []map[string]any
		for _, r := range f.NodeRules {
			if p := int(r["pos"].(float64)); p != pos {
				if p > pos {
					r["pos"] = float64(p - 1)
				}
				kept = append(kept, r)
			}
		}
		f.NodeRules = kept
		return nil, nil
	case name == "pvesh" && strings.HasPrefix(args[1], "/nodes/") && strings.Contains(args[1], "/firewall/"):
		return nil, nil
	case name == "pvesh" && args[0] == "get" && strings.HasPrefix(args[1], "/nodes/") && strings.HasSuffix(args[1], "/storage"):
		// Like Proxmox, --content filters the list.
		var out []pve.Storage
		for _, st := range f.Storages {
			if strings.Contains(","+st.Content+",", ","+args[3]+",") {
				out = append(out, st)
			}
		}
		return json.Marshal(out)
	case name == "pvesh" && args[1] == "/pools":
		if args[0] == "create" {
			f.ResourcePools = append(f.ResourcePools, args[3])
			return nil, nil
		}
		var out []map[string]string
		for _, p := range f.ResourcePools {
			out = append(out, map[string]string{"poolid": p})
		}
		return json.Marshal(out)
	case name == "pvesh" && args[1] == "/cluster/resources":
		return json.Marshal(f.Guests)
	case name == "pvesh" && strings.HasSuffix(args[1], "/status"):
		return []byte(fmt.Sprintf(`{"memory":{"free":%d,"total":%d,"used":0}}`, int64(f.FreeMB)<<20, int64(f.FreeMB)<<20)), nil
	case name == "pvesm" && args[0] == "path":
		_, file, _ := strings.Cut(args[1], ":vztmpl/")
		return []byte("/var/lib/vz/template/cache/" + file + "\n"), nil
	case name == "pct":
		return nil, f.pct(args)
	case name == "install" || name == "find":
		return nil, nil
	case name == "du":
		return []byte("0\t" + args[len(args)-1] + "\n"), nil
	}
	return nil, fmt.Errorf("fake: unexpected command %q", line)
}

func (f *Fake) find(id string) *pve.Guest {
	n, _ := strconv.Atoi(id)
	for i := range f.Guests {
		if f.Guests[i].VMID == n {
			return &f.Guests[i]
		}
	}
	return nil
}

func (f *Fake) pct(args []string) error {
	switch args[0] {
	case "clone":
		src := f.find(args[1])
		if src == nil {
			return fmt.Errorf("fake: template %s not found", args[1])
		}
		id, _ := strconv.Atoi(args[2])
		g := pve.Guest{VMID: id, Type: "lxc", Status: "stopped", Node: src.Node}
		for i := 3; i+1 < len(args); i += 2 {
			switch args[i] {
			case "--hostname":
				g.Name = args[i+1]
			case "--pool":
				g.Pool = args[i+1]
			}
		}
		f.Guests = append(f.Guests, g)
	case "set":
		g := f.find(args[1])
		for i := 2; i+1 < len(args); i += 2 {
			if args[i] == "--tags" {
				g.RawTags = args[i+1]
			}
		}
	case "start":
		f.find(args[1]).Status = "running"
	case "stop":
		f.find(args[1]).Status = "stopped"
	case "destroy":
		n, _ := strconv.Atoi(args[1])
		for i, g := range f.Guests {
			if g.VMID == n {
				f.Guests = append(f.Guests[:i], f.Guests[i+1:]...)
				break
			}
		}
	case "push":
		content := ""
		if f.ReadSrc != nil {
			c, err := f.ReadSrc(args[2])
			if err != nil {
				return err
			}
			content = c
		}
		f.Pushed[args[1]+":"+args[3]] = content
	case "create":
		id, _ := strconv.Atoi(args[1])
		g := pve.Guest{VMID: id, Type: "lxc", Status: "stopped", Node: "pve1"}
		for i := 3; i+1 < len(args); i += 2 {
			switch args[i] {
			case "--hostname":
				g.Name = args[i+1]
			case "--tags":
				g.RawTags = args[i+1]
			}
		}
		f.Guests = append(f.Guests, g)
	case "template":
		f.find(args[1]).Template = 1
	case "exec", "resize":
	default:
		return fmt.Errorf("fake: unexpected pct %s", args[0])
	}
	return nil
}

// Mutations returns recorded calls that are not read-only pvesh queries.
func (f *Fake) Mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if !strings.HasPrefix(c, "pvesh get ") && !strings.HasPrefix(c, "cat ") && !strings.HasPrefix(c, "du ") {
			out = append(out, c)
		}
	}
	return out
}

// SetRegistered changes whether a guest's runner reports itself connected to GitHub, safely while
// other goroutines run commands.
func (f *Fake) SetRegistered(id int, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NotRegistered == nil {
		f.NotRegistered = map[int]bool{}
	}
	f.NotRegistered[id] = !ok
}

// Guest returns a copy of a guest by ID.
func (f *Fake) Guest(id int) (pve.Guest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.Guests {
		if g.VMID == id {
			return g, true
		}
	}
	return pve.Guest{}, false
}

// Stop marks a guest stopped, as an ephemeral runner powering itself off after its job.
func (f *Fake) Stop(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.find(strconv.Itoa(id)); g != nil {
		g.Status = "stopped"
	}
}
