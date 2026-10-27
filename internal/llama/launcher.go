// Package llama runs REAL inference replicas: one llama.cpp llama-server
// process per replica, pinned to its own CPU cores (PLAN.md decisions
// Q5-Q8). A Launcher starts processes; each *Process implements
// pool.Instance, and NewBackend plugs a Launcher into a pool.Pool.
//
// No orphans: before its first launch TenantScaler joins a Windows Job
// Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so every child is in the
// job from creation and the OS kills it when TenantScaler exits, however it
// exits (Linux: Pdeathsig). As a consequence, any process TenantScaler
// starts after that dies with it too. Every PID is also appended to a PID
// file that Cleanup (the 'cleanup' subcommand) uses after a hard crash.
package llama

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/topology"
)

// Defaults and environment variables.
const (
	// DefaultLogDir holds one log file per replica (<id>.log).
	DefaultLogDir = ".run/llama"
	// DefaultPIDFile lists every launched PID for Cleanup.
	DefaultPIDFile = ".run/llama.pids"
	// EnvServer is the fallback path of the llama-server binary.
	EnvServer = "TS_LLAMA_SERVER"
	// EnvModel is the fallback path of the GGUF model.
	EnvModel = "TS_LLAMA_MODEL"
	// DefaultDrainGrace is the minimum time a draining replica is kept
	// alive. It exceeds the gateway's local endpoint poll (500 ms), so every
	// gateway has stopped dispatching to the replica before the idle
	// readings that allow it to be killed.
	DefaultDrainGrace = time.Second
)

// Launcher starts llama-server processes. The zero value of LogDir,
// PIDFile, Logger and DrainGrace selects the defaults. A Launcher is safe for
// concurrent use; do not change its fields while it launches.
type Launcher struct {
	// Spec is the backend.llama section of the configuration.
	Spec config.LlamaSpec
	// LogDir receives <id>.log per replica; "" means DefaultLogDir.
	LogDir string
	// PIDFile receives one "<pid> <id> <time>" line per launch; ""
	// means DefaultPIDFile.
	PIDFile string
	// Logger receives lifecycle events; nil means slog.Default().
	Logger *slog.Logger
	// DrainGrace is the minimum time between BeginDrain and the end of
	// WaitDrained; 0 means DefaultDrainGrace. llama-server has no drain
	// mode and keeps accepting requests, so the grace must exceed the
	// slowest endpoint-discovery interval of any gateway in front of it.
	DrainGrace time.Duration
}

