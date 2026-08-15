package telemetry

import "testing"

func TestRealFS(t *testing.T) {
	cases := []struct {
		fstype string
		want   bool
	}{
		{"ext4", true},
		{"xfs", true},
		{"btrfs", true},
		{"zfs", true},
		{"apfs", true},
		{"hfs", true},
		{"ntfs", true},
		{"NTFS", true}, // Windows reports uppercase
		{"FAT32", true},
		{"exfat", true},
		{"vfat", true},
		{"tmpfs", false},
		{"proc", false},
		{"sysfs", false},
		{"devtmpfs", false},
		{"overlay", false},
		{"squashfs", false},
		{"cgroup2", false},
		{"nfs4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := realFS(c.fstype); got != c.want {
			t.Errorf("realFS(%q) = %v, want %v", c.fstype, got, c.want)
		}
	}
}
