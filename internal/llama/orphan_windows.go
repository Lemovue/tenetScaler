//go:build windows

package llama

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"tenantscaler/internal/topology"
)

// ServerImage is the llama-server executable name that Cleanup kills.
const ServerImage = "llama-server.exe"

// Win32 constants (winnt.h, processthreadsapi.h).
const (
	jobObjectExtendedLimitInformation = 9      // JOBOBJECTINFOCLASS
	jobObjectLimitKillOnJobClose      = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	processSetQuota                   = 0x0100 // PROCESS_SET_QUOTA
	processSuspendResume              = 0x0800 // PROCESS_SUSPEND_RESUME
	createSuspended                   = 0x0004 // CREATE_SUSPENDED
	processTerminate                  = 0x0001 // PROCESS_TERMINATE
	processQueryLimitedInformation    = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procIsProcessInJob           = kernel32.NewProc("IsProcessInJob")
	procNtResumeProcess          = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

// jobBasicLimitInformation is JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// ioCounters is IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

// jobExtendedLimitInformation is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// killJob is the process-wide Job Object. Its only handle is never closed
// explicitly: Windows closes it when TenantScaler exits, however it exits,
// and JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE then kills every replica.
// TenantScaler itself joins the job (selfIn), so every child it starts
// afterwards is in the job from the moment it is created.
var killJob struct {
	once   sync.Once
	handle syscall.Handle
	err    error
	selfIn bool
}

// jobHandle creates the kill-on-close Job Object once and makes the current
// process a member. Joining fails only inside an outer job that forbids
// nesting; protectChild then assigns each child right after its start.
func jobHandle() (syscall.Handle, error) {
	killJob.once.Do(func() {
		h, _, err := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			killJob.err = fmt.Errorf("CreateJobObjectW: %w", err)
			return
		}
		var info jobExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		ok, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ok == 0 {
			_ = syscall.CloseHandle(syscall.Handle(h))
			killJob.err = fmt.Errorf("SetInformationJobObject(KILL_ON_JOB_CLOSE): %w", err)
			return
		}
		killJob.handle = syscall.Handle(h)
		self, err := syscall.GetCurrentProcess()
		if err == nil {
			ok, _, _ := procAssignProcessToJobObject.Call(h, uintptr(self))
			killJob.selfIn = ok != 0
		}
	})
	return killJob.handle, killJob.err
}

// enterKillJob prepares orphan protection before a child is started: it
// creates the kill-on-close Job Object and puts TenantScaler in it, so a
// crash between process creation and protectChild cannot leave the child
// outside the job.
func enterKillJob() error {
	_, err := jobHandle()
	return err
}

// setSysProcAttr starts the child in its own process group, so a console
// Ctrl+C reaches TenantScaler (which then drains and closes the pool) but
// not the replicas directly, and suspended, so releaseChild can pin it
// before it runs.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createSuspended}
}

// releaseChild sets the suspended child's process affinity to mask and
// resumes it. A child inherits TenantScaler's affinity, which may be pinned
// to the service CPUs (PinServices); llama-server's own -C thread affinity
// must lie inside its process affinity, so the process mask is set first.
func releaseChild(pid int, mask uint64) error {
	h, err := syscall.OpenProcess(processSuspendResume|processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	pinErr := topology.SetProcessAffinity(pid, mask)
	if status, _, _ := procNtResumeProcess.Call(uintptr(h)); status != 0 {
		return errors.Join(pinErr, fmt.Errorf("NtResumeProcess(%d): NTSTATUS 0x%x", pid, status))
	}
	return pinErr
}

// protectChild makes sure the started child is in the kill-on-close Job
// Object: it normally joined at creation (enterKillJob); otherwise it is
// assigned now. llama-server starts no processes of its own, so nothing is
// left outside the job.
func protectChild(pid int) error {
	job, err := jobHandle()
	if err != nil {
		return err
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate|processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	if in, err := inJob(h, job); err == nil && in {
		return nil
	}
	if ok, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); ok == 0 {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// inJob reports whether the process behind h is a member of job.
func inJob(h, job syscall.Handle) (bool, error) {
	var in int32
	if ok, _, err := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&in))); ok == 0 {
		return false, fmt.Errorf("IsProcessInJob: %w", err)
	}
	return in != 0, nil
}

// processStartTime returns when the process with the given PID was
// created.
func processStartTime(pid int) (time.Time, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return time.Time{}, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, fmt.Errorf("GetProcessTimes(%d): %w", pid, err)
	}
	return time.Unix(0, created.Nanoseconds()), nil
}

// llamaServerPIDs lists running llama-server.exe processes with tasklist.
func llamaServerPIDs() (map[int]bool, error) {
	tasklist := "tasklist"
	if root := os.Getenv("SystemRoot"); root != "" {
		tasklist = filepath.Join(root, "System32", "tasklist.exe")
	}
	out, err := exec.Command(tasklist, "/FI", "IMAGENAME eq "+ServerImage, "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil, fmt.Errorf("tasklist: %w", err)
	}
	return parseTasklist(out)
}

// parseTasklist reads `tasklist /FO CSV /NH` output: one quoted row per
// process ("image","pid",...). The localised "no tasks" message is not a
// quoted row and yields an empty set.
func parseTasklist(out []byte) (map[int]bool, error) {
	pids := map[int]bool{}
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '"' {
		return pids, nil
	}
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse tasklist output: %w", err)
	}
	for _, row := range rows {
		if len(row) < 2 || !strings.EqualFold(row[0], ServerImage) {
			continue
		}
		pid, err := strconv.Atoi(row[1])
		if err != nil {
			return nil, fmt.Errorf("parse tasklist pid %q: %w", row[1], err)
		}
		pids[pid] = true
	}
	return pids, nil
}
