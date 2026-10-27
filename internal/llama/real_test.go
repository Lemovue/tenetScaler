package llama

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/pool"
)

// TestRealLlamaServer is the REAL integration test (requirement R26): it
// launches two real llama-server replicas through a pool with the
// config/real.yaml settings (CPU-pinned, -ngl 0 -dev none, 4 slots),
// streams one completion through each, checks that the pool's load
// (parsed from the real /metrics) reflects a live request, drains and
// closes the replicas, and checks that the processes are gone. It is skipped unless $TS_LLAMA_SERVER and
// $TS_LLAMA_MODEL point to existing files (scripts/setup-llama.ps1).
func TestRealLlamaServer(t *testing.T) {
	for _, env := range []string{EnvServer, EnvModel} {
		path := os.Getenv(env)
		if path == "" {
			t.Skipf("set %s and %s to run the real llama-server test", EnvServer, EnvModel)
		}
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s: %v", env, err)
		}
	}
	realCfg, err := config.Load(filepath.Join("..", "..", "config", "real.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := realCfg.Backend.Llama // Server/Model empty: resolved from the environment
	dir := t.TempDir()
	l := &Launcher{Spec: spec, LogDir: dir, PIDFile: filepath.Join(dir, "llama.pids"), Logger: discardLogger}
	p := pool.New(pool.Config{Name: "real", MaxReplicas: 2, Backend: NewBackend(l)})
	t.Cleanup(func() { _ = p.Close() })

	start := time.Now()
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Duration(spec.ReadyTimeoutSeconds*float64(time.Second)), "2 real replicas ready", func() bool {
		return p.Status().Ready == 2
	})
	t.Logf("2 replicas ready after %v", time.Since(start).Round(time.Millisecond))
	st := p.Status()
	if st.Backend != config.BackendLlama || st.Simulated {
		t.Fatalf("status backend %q simulated %v", st.Backend, st.Simulated)
	}
	for i, r := range st.Replicas {
		t.Logf("replica %s slot %d (cpu mask %s) url %s cold start %.2f s", r.ID, r.Slot, DefaultCPUMask(r.Slot), r.URL, r.ColdStartSeconds)
		if r.Slot != i || r.ColdStartSeconds <= 0 {
			t.Fatalf("replica %+v, want slot %d and a measured cold start", r, i)
		}
	}
	pids, err := readPIDs(l.PIDFile)
	if err != nil || len(pids) != 2 {
		t.Fatalf("pid file: %v, %v", pids, err)
	}

	prompts := []string{"Write one sentence about mountains.", "List three colours of the sea."}
	for i, ep := range p.Endpoints() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		c, err := streamCompletion(ctx, ep, prompts[i], 8)
		cancel()
		if err != nil {
			t.Fatalf("stream via %s: %v", ep, err)
		}
		t.Logf("%s: usage.completion_tokens=%d text chunks=%d finish=%q ttft=%v total=%v text=%q",
			ep, c.CompletionTokens, c.TextChunks, c.FinishReason, c.TTFT.Round(time.Millisecond), c.Total.Round(time.Millisecond), c.Text)
		if c.CompletionTokens != 8 || c.TextChunks == 0 || c.FinishReason != "length" || !c.Done {
			t.Fatalf("completion via %s: %+v, want 8 completion tokens ending with length and [DONE]", ep, c)
		}
	}

	// The load parsed from the real /metrics must follow a live request:
	// Running >= 1 while a longer stream runs, 0 once it has finished.
	for _, ep := range p.Endpoints() {
		running := func() int {
			for _, r := range p.Status().Replicas {
				if r.URL == ep {
					return r.Running
				}
			}
			return -1
		}
		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c, err := streamCompletion(ctx, ep, "Count slowly from one to one hundred.", 64)
			if err == nil && c.CompletionTokens != 64 {
				err = fmt.Errorf("%d completion tokens, want 64", c.CompletionTokens)
			}
			done <- err
		}()
		waitFor(t, 30*time.Second, "running >= 1 on "+ep, func() bool { return running() >= 1 })
		if err := <-done; err != nil {
			t.Fatalf("long stream via %s: %v", ep, err)
		}
		waitFor(t, 5*time.Second, "running == 0 on "+ep, func() bool { return running() == 0 })
		t.Logf("%s: /metrics showed the live request, then idle", ep)
	}

	if err := p.Scale(0); err != nil { // drain, then close each replica
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "both replicas drained and closed", func() bool { return len(p.Status().Replicas) == 0 })
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	alive, err := llamaServerPIDs()
	if err != nil {
		t.Fatal(err)
	}
	for pid := range alive {
		if slices.Contains(pids, pid) {
			t.Fatalf("llama-server pid %d still running after Close", pid)
		}
	}
	t.Logf("pids %v gone after drain and Close", pids)
}
