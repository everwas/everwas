package inventory

import (
	"context"
	"testing"
)

// TestCollectNetworkSeesThisMachine is a smoke test against the real host:
// any machine running this has at least a loopback.
func TestCollectNetworkSeesThisMachine(t *testing.T) {
	got, err := collectNetwork(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	snap, ok := got.(networkSnapshot)
	if !ok {
		t.Fatalf("collect returned %T", got)
	}
	if len(snap.Interfaces) == 0 {
		t.Fatal("no interfaces at all; even loopback should be here")
	}
}

// TestInterfacesAreSorted matters more than it looks. The snapshot is hashed
// to decide whether anything CHANGED, and the kernel makes no promise about
// ordering, so an unsorted list would look different on most polls and fill
// the bitemporal fact history with churn that is not change.
func TestInterfacesAreSorted(t *testing.T) {
	got, _ := collectNetwork(context.Background())
	snap := got.(networkSnapshot)
	for i := 1; i < len(snap.Interfaces); i++ {
		if snap.Interfaces[i-1].Name > snap.Interfaces[i].Name {
			t.Fatalf("interfaces out of order at %d: %q then %q",
				i, snap.Interfaces[i-1].Name, snap.Interfaces[i].Name)
		}
	}
	for _, in := range snap.Interfaces {
		for i := 1; i < len(in.Addresses); i++ {
			if in.Addresses[i-1] > in.Addresses[i] {
				t.Fatalf("%s addresses out of order: %v", in.Name, in.Addresses)
			}
		}
	}
}

// TestCollectIsStable is the property the hash depends on: two calls with no
// change between them must produce identical output.
func TestCollectIsStable(t *testing.T) {
	a, _ := collectNetwork(context.Background())
	b, _ := collectNetwork(context.Background())
	sa, sb := a.(networkSnapshot), b.(networkSnapshot)
	if len(sa.Interfaces) != len(sb.Interfaces) {
		t.Fatalf("interface count changed between calls: %d then %d",
			len(sa.Interfaces), len(sb.Interfaces))
	}
	for i := range sa.Interfaces {
		if sa.Interfaces[i].Name != sb.Interfaces[i].Name {
			t.Fatalf("order changed between calls at %d", i)
		}
	}
}

func TestPrimaryAddress(t *testing.T) {
	cases := []struct {
		name string
		in   []NetInterface
		want string
		why  string
	}{
		{
			name: "skips loopback",
			in: []NetInterface{
				{Name: "lo", Up: true, Loopback: true, Addresses: []string{"127.0.0.1/8"}},
				{Name: "eth0", Up: true, Addresses: []string{"10.0.2.15/24"}},
			},
			want: "10.0.2.15",
			why:  "loopback is never what anybody means by the machine's IP",
		},
		{
			name: "skips an interface that is down",
			in: []NetInterface{
				{Name: "eth0", Up: false, Addresses: []string{"10.0.0.5/24"}},
				{Name: "eth1", Up: true, Addresses: []string{"10.0.0.6/24"}},
			},
			want: "10.0.0.6",
			why:  "a cable-out NIC keeps its address; reporting it sends people to the wrong box",
		},
		{
			name: "skips link-local",
			in: []NetInterface{
				{Name: "eth0", Up: true, Addresses: []string{"169.254.3.4/16", "192.168.1.10/24"}},
			},
			want: "192.168.1.10",
			why:  "169.254/16 means DHCP failed, which is the opposite of reachable",
		},
		{
			name: "ignores v6 for this purpose",
			in: []NetInterface{
				{Name: "eth0", Up: true, Addresses: []string{"fe80::1/64", "2001:db8::1/64"}},
			},
			want: "",
			why:  "v4 only by design; an empty answer is better than a wrong one",
		},
		{
			name: "nothing usable",
			in:   []NetInterface{{Name: "lo", Up: true, Loopback: true}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PrimaryAddress(tc.in); got != tc.want {
				t.Errorf("PrimaryAddress = %q, want %q; %s", got, tc.want, tc.why)
			}
		})
	}
}
