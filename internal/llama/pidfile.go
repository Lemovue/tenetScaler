package llama

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// cleanupWait bounds how long Cleanup waits for killed processes to
	// vanish.
	cleanupWait = 5 * time.Second
	// launchTimeSlack is how far a process's creation time may lie from
	// the launch time recorded for its PID for Cleanup to treat it as the
	// recorded process. The record is written right after the start (the
	// one-second resolution of Linux start times and of older records
	// without fractional seconds included); a process that reused the PID
	// was created after the recorded one had exited.
	launchTimeSlack = 3 * time.Second
)

// pidFileMu serialises PID-file appends and Cleanup within this process.
var pidFileMu sync.Mutex

// pidEntry is one launched process from the PID file.
type pidEntry struct {
	pid int
	// launched is the recorded launch time; zero for a line without one.
	launched time.Time
}

// appendPID records a launched process as "<pid> <id> <RFC 3339 time>".
func appendPID(path string, pid int, id string) error {
	pidFileMu.Lock()
	defer pidFileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create pid file dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open pid file: %w", err)
	}
	_, werr := fmt.Fprintf(f, "%d %s %s\n", pid, id, time.Now().Format(time.RFC3339Nano))
	return errors.Join(werr, f.Close())
}

// CleanupResult reports what Cleanup found and did.
type CleanupResult struct {
	// Listed is the number of distinct PIDs in the PID file.
	Listed int
	// Killed lists the PIDs that were still running llama-server and were
	// killed.
	Killed []int
	// Skipped lists PIDs that run a llama-server created at another time
	// than the recorded launch: the PID was reused by a process that is
	// not ours (or its creation time could not be read), so it was left
	// alone.
	Skipped []int
}

// Cleanup kills every process listed in pidFile that is still alive, is a
// llama-server (checked by image name) AND was created when the file says
// it was launched, waits for them to vanish, and truncates the file. A
// reused PID, whether of another program or of a llama-server that is not
// ours, is never touched; lines without a launch time are checked by image
// name only. A missing file means there is nothing to do. Run it only when
// no TenantScaler process is running: it would kill that process's
// replicas.
func Cleanup(pidFile string) (CleanupResult, error) {
	pidFileMu.Lock()
	defer pidFileMu.Unlock()
	entries, err := readPIDEntries(pidFile)
	if errors.Is(err, os.ErrNotExist) {
		return CleanupResult{}, nil
	}
	if err != nil {
		return CleanupResult{}, err
	}
	res := CleanupResult{Listed: len(entries)}
	if len(entries) > 0 {
		alive, err := llamaServerPIDs()
		if err != nil {
			return res, fmt.Errorf("list running llama-server processes (pid file kept): %w", err)
		}
		var errs []error
		for _, e := range entries {
			if !alive[e.pid] {
				continue
			}
			if !e.launched.IsZero() {
				created, err := processStartTime(e.pid)
				if err != nil || !sameLaunch(created, e.launched) {
					res.Skipped = append(res.Skipped, e.pid)
					continue
				}
			}
			if err := killPID(e.pid); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Killed = append(res.Killed, e.pid)
		}
		if err := waitGone(res.Killed, cleanupWait); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return res, fmt.Errorf("pid file kept: %w", errors.Join(errs...))
		}
	}
	if err := os.WriteFile(pidFile, nil, 0o644); err != nil {
		return res, fmt.Errorf("truncate pid file: %w", err)
	}
	return res, nil
}

// sameLaunch reports whether a process created at created is the one
// recorded as launched at launched.
func sameLaunch(created, launched time.Time) bool {
	d := created.Sub(launched)
	return d > -launchTimeSlack && d < launchTimeSlack
}

// readPIDEntries returns the distinct PIDs of a PID file in order of first
// appearance. A PID's launch time is the one on its last line that carries
// a valid time, because a later line describes a later process that reused
// the PID. Blank lines and lines that do not start with a positive integer
// are skipped.
func readPIDEntries(path string) ([]pidEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []pidEntry
	index := map[int]int{} // pid -> position in entries
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		i, seen := index[pid]
		if !seen {
			i = len(entries)
			index[pid] = i
			entries = append(entries, pidEntry{pid: pid})
		}
		if len(fields) >= 3 {
			if t, err := time.Parse(time.RFC3339, fields[2]); err == nil {
				entries[i].launched = t
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read pid file: %w", err)
	}
	return entries, nil
}

// killPID kills one process by PID.
func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	defer proc.Release()
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}

// waitGone polls until none of pids is a running llama-server.
func waitGone(pids []int, timeout time.Duration) error {
	if len(pids) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		alive, err := llamaServerPIDs()
		if err != nil {
			return err
		}
		var left []int
		for _, pid := range pids {
			if alive[pid] {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama-server pids %v still running %s after kill", left, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
