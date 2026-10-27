package llama

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/pool"
	"tenantscaler/internal/replica"
)

// Compile-time checks: a Process is a pool instance with a cold start.
var (
	_ pool.Instance    = (*Process)(nil)
	_ pool.ColdStarter = (*Process)(nil)
)

// TestProcessLifecycle runs the fake llama-server through the whole
// lifecycle: starting while "loading", ready with a measured cold start,
// streaming, draining while a request is in flight, and Close.
func TestProcessLifecycle(t *testing.T) {
	const load = 300 * time.Millisecond
	t.Setenv(envFakeLoad, load.String())
	l := fakeLauncher(t)
	p := launch(t, l, "r-1", 1)
	if st := p.State(); st != replica.StateStarting {
		t.Fatalf("state right after Launch = %q, want starting", st)
	}
	if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") || p.Slot() != 1 || p.PID() <= 0 {
		t.Fatalf("url %q slot %d pid %d", p.URL(), p.Slot(), p.PID())
	}
	if p.jobErr != nil {
		t.Fatalf("orphan protection: %v", p.jobErr)
	}
	waitFor(t, 30*time.Second, "ready", func() bool { return p.State() == replica.StateReady })
	if cs := p.ColdStart(); cs < load {
		t.Fatalf("cold start %v, want >= the %v load time", cs, load)
	}
	if s := p.Stats(); s != (replica.Stats{}) {
		t.Fatalf("idle stats %+v", s)
	}

	ctx := context.Background()
	c, err := streamCompletion(ctx, p.URL(), "hello fake world", 8)
	if err != nil || c.CompletionTokens != 8 || c.TextChunks != 8 || c.FinishReason != "length" || !c.Done {
		t.Fatalf("completion %+v, %v", c, err)
	}

	done := make(chan error, 1)
	go func() {
		c, err := streamCompletion(ctx, p.URL(), "long request", 40)
		if err == nil && c.CompletionTokens != 40 {
			err = fmt.Errorf("got %d tokens, want 40", c.CompletionTokens)
		}
		done <- err
	}()
	waitFor(t, 5*time.Second, "request running", func() bool { return p.Stats().Running == 1 })
	p.BeginDrain()
	began := time.Now()
	if st := p.State(); st != replica.StateDraining {
		t.Fatalf("state after BeginDrain = %q", st)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.WaitDrained(dctx); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited < DefaultDrainGrace {
		t.Fatalf("drained after %v, before the default %v grace", waited, DefaultDrainGrace)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("in-flight request during drain: %v", err)
		}
	default:
		t.Fatal("WaitDrained returned before the in-flight request finished")
	}

	pid := p.PID()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if st := p.State(); st != replica.StateStopped || p.Err() != nil {
		t.Fatalf("after Close: state %q err %v", st, p.Err())
	}
	assertGone(t, pid)
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	logData, err := os.ReadFile(p.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"launching replica r-1 (slot 1)", "fake llama-server args:", "-C c --cpu-strict 1", "--metrics", "-dev none"} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("log %s lacks %q:\n%s", p.LogPath(), want, logData)
		}
	}
	pids, err := readPIDs(l.PIDFile)
	if err != nil || len(pids) != 1 || pids[0] != pid {
		t.Fatalf("pid file: %v, %v; want [%d]", pids, err, pid)
	}
}

func TestProcessCrashIsStopped(t *testing.T) {
	t.Setenv(envFakeMode, "crash")
	p := launch(t, fakeLauncher(t), "crash-1", 0)
	waitFor(t, 30*time.Second, "stopped", func() bool { return p.State() == replica.StateStopped })
	if err := p.Err(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Err() = %v, want the exit status", err)
	}
	if s := p.Stats(); s != (replica.Stats{}) {
		t.Fatalf("stats of a crashed process %+v", s)
	}
	if err := p.WaitDrained(context.Background()); err != nil {
		t.Fatalf("WaitDrained on an exited process: %v", err)
	}
}

func TestProcessReadyTimeout(t *testing.T) {
	t.Setenv(envFakeMode, "never-ready")
	l := fakeLauncher(t)
	l.Spec.ReadyTimeoutSeconds = 0.3
	p := launch(t, l, "slow-1", 0)
	waitFor(t, 30*time.Second, "stopped", func() bool { return p.State() == replica.StateStopped })
	if err := p.Err(); err == nil || !strings.Contains(err.Error(), "not ready within 300ms") {
		t.Fatalf("Err() = %v", err)
	}
	assertGone(t, p.PID())
}

// TestDrainWhileStarting checks that a replica drained before it became
// ready is drained at once: it never received traffic.
func TestDrainWhileStarting(t *testing.T) {
	t.Setenv(envFakeMode, "never-ready")
	p := launch(t, fakeLauncher(t), "early-1", 0)
	p.BeginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.WaitDrained(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestOrphanProtection kills a parent process that launched a replica
// without closing it, as a crash would, and checks that the operating
// system then kills the replica (Job Object on Windows, Pdeathsig on
// Linux).
func TestOrphanProtection(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("orphan protection exists on Windows and Linux only")
	}
	l := fakeLauncher(t) // for its temporary model file
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	parent := exec.Command(exe)
	parent.Env = append(os.Environ(), envFakeParent+"=1", envFakeDir+"="+dir,
		EnvServer+"="+fakeServerPath, EnvModel+"="+l.Spec.Model)
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read parent output: %v (%q)", err, line)
	}
	pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "pid ")
	child, convErr := strconv.Atoi(pidText)
	if !ok || convErr != nil {
		t.Fatalf("parent said %q", line)
	}
	alive, err := llamaServerPIDs()
	if err != nil || !alive[child] {
		t.Fatalf("replica %d not running before the parent dies: %v", child, err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	waitFor(t, 10*time.Second, "orphaned replica killed by the OS", func() bool {
		alive, err := llamaServerPIDs()
		return err == nil && !alive[child]
	})
}
