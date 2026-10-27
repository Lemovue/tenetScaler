//go:build linux

package llama

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ServerImage is the llama-server executable name that Cleanup kills.
const ServerImage = "llama-server"

// clockTicksPerSecond is USER_HZ, the unit of /proc/<pid>/stat times; the
// kernel fixes it at 100 for user space on every architecture.
const clockTicksPerSecond = 100

// setSysProcAttr puts the child in its own process group (a terminal
// Ctrl+C reaches only TenantScaler, which closes the pool) and asks the
// kernel to SIGKILL it when the thread that started it dies.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// enterKillJob has nothing to prepare on Linux: Pdeathsig is part of the
// child's creation.
func enterKillJob() error { return nil }

// protectChild has nothing to do on Linux: Pdeathsig is set at start.
func protectChild(int) error { return nil }

// releaseChild has nothing to do on Linux: the child was not started
// suspended, and a thread's affinity (llama-server's -C) is not limited by
// the parent's process mask there.
func releaseChild(int, uint64) error { return nil }

// llamaServerPIDs lists processes whose /proc/<pid>/comm is llama-server.
func llamaServerPIDs() (map[int]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := map[int]bool{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue // exited meanwhile
		}
		if strings.TrimSpace(string(comm)) == ServerImage {
			pids[pid] = true
		}
	}
	return pids, nil
}

// processStartTime returns when the process with the given PID was
// created: boot time (btime in /proc/stat) plus field 22 (starttime) of
// /proc/<pid>/stat. The resolution is one second.
func processStartTime(pid int) (time.Time, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}
	// Field 2 (comm) is parenthesised and may contain spaces; field 3
	// follows the last ')'. starttime, field 22, is the 20th from there.
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 20 {
		return time.Time{}, fmt.Errorf("/proc/%d/stat: %d fields after comm, want >= 20", pid, len(fields))
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("/proc/%d/stat starttime: %w", pid, err)
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	return boot.Add(time.Duration(ticks) * (time.Second / clockTicksPerSecond)), nil
}

// bootTime reads the boot time from the btime line of /proc/stat.
func bootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("/proc/stat btime: %w", err)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, errors.New("/proc/stat has no btime line")
}
