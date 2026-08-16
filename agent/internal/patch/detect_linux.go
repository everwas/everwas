package patch

// Detect picks the update backend for this host.
//
// Order matters where two managers coexist. dnf wins over apt because a
// host with both is an RPM host with a stray apt package, never the other
// way round. pacman is last and is best effort: Arch is a rolling release
// with no security-only channel and no supported way to pin one package to
// a version, so the backend reports what is pending and installs whatever
// the sync database currently holds.
func Detect() (Manager, error) {
	switch {
	case have("dnf"):
		return &dnfManager{bin: "dnf"}, nil
	case have("dnf5"):
		return &dnfManager{bin: "dnf5"}, nil
	case have("apt-get"):
		return &aptManager{}, nil
	case have("pacman"):
		return &pacmanManager{}, nil
	default:
		return nil, ErrUnsupported
	}
}
