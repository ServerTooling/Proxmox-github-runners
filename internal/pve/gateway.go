package pve

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// A runner bridge whose gateway address is also on the host, with forwarding off, gives runners two
// answers to "who has the gateway": the router's and the host's. While a runner's ARP entry points at
// the host, its packets are dropped, so open connections stall (npm ETIMEDOUT, checkouts and cache
// downloads hanging) until the entry flips back. Only the host's network config can fix it, so this
// is reported, not changed.

func RunnerRouteCmd(id int) Cmd {
	return LXCExec(id, "ip", "-4", "route", "show", "default")
}

func BridgeAddrsCmd(bridge string) Cmd {
	return Cmd{"ip", []string{"-4", "-o", "addr", "show", "dev", bridge}}
}

func IPForwardCmd() Cmd { return Cmd{"cat", []string{"/proc/sys/net/ipv4/ip_forward"}} }

// GatewayConflict checks a running runner on bridge and returns a warning when its default gateway is
// one of the host's own addresses on that bridge and the host doesn't forward; "" means no conflict.
func (h *Host) GatewayConflict(ctx context.Context, id int, bridge string) (string, error) {
	route, err := h.run(ctx, RunnerRouteCmd(id))
	if err != nil {
		return "", err
	}
	gw, ok := defaultGateway(string(route))
	if !ok {
		return "", nil
	}
	addrs, err := h.run(ctx, BridgeAddrsCmd(bridge))
	if err != nil {
		return "", err
	}
	if !hasAddr(string(addrs), gw) {
		return "", nil
	}
	fwd, err := h.run(ctx, IPForwardCmd())
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(fwd)) == "1" {
		return "", nil // the host is the runners' router
	}
	return fmt.Sprintf("runners on %s use gateway %s, which this host also holds on %s without forwarding; "+
		"their connections stall whenever they reach the host instead of the router. Remove the address from "+
		"%s on the host (iface %s inet manual in /etc/network/interfaces, then ifreload -a)",
		bridge, gw, bridge, bridge, bridge), nil
}

// defaultGateway reads `ip -4 route show default`: "default via 192.0.2.1 dev eth0 ...".
func defaultGateway(out string) (netip.Addr, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[0] == "default" && f[i] == "via" {
				a, err := netip.ParseAddr(f[i+1])
				return a, err == nil
			}
		}
	}
	return netip.Addr{}, false
}

// hasAddr reads `ip -4 -o addr show`: "5: vmbr4    inet 192.0.2.1/26 brd ... scope global vmbr4 ...".
func hasAddr(out string, a netip.Addr) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] != "inet" {
				continue
			}
			if p, err := netip.ParsePrefix(f[i+1]); err == nil && p.Addr() == a {
				return true
			}
		}
	}
	return false
}

// GatewayWarnings checks one running runner per bridge in use and says how many bridges it could check.
func (h *Host) GatewayWarnings(ctx context.Context, bridgeOf func(Guest) string) (warnings []string, checked int, err error) {
	runners, err := h.Runners(ctx)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for _, g := range runners {
		b := bridgeOf(g)
		if g.Status != "running" || b == "" || seen[b] {
			continue
		}
		w, err := h.GatewayConflict(ctx, g.VMID, b)
		if err != nil {
			continue // try another runner on this bridge
		}
		seen[b] = true
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings, len(seen), nil
}
