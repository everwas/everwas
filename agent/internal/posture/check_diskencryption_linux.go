package posture

import (
	"context"
	"encoding/json"
	"strings"
)

// diskEncryptionCheck reports whether the filesystem holding the OS is on
// encrypted storage.
//
// The question people mean when they ask "is this disk encrypted" is about the
// data at rest if the machine is stolen, so what matters is the root
// filesystem's backing device, not whether any LUKS volume exists anywhere. A
// machine with an encrypted spare disk and a plaintext root is not encrypted.
type diskEncryptionCheck struct{}

func (diskEncryptionCheck) Name() string { return "disk-encryption" }

func (diskEncryptionCheck) Category() Category { return CategoryEncryption }

// lsblkNode is the subset of `lsblk --json` we read.
type lsblkNode struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Mountpoint *string     `json:"mountpoint"`
	Children   []lsblkNode `json:"children"`
}

func (c diskEncryptionCheck) Run(ctx context.Context) Result {
	const name = "disk-encryption"

	out, err := output(ctx, "lsblk", "--json", "-o", "NAME,TYPE,MOUNTPOINT")
	if err != nil {
		return unknown(name, "lsblk is unavailable, so the root device could not be traced")
	}

	var tree struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		return unknown(name, "lsblk output could not be parsed")
	}

	encrypted, found := rootIsEncrypted(tree.Blockdevices, nil)
	if !found {
		// No node claims to be mounted at /. Happens inside containers and on
		// exotic root setups (NFS root, overlay). Not a failure: we did not
		// find the thing we were asked about.
		return unknown(name, "the root filesystem's backing device could not be identified")
	}
	if encrypted {
		return pass(name, "the root filesystem is on an encrypted volume",
			map[string]string{"mechanism": "luks"})
	}
	return fail(name, "the root filesystem is on unencrypted storage", nil)
}

// rootIsEncrypted walks the block device tree looking for the node mounted at
// "/" and reports whether any ancestor of it is a crypt device.
//
// Ancestor, not the node itself: with LUKS the mounted filesystem sits on a
// dm-crypt mapping whose PARENT is the crypt layer, so checking only the
// mounted node's own type finds nothing on a correctly encrypted machine.
func rootIsEncrypted(nodes []lsblkNode, ancestors []lsblkNode) (encrypted, found bool) {
	for _, n := range nodes {
		if n.Mountpoint != nil && strings.TrimSpace(*n.Mountpoint) == "/" {
			if n.Type == "crypt" {
				return true, true
			}
			for _, a := range ancestors {
				if a.Type == "crypt" {
					return true, true
				}
			}
			return false, true
		}
		if enc, ok := rootIsEncrypted(n.Children, append(ancestors, n)); ok {
			return enc, true
		}
	}
	return false, false
}
