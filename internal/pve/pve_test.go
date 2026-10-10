package pve_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

func TestCommandBuildersExactArgv(t *testing.T) {
	cases := []struct {
		got  pve.Cmd
		want string
	}{
		{pve.LXCClone(8000, 9001, "gh-lxc-9001", "gh-runners", "local-lvm", false),
			"pct clone 8000 9001 --hostname gh-lxc-9001 --pool gh-runners"},
		{pve.LXCClone(8000, 9001, "gh-lxc-9001", "gh-runners", "local-lvm", true),
			"pct clone 8000 9001 --hostname gh-lxc-9001 --pool gh-runners --full 1 --storage local-lvm"},
		{pve.LXCSet(9001, pve.LXCSpec{Cores: 2, MemoryMB: 4096, Tags: []string{"gh-runner", "pool-small"}}),
			"pct set 9001 --cores 2 --memory 4096 --tags gh-runner;pool-small --onboot 0"},
		{pve.LXCSet(9001, pve.LXCSpec{Cores: 1, MemoryMB: 512, Tags: []string{"gh-runner"}, Description: "d", Bridge: "vmbr1", VLAN: 50}),
			"pct set 9001 --cores 1 --memory 512 --tags gh-runner --onboot 0 --description d --net0 name=eth0,bridge=vmbr1,ip=dhcp,firewall=1,tag=50"},
		{pve.LXCStart(9001), "pct start 9001"},
		{pve.LXCResize(9001, 40), "pct resize 9001 rootfs 40G"},
		{pve.LXCStop(9001), "pct stop 9001"},
		{pve.LXCDestroy(9001), "pct destroy 9001 --purge 1 --force 1"},
		{pve.LXCPush(9001, "/tmp/a", "/etc/gh-runner/jitconfig", "0600"), "pct push 9001 /tmp/a /etc/gh-runner/jitconfig --perms 0600"},
		{pve.LXCExec(9001, "systemctl", "start", "x"), "pct exec 9001 -- systemctl start x"},
		{pve.ListGuestsCmd(), "pvesh get /cluster/resources --type vm --output-format json"},
	}
	for _, c := range cases {
		if c.got.String() != c.want {
			t.Errorf("got  %q\nwant %q", c.got.String(), c.want)
		}
	}
}

func TestEnsurePool(t *testing.T) {
	f := pvetest.New()
	h := &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9010}}
	ctx := context.Background()
	if err := h.EnsurePool(ctx, "gh-runners"); err != nil || len(f.Mutations()) != 0 {
		t.Fatalf("existing pool: %v %q", err, f.Mutations())
	}
	if err := h.EnsurePool(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsurePool(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pvesh create /pools --poolid ci --comment GitHub runners managed by pve-runners"}
	if !reflect.DeepEqual(f.Mutations(), want) {
		t.Fatalf("got %q", f.Mutations())
	}
	if got := pve.PoolsGetCmd().String(); got != "pvesh get /pools --output-format json" {
		t.Errorf("list: %q", got)
	}
}

func TestParseGuestsAndTags(t *testing.T) {
	gs, err := pve.ParseGuests([]byte(`[{"vmid":9001,"name":"gh","node":"pve1","type":"lxc","status":"running","tags":"gh-runner;pool-small","template":0}]`))
	if err != nil {
		t.Fatal(err)
	}
	g := gs[0]
	if !g.HasTag("gh-runner") || g.TagValue("pool") != "small" || !reflect.DeepEqual(g.Tags(), []string{"gh-runner", "pool-small"}) {
		t.Fatalf("unexpected tags: %+v", g)
	}
}

func host(f *pvetest.Fake) *pve.Host {
	return &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9002}}
}

func TestNextFreeIDSkipsUsedAndFailsWhenFull(t *testing.T) {
	f := pvetest.New(pve.Guest{VMID: 9000}, pve.Guest{VMID: 9002})
	id, err := host(f).NextFreeID(context.Background())
	if err != nil || id != 9001 {
		t.Fatalf("got %d, %v", id, err)
	}
	f.Guests = append(f.Guests, pve.Guest{VMID: 9001})
	if _, err := host(f).NextFreeID(context.Background()); err == nil {
		t.Fatal("expected range-exhausted error")
	}
}

