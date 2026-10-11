package pve_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

func TestGatewayConflict(t *testing.T) {
	route := "pct exec 9001 -- ip -4 route show default"
	addrs := "ip -4 -o addr show dev vmbr4"
	fwd := "cat /proc/sys/net/ipv4/ip_forward"
	for _, tc := range []struct {
		name, route, addrs, fwd string
		warn                    bool
	}{
		{"host holds the gateway without forwarding", "default via 192.0.2.1 dev eth0 proto dhcp src 192.0.2.20 metric 1024\n",
			"7: vmbr4    inet 192.0.2.1/26 scope global vmbr4\\       valid_lft forever preferred_lft forever\n", "0\n", true},
		{"host is the router", "default via 192.0.2.1 dev eth0\n", "7: vmbr4    inet 192.0.2.1/26 scope global vmbr4\n", "1\n", false},
		{"host has another address", "default via 192.0.2.1 dev eth0\n", "7: vmbr4    inet 192.0.2.2/26 scope global vmbr4\n", "0\n", false},
		{"host has no address", "default via 192.0.2.1 dev eth0\n", "", "0\n", false},
		{"no default route", "", "7: vmbr4    inet 192.0.2.1/26 scope global vmbr4\n", "0\n", false},
	} {
		f := pvetest.New()
		f.Outputs = map[string]string{route: tc.route, addrs: tc.addrs, fwd: tc.fwd}
		w, err := (&pve.Host{R: f}).GatewayConflict(context.Background(), 9001, "vmbr4")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := w != ""; got != tc.warn {
			t.Errorf("%s: warning %q", tc.name, w)
		}
		if tc.warn && (!strings.Contains(w, "192.0.2.1") || !strings.Contains(w, "inet manual")) {
			t.Errorf("%s: warning lacks the address or the fix: %q", tc.name, w)
		}
	}
}

func TestGatewayWarningsChecksOneRunningRunnerPerBridge(t *testing.T) {
	f := pvetest.New(
		pve.Guest{VMID: 9000, Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-default"},
		pve.Guest{VMID: 9001, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-default"},
		pve.Guest{VMID: 9002, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-default"})
	f.Outputs = map[string]string{
		"pct exec 9001 -- ip -4 route show default": "default via 192.0.2.1 dev eth0\n",
		"ip -4 -o addr show dev vmbr4":              "7: vmbr4    inet 192.0.2.1/26 scope global vmbr4\n",
		"cat /proc/sys/net/ipv4/ip_forward":         "0\n",
	}
	h := &pve.Host{R: f, VMIDRange: [2]int{9000, 9499}}
	ws, checked, err := h.GatewayWarnings(context.Background(), func(pve.Guest) string { return "vmbr4" })
	if err != nil || checked != 1 || len(ws) != 1 {
		t.Fatalf("%v %d %v", ws, checked, err)
	}
	if strings.Contains(strings.Join(f.Calls, "\n"), "pct exec 9002") || strings.Contains(strings.Join(f.Calls, "\n"), "pct exec 9000") {
		t.Fatalf("checked more than one running runner: %v", f.Calls)
	}
}
