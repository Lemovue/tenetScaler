//go:build !windows

package topology

// Detect is not implemented on this platform.
func Detect() (Topology, error) { return Topology{}, ErrUnsupported }

// PinCurrentProcess is not implemented on this platform.
func PinCurrentProcess(uint64) error { return ErrUnsupported }

// SetProcessAffinity is not implemented on this platform.
func SetProcessAffinity(int, uint64) error { return ErrUnsupported }

// ProcessAffinity is not implemented on this platform.
func ProcessAffinity(int) (uint64, error) { return 0, ErrUnsupported }
