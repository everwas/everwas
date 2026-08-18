//go:build linux

package posture

import (
	"encoding/json"
	"testing"
)

func TestNftablesRulesAreDistinguishedFromAnEmptyRuleset(t *testing.T) {
	// The distinction that matters: nft being installed proves nothing. A
	// ruleset with tables and chains but no rules permits everything, and
	// counting the tool's presence as a pass would report an open machine as
	// firewalled.
	for _, tc := range []struct {
		name    string
		ruleset string
		want    bool
	}{
		{"nothing at all", "", false},
		{
			"declared but empty, which permits everything",
			`table inet filter {
	chain input {
		type filter hook input priority 0; policy accept;
	}
}`,
			false,
		},
		{
			"an actual rule",
			`table inet filter {
	chain input {
		type filter hook input priority 0; policy drop;
		ct state established,related accept
	}
}`,
			true,
		},
	} {
		if got := hasRules(tc.ruleset); got != tc.want {
			t.Errorf("%s: hasRules = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRootEncryptionLooksAtTheWholeDeviceStack(t *testing.T) {
	// The mistake this guards: with LUKS the mounted filesystem sits ON a
	// dm-crypt mapping, so the node mounted at / is of type "lvm" or "part"
	// and its PARENT is the crypt layer. A check that only looks at the
	// mounted node's own type finds nothing on a correctly encrypted machine
	// and reports it as unencrypted.
	for _, tc := range []struct {
		name          string
		lsblk         string
		wantEncrypted bool
		wantFound     bool
	}{
		{
			name: "plain ext4 root, genuinely unencrypted",
			lsblk: `{"blockdevices":[
				{"name":"vda","type":"disk","mountpoint":null,"children":[
					{"name":"vda1","type":"part","mountpoint":"/"}]}]}`,
			wantEncrypted: false, wantFound: true,
		},
		{
			name: "LUKS root: the crypt layer is an ANCESTOR, not the mounted node",
			lsblk: `{"blockdevices":[
				{"name":"nvme0n1","type":"disk","mountpoint":null,"children":[
					{"name":"nvme0n1p3","type":"part","mountpoint":null,"children":[
						{"name":"cryptroot","type":"crypt","mountpoint":null,"children":[
							{"name":"vg-root","type":"lvm","mountpoint":"/"}]}]}]}]}`,
			wantEncrypted: true, wantFound: true,
		},
		{
			name: "encrypted spare disk, plaintext root: NOT an encrypted machine",
			lsblk: `{"blockdevices":[
				{"name":"vda","type":"disk","mountpoint":null,"children":[
					{"name":"vda1","type":"part","mountpoint":"/"}]},
				{"name":"vdb","type":"disk","mountpoint":null,"children":[
					{"name":"cryptdata","type":"crypt","mountpoint":"/data"}]}]}`,
			wantEncrypted: false, wantFound: true,
		},
		{
			name: "nothing mounted at root, so we did not find out",
			lsblk: `{"blockdevices":[
				{"name":"vda","type":"disk","mountpoint":null,"children":[
					{"name":"vda1","type":"part","mountpoint":"/boot"}]}]}`,
			wantEncrypted: false, wantFound: false,
		},
	} {
		var tree struct {
			Blockdevices []lsblkNode `json:"blockdevices"`
		}
		if err := json.Unmarshal([]byte(tc.lsblk), &tree); err != nil {
			t.Fatalf("%s: fixture does not parse: %v", tc.name, err)
		}
		enc, found := rootIsEncrypted(tree.Blockdevices, nil)
		if enc != tc.wantEncrypted || found != tc.wantFound {
			t.Errorf("%s: encrypted=%v found=%v, want encrypted=%v found=%v",
				tc.name, enc, found, tc.wantEncrypted, tc.wantFound)
		}
	}
}

func TestAnUnidentifiableRootIsUnknownNotAFailure(t *testing.T) {
	// Containers, NFS root and overlay roots all produce an lsblk tree with
	// nothing mounted at "/". That is a machine we could not assess, and
	// reporting it as unencrypted would fail every containerised workload for
	// a property that does not apply to it the way the question assumes.
	var tree struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	_ = json.Unmarshal([]byte(`{"blockdevices":[]}`), &tree)
	if _, found := rootIsEncrypted(tree.Blockdevices, nil); found {
		t.Error("claimed to have found a root device in an empty tree")
	}
}
