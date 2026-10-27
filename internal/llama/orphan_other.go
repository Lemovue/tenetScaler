//go:build !windows && !linux

package llama

import (
	"errors"
	"os/exec"
	"time"
)

// ServerImage is the llama-server executable name that Cleanup kills.
const ServerImage = "llama-server"

// errUnsupported reports a process operation this platform lacks.
var errUnsupported = errors.New("listing and inspecting llama-server processes is supported on Windows and Linux only")

// setSysProcAttr has no orphan protection to configure on this platform.
func setSysProcAttr(*exec.Cmd) {}

// enterKillJob has no orphan protection to prepare on this platform.
func enterKillJob() error { return nil }

// protectChild has no orphan protection on this platform; Pool.Close and
// the PID file remain.
func protectChild(int) error { return nil }

// releaseChild has nothing to do here: the child was not started suspended
// and llama-server's -C option pins its threads.
func releaseChild(int, uint64) error { return nil }

// llamaServerPIDs is not implemented on this platform.
func llamaServerPIDs() (map[int]bool, error) { return nil, errUnsupported }

// processStartTime is not implemented on this platform.
func processStartTime(int) (time.Time, error) { return time.Time{}, errUnsupported }
