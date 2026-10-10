package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// EgressGroup is the cluster firewall security group every runner guest gets: internet yes, LAN no.
const EgressGroup = "gh-runner-egress"

// DefaultBlocked are the destinations runners may never reach: private (RFC 1918), carrier-grade NAT,
// link-local (including cloud-style metadata at 169.254.169.254) and IPv6 unique-local. The Proxmox
// host, the iDRAC and everything else at home live in these ranges.
var DefaultBlocked = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "fc00::/7"}

// Rule is one firewall rule in the egress group, in evaluation order.
type Rule struct {
	Action  string // ACCEPT | DROP
	Dest    string
	Proto   string
	DPort   string
	Comment string
}

// AllowRule is a LAN exception, e.g. DNS on the home router.
type AllowRule struct {
	Dest  string `yaml:"dest,omitempty"` // empty = any blocked range
	Proto string `yaml:"proto"`
	Port  int    `yaml:"port"`
}

// EgressRules builds the ordered rule list: explicit LAN exceptions first, then drops for every
// blocked range. Anything not matched (the internet) falls through to the guest's ACCEPT out-policy.
func EgressRules(blocked []string, allow []AllowRule) []Rule {
	var rules []Rule
	for _, a := range allow {
		dests := []string{a.Dest}
		if a.Dest == "" {
			dests = blocked
		}
		for _, d := range dests {
			rules = append(rules, Rule{"ACCEPT", d, a.Proto, strconv.Itoa(a.Port), "pve-runners LAN exception"})
		}
	}
	for _, b := range blocked {
		rules = append(rules, Rule{"DROP", b, "", "", "pve-runners: no LAN access"})
	}
	return rules
}

const groupPath = "/cluster/firewall/groups/" + EgressGroup

func GroupGetCmd() Cmd {
	return Cmd{"pvesh", []string{"get", groupPath, "--output-format", "json"}}
}

func GroupCreateCmd() Cmd {
	return Cmd{"pvesh", []string{"create", "/cluster/firewall/groups", "--group", EgressGroup,
		"--comment", "Runner guests: internet only, no LAN (managed by pve-runners)"}}
}

func GroupDeleteRuleCmd(pos int) Cmd {
	return Cmd{"pvesh", []string{"delete", groupPath + "/" + strconv.Itoa(pos)}}
}

func GroupAddRuleCmd(pos int, r Rule) Cmd {
	args := []string{"create", groupPath, "--pos", strconv.Itoa(pos), "--type", "out", "--action", r.Action,
		"--dest", r.Dest, "--enable", "1", "--comment", r.Comment}
	if r.Proto != "" {
		args = append(args, "--proto", r.Proto)
	}
	if r.DPort != "" {
		args = append(args, "--dport", r.DPort)
	}
	return Cmd{"pvesh", args}
}

func ClusterFirewallOptionsCmd() Cmd {
	return Cmd{"pvesh", []string{"get", "/cluster/firewall/options", "--output-format", "json"}}
}

// GuestFirewallCmds turns the firewall on for one container, drops all inbound, allows outbound, and
// attaches the egress group, which is evaluated before the out-policy.
func GuestFirewallCmds(node string, id int) []Cmd {
	base := fmt.Sprintf("/nodes/%s/lxc/%d/firewall", node, id)
	return []Cmd{
		{"pvesh", []string{"set", base + "/options", "--enable", "1", "--policy_in", "DROP", "--policy_out", "ACCEPT", "--dhcp", "1", "--ipfilter", "1"}},
		{"pvesh", []string{"create", base + "/rules", "--type", "group", "--action", EgressGroup, "--enable", "1"}},
	}
}

