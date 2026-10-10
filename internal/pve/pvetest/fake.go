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
	Group      []map[string]any
}

func New(guests ...pve.Guest) *Fake {
	return &Fake{Guests: guests, FreeMB: 32768, FirewallOn: true, Pushed: map[string]string{}, FailOn: map[string]error{}}
}

func (f *Fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.Calls = append(f.Calls, line)
	for prefix, err := range f.FailOn {
		if strings.HasPrefix(line, prefix) {
			return nil, err
		}
	}
	switch {
	case name == "pvesh" && args[0] == "get" && args[1] == "/cluster/firewall/options":
		on := 0
		if f.FirewallOn {
			on = 1
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
	case name == "pvesh" && strings.HasPrefix(args[1], "/nodes/") && strings.Contains(args[1], "/firewall/"):
		return nil, nil
	case name == "pvesh" && args[1] == "/cluster/resources":
		return json.Marshal(f.Guests)
	case name == "pvesh" && strings.HasSuffix(args[1], "/status"):
		return []byte(fmt.Sprintf(`{"memory":{"free":%d,"total":%d,"used":0}}`, int64(f.FreeMB)<<20, int64(f.FreeMB)<<20)), nil
	case name == "pvesm" && args[0] == "path":
		_, file, _ := strings.Cut(args[1], ":vztmpl/")
		return []byte("/var/lib/vz/template/cache/" + file + "\n"), nil
	case name == "pct":
		return nil, f.pct(args)
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
		if !strings.HasPrefix(c, "pvesh get ") {
			out = append(out, c)
		}
	}
	return out
}