func TestDestroyRefusesGuestsItDoesNotOwn(t *testing.T) {
	f := pvetest.New(
		pve.Guest{VMID: 9000, Type: "lxc", Status: "running"},                       // in range, untagged
		pve.Guest{VMID: 100, Type: "lxc", RawTags: "gh-runner"},                     // tagged, out of range
		pve.Guest{VMID: 9001, Type: "lxc", RawTags: "gh-runner", Template: 1},       // a template
		pve.Guest{VMID: 9002, Type: "lxc", Status: "running", RawTags: "gh-runner"}, // ours
	)
	h := host(f)
	for _, id := range []int{9000, 100, 9001} {
		if err := h.Destroy(context.Background(), id); !errors.Is(err, pve.ErrNotOwned) {
			t.Errorf("destroy %d: want ErrNotOwned, got %v", id, err)
		}
	}
	if len(f.Mutations()) != 0 {
		t.Fatalf("no mutation expected, got %v", f.Mutations())
	}
	if err := h.Destroy(context.Background(), 9002); err != nil {
		t.Fatal(err)
	}
	want := []string{"pct stop 9002", "pct destroy 9002 --purge 1 --force 1"}
	if !reflect.DeepEqual(f.Mutations(), want) {
		t.Fatalf("got %v want %v", f.Mutations(), want)
	}
}

func TestFreeMemory(t *testing.T) {
	f := pvetest.New()
	f.FreeMB = 1234
	mb, err := host(f).FreeMemoryMB(context.Background())
	if err != nil || mb != 1234 {
		t.Fatalf("got %d, %v", mb, err)
	}
}

func TestEgressRulesOrder(t *testing.T) {
	r := pve.EgressRules([]string{"10.0.0.0/8", "192.168.0.0/16"}, []pve.AllowRule{{Dest: "192.0.2.1", Proto: "udp", Port: 53}})
	want := []pve.Rule{
		{Action: "ACCEPT", Dest: "192.0.2.1", Proto: "udp", DPort: "53", Comment: "pve-runners LAN exception"},
		{Action: "DROP", Dest: "10.0.0.0/8", Comment: "pve-runners: no LAN access"},
		{Action: "DROP", Dest: "192.168.0.0/16", Comment: "pve-runners: no LAN access"},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v", r)
	}
}

func TestEnsureEgressGroupRewritesOnlyWhenChanged(t *testing.T) {
	f := pvetest.New()
	h := host(f)
	ctx := context.Background()
	v1 := pve.EgressRules([]string{"10.0.0.0/8"}, nil)
	if err := h.EnsureEgressGroup(ctx, v1); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureEgressGroup(ctx, v1); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Mutations()); n != 2 { // create group + one rule, then nothing
		t.Fatalf("got %q", f.Mutations())
	}
	v2 := pve.EgressRules([]string{"10.0.0.0/8", "172.16.0.0/12"}, nil)
	if err := h.EnsureEgressGroup(ctx, v2); err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()[2:]
	want := []string{
		"pvesh delete /cluster/firewall/groups/gh-runner-egress/0",
		"pvesh create /cluster/firewall/groups/gh-runner-egress --pos 0 --type out --action DROP --dest 10.0.0.0/8 --enable 1 --comment pve-runners: no LAN access",
		"pvesh create /cluster/firewall/groups/gh-runner-egress --pos 1 --type out --action DROP --dest 172.16.0.0/12 --enable 1 --comment pve-runners: no LAN access",
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %q", m)
	}
}

func TestSetupPortRuleIsAddedAndOnlyItsOwnRemoved(t *testing.T) {
	f := pvetest.New()
	f.NodeRules = []map[string]any{{"pos": float64(0), "comment": "owner rule", "dport": "22"}}
	h := &pve.Host{R: f, Node: "pve1"}
	closeFn, err := h.OpenSetupPort(context.Background(), 8765)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.Mutations(); !reflect.DeepEqual(m, []string{"pvesh create /nodes/pve1/firewall/rules --type in --action ACCEPT " +
		"--proto tcp --dport 8765 --enable 1 --comment pve-runners setup (temporary)"}) {
		t.Fatalf("%q", m)
	}
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := f.Mutations(); m[len(m)-1] != "pvesh delete /nodes/pve1/firewall/rules/0" ||
		len(f.NodeRules) != 1 || f.NodeRules[0]["comment"] != "owner rule" {
		t.Fatalf("commands %q, rules left %v", m, f.NodeRules)
	}
}

func TestHostInputBlocked(t *testing.T) {
	for _, c := range []struct {
		on     bool
		policy string
		want   bool
	}{{true, "", true}, {true, "DROP", true}, {true, "ACCEPT", false}, {false, "DROP", false}} {
		f := pvetest.New()
		f.FirewallOn, f.PolicyIn = c.on, c.policy
		got, err := (&pve.Host{R: f}).HostInputBlocked(context.Background())
		if err != nil || got != c.want {
			t.Errorf("on=%v policy=%q: got %v %v", c.on, c.policy, got, err)
		}
	}
}
