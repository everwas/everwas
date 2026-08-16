package patch

// Detect returns the macOS backend. softwareupdate ships with the OS, so
// its absence means something is badly wrong with the host rather than
// that a different backend should be tried.
func Detect() (Manager, error) {
	if !have("softwareupdate") {
		return nil, ErrUnsupported
	}
	return &softwareUpdateManager{}, nil
}
