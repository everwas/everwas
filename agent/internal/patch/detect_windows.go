package patch

// Detect returns the Windows Update Agent backend. The COM thread is
// started lazily on first use, so detection itself never blocks.
func Detect() (Manager, error) {
	return newWUAManager(), nil
}