// Launch starts one llama-server for replica id on CPU slot slot and
// returns it in state "starting". It resolves the server and model paths
// (Spec, else $TS_LLAMA_SERVER / $TS_LLAMA_MODEL), picks a free port on
// 127.0.0.1, pins the process to the slot's CPU mask, appends its output
// to LogDir/<id>.log and protects it against becoming an orphan.
func (l *Launcher) Launch(id string, slot int) (*Process, error) {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\:`) {
		return nil, fmt.Errorf("llama: replica id %q must be a plain file name", id)
	}
	server, model, err := l.resolve()
	if err != nil {
		return nil, err
	}
	mask, err := l.cpuMask(slot)
	if err != nil {
		return nil, err
	}
	bits, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(mask), "0x"), 16, 64)
	if err != nil || bits == 0 {
		return nil, fmt.Errorf("llama: CPU mask %q of slot %d is not a non-zero hex mask", mask, slot)
	}
	port, err := reservePort()
	if err != nil {
		return nil, err
	}
	args := l.args(model, mask, port)
	p, err := l.start(id, slot, server, args, port, bits)
	if err != nil {
		releasePort(port)
		return nil, err
	}
	return p, nil
}

// Check validates the spec and that the server binary and the model exist,
// without launching anything; use it to fail fast or to fall back to the
// simulated backend.
func (l *Launcher) Check() error {
	_, _, err := l.resolve()
	return err
}

// resolve validates the spec and returns the absolute server and model
// paths.
func (l *Launcher) resolve() (server, model string, err error) {
	if err := (config.BackendSpec{Kind: config.BackendLlama, Llama: l.Spec}).Validate(0); err != nil {
		return "", "", fmt.Errorf("llama: %w", err)
	}
	if server, err = resolvePath("server binary", l.Spec.Server, EnvServer, "backend.llama.server"); err != nil {
		return "", "", err
	}
	if model, err = resolvePath("model", l.Spec.Model, EnvModel, "backend.llama.model"); err != nil {
		return "", "", err
	}
	return server, model, nil
}

// start opens the log file, starts the process and its watchers. The log
// is appended to: replica IDs restart at "<pool>-1" in every run, and the
// timestamped header separates the launches.
func (l *Launcher) start(id string, slot int, server string, args []string, port int, mask uint64) (*Process, error) {
	logDir := cmp.Or(l.LogDir, DefaultLogDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("llama: create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, id+".log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("llama: open log file: %w", err)
	}
	fmt.Fprintf(logf, "# tenantscaler %s launching replica %s (slot %d): %s %s\n",
		time.Now().Format(time.RFC3339), id, slot, server, strings.Join(args, " "))

	cmd := exec.Command(server, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	setSysProcAttr(cmd)
	// Before the child exists: on Windows TenantScaler joins its
	// kill-on-close Job Object, so the child belongs to it from creation.
	jobErr := enterKillJob()
	launched := time.Now()
	startErr := cmd.Start()
	// The child holds its own handle to the log file; ours is not needed.
	closeErr := logf.Close()
	if startErr != nil {
		return nil, fmt.Errorf("llama: start %s: %w", server, startErr)
	}
	logger := cmp.Or(l.Logger, slog.Default()).With("replica", id, "pid", cmd.Process.Pid)
	if closeErr != nil {
		logger.Warn("close parent log handle", "err", closeErr)
	}
	// Record the PID first: it is the fallback when protection fails.
	if err := appendPID(cmp.Or(l.PIDFile, DefaultPIDFile), cmd.Process.Pid, id); err != nil {
		logger.Warn("record pid", "err", err)
	}
	if jobErr == nil {
		jobErr = protectChild(cmd.Process.Pid)
	}
	if jobErr != nil {
		logger.Warn("orphan protection failed; run 'tenantscaler cleanup' after a crash", "err", jobErr)
	}
	if err := releaseChild(cmd.Process.Pid, mask); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("llama: pin replica %s to CPUs %s: %w", id, topology.MaskString(mask), err)
	}
	p := newProcess(processOpts{
		id: id, slot: slot, port: port, cmd: cmd, logPath: logPath, log: logger, launched: launched,
		readyTimeout: time.Duration(l.Spec.ReadyTimeoutSeconds * float64(time.Second)),
		drainGrace:   cmp.Or(l.DrainGrace, DefaultDrainGrace),
		jobErr:       jobErr,
	})
	logger.Info("llama-server launched", "url", p.url, "slot", slot, "log", logPath)
	return p, nil
}

// args builds the llama-server command line (DESIGN.md section 16).
func (l *Launcher) args(model, mask string, port int) []string {
	s := l.Spec
	args := []string{
		"-m", model,
		"-ngl", strconv.Itoa(s.GPULayers),
		"-t", strconv.Itoa(s.Threads),
		"-C", mask,
		"--cpu-strict", "1",
		"-c", strconv.Itoa(s.Parallel * s.CtxPerSlot),
		"-np", strconv.Itoa(s.Parallel),
		"--metrics",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		// The b11898 default host prompt cache is 8192 MiB per replica, which
		// four replicas could grow past the machine's RAM limits.
		"--cache-ram", strconv.Itoa(s.CacheRAMMiB),
	}
	if s.GPULayers == 0 {
		args = append(args, "-dev", "none") // keep the GPU out entirely
	}
	return append(args, s.ExtraArgs...)
}

// detectTopology reads the CPU cores once per process.
var detectTopology = sync.OnceValues(topology.Detect)

// cpuMask returns the hex affinity mask of a slot: Spec.CPUMasks[slot] when
// masks are configured; else the slot-th physical P-core of the detected
// topology, both hardware threads (decision Q13); where detection is not
// implemented, DefaultCPUMask(slot).
func (l *Launcher) cpuMask(slot int) (string, error) {
	if slot < 0 {
		return "", fmt.Errorf("llama: slot %d is negative", slot)
	}
	if masks := l.Spec.CPUMasks; len(masks) > 0 {
		if slot >= len(masks) {
			return "", fmt.Errorf("llama: slot %d has no entry in backend.llama.cpuMasks (%d masks)", slot, len(masks))
		}
		return masks[slot], nil
	}
	topo, err := detectTopology()
	switch {
	case err == nil:
		masks, err := topo.ReplicaMasks(slot + 1)
		if err != nil {
			return "", fmt.Errorf("llama: slot %d needs a P-core of its own: %w; lower the pool capacity or set backend.llama.cpuMasks", slot, err)
		}
		return topology.HexMask(masks[slot]), nil
	case !errors.Is(err, topology.ErrUnsupported):
		return "", fmt.Errorf("llama: detect CPU cores: %w", err)
	}
	if need := 2*slot + 2; need > runtime.NumCPU() {
		return "", fmt.Errorf("llama: slot %d needs logical CPUs %d-%d but this machine has %d; lower the pool capacity or set backend.llama.cpuMasks",
			slot, 2*slot, 2*slot+1, runtime.NumCPU())
	}
	return DefaultCPUMask(slot), nil
}

// DefaultCPUMask is the affinity mask of CPU slot k: hex 3 << (2k), the two
// SMT threads of one physical core when siblings are numbered adjacently
// (slot 0 = "3", 1 = "c", 2 = "30", 3 = "c0").
func DefaultCPUMask(slot int) string {
	return new(big.Int).Lsh(big.NewInt(3), uint(2*slot)).Text(16)
}

// resolvePath picks the configured path, else the environment variable, and
// checks that the file exists.
func resolvePath(what, configured, env, field string) (string, error) {
	path := configured
	if path == "" {
		path = os.Getenv(env)
	}
	if path == "" {
		return "", fmt.Errorf("llama: no %s configured: set %s or $%s (scripts/setup-llama.ps1 downloads it)", what, field, env)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("llama: %s path %q: %w", what, path, err)
	}
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("llama: %s %q does not exist (set %s or $%s)", what, abs, field, env)
	case err != nil:
		return "", fmt.Errorf("llama: %s %q: %w", what, abs, err)
	case info.IsDir():
		return "", fmt.Errorf("llama: %s %q is a directory, want a file", what, abs)
	}
	return abs, nil
}

// ports reserves the ports handed to live processes, so two launches in
// this process never pick the same one between probe and bind.
var ports = struct {
	sync.Mutex
	inUse map[int]bool
}{inUse: map[int]bool{}}

// reservePort finds a free 127.0.0.1 port by binding port 0.
func reservePort() (int, error) {
	for range 16 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, fmt.Errorf("llama: find a free port: %w", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if err := ln.Close(); err != nil {
			return 0, fmt.Errorf("llama: release probe port: %w", err)
		}
		ports.Lock()
		taken := ports.inUse[port]
		ports.inUse[port] = true
		ports.Unlock()
		if !taken {
			return port, nil
		}
	}
	return 0, errors.New("llama: no free port found")
}

// releasePort makes a port available again once its process has exited.
func releasePort(port int) {
	ports.Lock()
	defer ports.Unlock()
	delete(ports.inUse, port)
}

// NewBackend adapts a Launcher to pool.Backend, so that
// pool.New(pool.Config{Backend: llama.NewBackend(l), ...}) hosts real
// llama-server replicas.
func NewBackend(l *Launcher) pool.Backend { return backend{l} }

type backend struct{ l *Launcher }

func (b backend) Name() string    { return config.BackendLlama }
func (b backend) Simulated() bool { return false }

func (b backend) Launch(id string, slot int) (pool.Instance, error) {
	p, err := b.l.Launch(id, slot)
	if err != nil {
		return nil, err // never a typed nil inside the interface
	}
	return p, nil
}