// ClusterFirewallEnabled reports whether the datacenter firewall is on. Guest rules do nothing while
// it is off, so isolation can't be promised without it.
func (h *Host) ClusterFirewallEnabled(ctx context.Context) (bool, error) {
	out, err := h.run(ctx, ClusterFirewallOptionsCmd())
	if err != nil {
		return false, err
	}
	var o struct {
		Enable int `json:"enable"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		return false, fmt.Errorf("parse firewall options: %w", err)
	}
	return o.Enable == 1, nil
}

type groupRule struct {
	Pos     int    `json:"pos"`
	Action  string `json:"action"`
	Dest    string `json:"dest"`
	Proto   string `json:"proto"`
	DPort   string `json:"dport"`
	Comment string `json:"comment"`
}

// EnsureEgressGroup makes the cluster security group hold exactly rules, rewriting it only when it
// differs, so it is cheap to call before every spawn.
func (h *Host) EnsureEgressGroup(ctx context.Context, rules []Rule) error {
	out, err := h.run(ctx, GroupGetCmd())
	var existing []groupRule
	if err != nil {
		if err := h.Exec(ctx, GroupCreateCmd()); err != nil {
			return fmt.Errorf("create firewall group %s: %w", EgressGroup, err)
		}
	} else if err := json.Unmarshal(out, &existing); err != nil {
		return fmt.Errorf("parse firewall group: %w", err)
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].Pos < existing[j].Pos })
	same := len(existing) == len(rules)
	for i := 0; same && i < len(rules); i++ {
		e, r := existing[i], rules[i]
		same = e.Action == r.Action && e.Dest == r.Dest && e.Proto == r.Proto && e.DPort == r.DPort
	}
	if same {
		return nil
	}
	for i := len(existing) - 1; i >= 0; i-- {
		if err := h.Exec(ctx, GroupDeleteRuleCmd(existing[i].Pos)); err != nil {
			return err
		}
	}
	for i, r := range rules {
		if err := h.Exec(ctx, GroupAddRuleCmd(i, r)); err != nil {
			return err
		}
	}
	return nil
}

// SetupRuleComment marks the temporary rule `pve-runners setup --open-firewall` adds, so it can be
// found and removed again without touching the owner's own rules.
const SetupRuleComment = "pve-runners setup (temporary)"

func nodeRulesPath(node string) string { return "/nodes/" + node + "/firewall/rules" }

func NodeRulesGetCmd(node string) Cmd {
	return Cmd{"pvesh", []string{"get", nodeRulesPath(node), "--output-format", "json"}}
}

// SetupRuleAddCmd lets the setup page through the host firewall, which with the datacenter
// firewall on only allows the Proxmox UI, SSH and console ports by default.
func SetupRuleAddCmd(node string, port int) Cmd {
	return Cmd{"pvesh", []string{"create", nodeRulesPath(node), "--type", "in", "--action", "ACCEPT",
		"--proto", "tcp", "--dport", strconv.Itoa(port), "--enable", "1", "--comment", SetupRuleComment}}
}

func NodeRuleDeleteCmd(node string, pos int) Cmd {
	return Cmd{"pvesh", []string{"delete", nodeRulesPath(node) + "/" + strconv.Itoa(pos)}}
}

// OpenSetupPort adds the temporary rule and returns a func that removes it (and any left over from
// an earlier run that was killed).
func (h *Host) OpenSetupPort(ctx context.Context, port int) (closeFn func(context.Context) error, err error) {
	if err := h.Exec(ctx, SetupRuleAddCmd(h.Node, port)); err != nil {
		return nil, err
	}
	return h.CloseSetupPort, nil
}

// CloseSetupPort deletes every rule carrying SetupRuleComment, highest position first so earlier
// positions stay valid.
func (h *Host) CloseSetupPort(ctx context.Context) error {
	out, err := h.run(ctx, NodeRulesGetCmd(h.Node))
	if err != nil {
		return err
	}
	var rules []struct {
		Pos     int    `json:"pos"`
		Comment string `json:"comment"`
	}
	if err := json.Unmarshal(out, &rules); err != nil {
		return fmt.Errorf("parse node firewall rules: %w", err)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Pos > rules[j].Pos })
	for _, r := range rules {
		if r.Comment == SetupRuleComment {
			if err := h.Exec(ctx, NodeRuleDeleteCmd(h.Node, r.Pos)); err != nil {
				return err
			}
		}
	}
	return nil
}
