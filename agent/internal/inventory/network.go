package inventory

import (
	"context"
	"net"
	"sort"
	"strings"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// NetInterface is one interface as the agent sees it.
//
// This is INVENTORY, not telemetry: it answers "what interfaces does this
// machine have and what addresses are on them", which changes rarely and is
// worth keeping history for. Stored bitemporally, it answers the question an
// incident actually starts with -- what address did this box have at 03:00
// last Tuesday -- which a current-state-only view cannot.
//
// Throughput counters are deliberately NOT here; they belong in telemetry,
// which is sampled every minute and aged out. See internal/telemetry.
type NetInterface struct {
	Name string `json:"name"`
	MAC  string `json:"mac,omitempty"`
	MTU  int    `json:"mtu,omitempty"`
	// Up is the operational flag, not "has an address". A cable-out NIC is
	// down with an address still configured, and that distinction is the
	// whole point of looking.
	Up        bool     `json:"up"`
	Loopback  bool     `json:"loopback"`
	Addresses []string `json:"addresses,omitempty"`
}

type networkSnapshot struct {
	Interfaces []NetInterface `json:"interfaces"`
}

// collectNetwork lists interfaces and their addresses.
//
// Interfaces and addresses are both sorted. The snapshot is hashed to decide
// whether anything changed, and the kernel does not promise a stable order,
// so unsorted output would produce a new "change" on most polls and fill the
// fact history with churn that is not change.
func collectNetwork(ctx context.Context) (any, error) {
	ifaces, err := psnet.InterfacesWithContext(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]NetInterface, 0, len(ifaces))
	for _, in := range ifaces {
		flags := flagSet(in.Flags)
		iface := NetInterface{
			Name:     in.Name,
			MAC:      in.HardwareAddr,
			MTU:      in.MTU,
			Up:       flags["up"] || flags["running"],
			Loopback: flags["loopback"],
		}
		for _, a := range in.Addrs {
			// gopsutil hands these back in CIDR form. Keep the prefix: an
			// operator reading "10.0.2.15" cannot tell a /24 from a /32, and
			// the mask is half of what makes an address make sense.
			if addr := strings.TrimSpace(a.Addr); addr != "" {
				iface.Addresses = append(iface.Addresses, addr)
			}
		}
		sort.Strings(iface.Addresses)
		out = append(out, iface)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return networkSnapshot{Interfaces: out}, nil
}

func flagSet(flags []string) map[string]bool {
	set := make(map[string]bool, len(flags))
	for _, f := range flags {
		set[strings.ToLower(f)] = true
	}
	return set
}

// PrimaryAddress reports the address most likely to be the one somebody means
// by "the machine's IP": the first non-loopback, non-link-local IPv4 on an
// interface that is up.
//
// Best effort by construction. A host with two routable NICs has no single
// correct answer, so this is a display convenience and nothing depends on it.
func PrimaryAddress(ifaces []NetInterface) string {
	for _, i := range ifaces {
		if !i.Up || i.Loopback {
			continue
		}
		for _, a := range i.Addresses {
			ip, _, err := net.ParseCIDR(a)
			if err != nil {
				ip = net.ParseIP(a)
			}
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}
