//go:build windows

package topology

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	errorInsufficientBuffer        = 122    // ERROR_INSUFFICIENT_BUFFER
	processSetInformation          = 0x0200 // PROCESS_SET_INFORMATION
	processQueryLimitedInformation = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalProcessorInfo = kernel32.NewProc("GetLogicalProcessorInformationEx")
	procSetProcessAffinityMask  = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask  = kernel32.NewProc("GetProcessAffinityMask")
)

// Detect reads the physical cores with GetLogicalProcessorInformationEx.
func Detect() (Topology, error) {
	var n uint32
	r, _, err := procGetLogicalProcessorInfo.Call(relationProcessorCore, 0, uintptr(unsafe.Pointer(&n)))
	if r != 0 || n == 0 {
		return Topology{}, fmt.Errorf("GetLogicalProcessorInformationEx size query: %w", err)
	}
	if !errors.Is(err, syscall.Errno(errorInsufficientBuffer)) {
		return Topology{}, fmt.Errorf("GetLogicalProcessorInformationEx size query: %w", err)
	}
	buf := make([]byte, n)
	if r, _, err := procGetLogicalProcessorInfo.Call(relationProcessorCore, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return Topology{}, fmt.Errorf("GetLogicalProcessorInformationEx: %w", err)
	}
	return parseCores(buf[:n])
}

// PinCurrentProcess restricts this process to the logical processors in
// mask. Child processes inherit it; see SetProcessAffinity.
func PinCurrentProcess(mask uint64) error {
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	return setAffinity(self, mask)
}

// SetProcessAffinity sets another process's affinity mask, which may
// include processors outside this process's own mask.
func SetProcessAffinity(pid int, mask uint64) error {
	h, err := syscall.OpenProcess(processSetInformation|processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	return setAffinity(h, mask)
}

// ProcessAffinity returns a process's affinity mask (pid 0: this process).
func ProcessAffinity(pid int) (uint64, error) {
	h, err := syscall.GetCurrentProcess()
	if pid != 0 {
		if h, err = syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)); err != nil {
			return 0, fmt.Errorf("OpenProcess(%d): %w", pid, err)
		}
		defer syscall.CloseHandle(h)
	}
	if err != nil {
		return 0, err
	}
	var proc, system uintptr
	if ok, _, err := procGetProcessAffinityMask.Call(uintptr(h), uintptr(unsafe.Pointer(&proc)), uintptr(unsafe.Pointer(&system))); ok == 0 {
		return 0, fmt.Errorf("GetProcessAffinityMask: %w", err)
	}
	return uint64(proc), nil
}

func setAffinity(h syscall.Handle, mask uint64) error {
	if ok, _, err := procSetProcessAffinityMask.Call(uintptr(h), uintptr(mask)); ok == 0 {
		return fmt.Errorf("SetProcessAffinityMask(%s): %w", MaskString(mask), err)
	}
	return nil
}
