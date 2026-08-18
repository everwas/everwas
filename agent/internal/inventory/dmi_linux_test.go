//go:build linux

package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadDMIFromSysfsShape(t *testing.T) {
	dir := t.TempDir()
	// sysfs values carry a trailing newline; the reader must trim it.
	writeFixture(t, dir, "sys_vendor", "LENOVO\n")
	writeFixture(t, dir, "product_name", "21CB000JUS\n")
	writeFixture(t, dir, "product_serial", "PF3K2ABC\n")
	writeFixture(t, dir, "chassis_type", "10\n")

	got := readDMI(dir)
	want := dmiInfo{
		Manufacturer: "LENOVO",
		Model:        "21CB000JUS",
		Serial:       "PF3K2ABC",
		Chassis:      "laptop",
	}
	if got != want {
		t.Errorf("readDMI = %+v, want %+v", got, want)
	}
}

func TestReadDMIJunkSerialIsBlanked(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "sys_vendor", "ASUS\n")
	writeFixture(t, dir, "product_name", "System Product Name\n")
	writeFixture(t, dir, "product_serial", "To Be Filled By O.E.M.\n")
	writeFixture(t, dir, "chassis_type", "3\n")

	got := readDMI(dir)
	if got.Serial != "" {
		t.Errorf("junk serial survived: %q", got.Serial)
	}
	if got.Model != "" {
		t.Errorf("junk model survived: %q", got.Model)
	}
	if got.Manufacturer != "ASUS" || got.Chassis != "desktop" {
		t.Errorf("real fields lost: %+v", got)
	}
}

func TestReadDMIMissingDirIsEmptyNotError(t *testing.T) {
	// ARM SBCs and some VMs have no /sys/class/dmi/id at all. The machine
	// has nothing to say, and the snapshot must still succeed.
	got := readDMI(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != (dmiInfo{}) {
		t.Errorf("expected zero value, got %+v", got)
	}
}

func TestReadDMIUnreadableSerialLosesOnlySerial(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "sys_vendor", "Dell Inc.\n")
	writeFixture(t, dir, "product_name", "OptiPlex 7090\n")
	writeFixture(t, dir, "chassis_type", "15\n")
	// product_serial is 0400 root-only on real systems; simulate by absence
	// plus an unreadable file variant when not running as root.
	writeFixture(t, dir, "product_serial", "SECRET1")
	if err := os.Chmod(filepath.Join(dir, "product_serial"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not apply")
	}

	got := readDMI(dir)
	if got.Serial != "" {
		t.Errorf("expected empty serial without permission, got %q", got.Serial)
	}
	if got.Manufacturer != "Dell Inc." || got.Model != "OptiPlex 7090" || got.Chassis != "desktop" {
		t.Errorf("other fields lost: %+v", got)
	}
}
