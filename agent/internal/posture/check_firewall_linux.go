package posture

import (
	"context"
	"strings"
)

// firewallCheck reports whether a host firewall is active.
//
// Linux has no single answer to this, which is the whole difficulty. ufw,
// firewalld and plain nftables or iptables are all legitimate, and a machine
// using one will not have the others installed. So this asks each in turn and
// only concludes "no firewall" when it has actually looked and found nothing
// active, never because a tool was missing.
type firewallCheck struct{}

func (firewallCheck) Name() string { return "firewall" }

func (firewallCheck) Category() Category { return CategoryFirewall }

func (c firewallCheck) Run(ctx context.Context) Result {
	const name = "firewall"

	// ufw first: if it is installed it is almost certainly the one in charge.
	if out, err := output(ctx, "ufw", "status"); err == nil {
		if strings.Contains(strings.ToLower(out), "status: active") {
			return pass(name, "ufw is active", map[string]string{"backend": "ufw"})
		}
		return fail(name, "ufw is installed but inactive", map[string]string{"backend": "ufw"})
	}

	if out, err := output(ctx, "firewall-cmd", "--state"); err == nil {
		if strings.TrimSpace(out) == "running" {
			return pass(name, "firewalld is running", map[string]string{"backend": "firewalld"})
		}
		return fail(name, "firewalld is installed but not running",
			map[string]string{"backend": "firewalld"})
	}

	// nftables: a ruleset with no rules is not a firewall, so the presence of
	// the tool proves nothing and the content has to be looked at.
	if out, err := output(ctx, "nft", "list", "ruleset"); err == nil {
		if hasRules(out) {
			return pass(name, "nftables has an active ruleset",
				map[string]string{"backend": "nftables"})
		}
		return fail(name, "nftables is present with an empty ruleset",
			map[string]string{"backend": "nftables"})
	}

	// Reaching here means none of the tools were installed, or none could be
	// read without more privilege than we have. Either way we did not find
	// out, and saying so is the only honest answer. Reporting "no firewall"
	// here would fail every machine that simply uses a fourth thing.
	return unknown(name, "no firewall tool could be queried on this machine")
}

// hasRules reports whether an nft ruleset does anything.
//
// `nft list ruleset` on a machine with no rules prints nothing, or prints empty
// table and chain declarations. Both are a firewall that permits everything.
func hasRules(ruleset string) bool {
	for _, line := range strings.Split(ruleset, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", line == "}":
			continue
		case strings.HasPrefix(line, "table "), strings.HasPrefix(line, "chain "):
			continue
		case strings.HasPrefix(line, "type "), strings.HasPrefix(line, "comment "):
			// A chain's type/hook/policy declaration is structure, not a rule.
			continue
		default:
			return true
		}
	}
	return false
}
