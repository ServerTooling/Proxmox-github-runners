package docker

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// Chains holding the isolation rules, in the mangle table. Docker never touches mangle, so its own
// ACCEPT rules in the filter table can't skip ours, and a DROP in any table drops the packet whichever
// iptables backend (nft or legacy) the machine's Docker uses.
const (
	ForwardChain = "PVE-RUNNERS-FWD" // runner -> anywhere but the machine
	InputChain   = "PVE-RUNNERS-IN"  // runner -> the machine itself (its web UI, shares, SSH)
)

// IsolationRules is the iptables-restore input for the two chains: LAN exceptions return to normal
// processing, blocked ranges drop, and nothing reaches the machine but the exceptions. Runners get no
// IPv6 on their network, so IPv6 ranges are left out.
func IsolationRules(rules []pve.Rule) string {
	var b strings.Builder
	b.WriteString("*mangle\n")
	for _, c := range []string{ForwardChain, InputChain} {
		fmt.Fprintf(&b, ":%s - [0:0]\n-F %s\n", c, c)
	}
	for _, r := range rules {
		if ip, _, err := net.ParseCIDR(r.Dest); (err == nil && ip.To4() == nil) || strings.Contains(r.Dest, ":") {
			continue
		}
		match := "-d " + r.Dest
		if r.Proto != "" {
			match += " -p " + r.Proto
			if r.DPort != "" {
				match += " --dport " + r.DPort
			}
		}
		target := "DROP"
		if r.Action == "ACCEPT" {
			target = "RETURN"
		}
		fmt.Fprintf(&b, "-A %s %s -j %s\n", ForwardChain, match, target)
		if target == "RETURN" {
			fmt.Fprintf(&b, "-A %s %s -j RETURN\n", InputChain, match)
		}
	}
	fmt.Fprintf(&b, "-A %s -j DROP\nCOMMIT\n", InputChain)
	return b.String()
}

// isolateScript loads the rules in one atomic iptables-restore and hooks both chains in for traffic
// from the runner bridge. It runs in a throwaway container on the host's network, so it needs no
// root login on the machine, only Docker. Run on every spawn, it restores the rules after a reboot.
const isolateScript = `set -e
ipt=iptables
$ipt -t mangle -S >/dev/null 2>&1 || ipt=iptables-legacy
"$ipt-restore" --noflush
for hook in FORWARD:` + ForwardChain + ` INPUT:` + InputChain + `; do
  $ipt -t mangle -C "${hook%%:*}" -i ` + Bridge + ` -j "${hook#*:}" 2>/dev/null ||
    $ipt -t mangle -I "${hook%%:*}" 1 -i ` + Bridge + ` -j "${hook#*:}"
done
echo isolated`

// IsolateCmd runs isolateScript (rules on stdin) in a container of image, which needs iptables (the
// runner image has it, for Docker).
func IsolateCmd(image string) []string {
	return []string{"docker", "run", "--rm", "-i", "--network", "host", "--cap-add", "NET_ADMIN",
		"--cap-add", "NET_RAW", "--runtime", "runc", "--entrypoint", "/bin/sh", image, "-c", isolateScript}
}

// Isolate keeps runners on the machine off the LAN and off the machine itself.
func (h *Host) Isolate(ctx context.Context, image string, rules []pve.Rule) error {
	out, err := h.run(ctx, strings.NewReader(IsolationRules(rules)), IsolateCmd(image))
	if err != nil {
		return fmt.Errorf("isolating runners from the LAN failed, so none start: %w", err)
	}
	if !strings.Contains(string(out), "isolated") {
		return fmt.Errorf("isolating runners from the LAN did not finish, so none start: %q", out)
	}
	return nil
}
