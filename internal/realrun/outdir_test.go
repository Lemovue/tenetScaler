package realrun

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRunRefusesEarlierResults checks that Run never mixes its outputs
// with an earlier run's: it refuses such a directory unless Overwrite is
// set, and then removes only Run's own outputs.
func TestRunRefusesEarlierResults(t *testing.T) {
	plan, guards := tinyPlan(t, []string{"S7-ts-full"}, 1)
	stop := errors.New("guard stops the run")
	plan.Guard = func(context.Context) error { guards.Add(1); return stop }
	stale := filepath.Join(plan.OutDir, recordsDir, "S9-old-run7.csv")
	profile := filepath.Join(plan.OutDir, "profile.md")
	for _, f := range []string{stale, profile, filepath.Join(plan.OutDir, summaryMDFile), filepath.Join(plan.OutDir, ttftPlotFile)} {
		writeTestFile(t, f)
	}
	_, err := Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "earlier run") || !strings.Contains(err.Error(), "-overwrite") || guards.Load() != 0 {
		t.Fatalf("err %v after %d guard checks, want a refusal before any run", err, guards.Load())
	}
	if !exists(stale) || !exists(profile) {
		t.Fatal("a refused run removed files")
	}
	plan.Overwrite = true
	if _, err := Run(context.Background(), plan); !errors.Is(err, stop) {
		t.Fatalf("err %v, want the guard's", err)
	}
	if exists(stale) || exists(filepath.Join(plan.OutDir, summaryMDFile)) || exists(filepath.Join(plan.OutDir, ttftPlotFile)) {
		t.Error("Overwrite left an earlier run's results")
	}
	if !exists(profile) {
		t.Error("Overwrite removed profile.md, which is not Run's")
	}

	// An empty output subdirectory holds no results.
	fresh, _ := tinyPlan(t, []string{"S7-ts-full"}, 1)
	fresh.Guard = plan.Guard
	if err := os.MkdirAll(filepath.Join(fresh.OutDir, timelineDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), fresh); !errors.Is(err, stop) {
		t.Errorf("err %v with an empty timeline directory, want the guard's", err)
	}
}

// TestDefaultGuardLogsToPlanLogger runs the default guard against a guard
// script that always fails and checks that its retry warning reaches the
// plan's logger.
func TestDefaultGuardLogsToPlanLogger(t *testing.T) {
	if _, err := exec.LookPath("python"); err != nil {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python is not installed")
		}
	}
	plan, _ := tinyPlan(t, []string{"S7-ts-full"}, 1)
	dir := t.TempDir()
	script := filepath.Join(dir, filepath.FromSlash(GuardScript))
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("import sys\nsys.exit(3)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// The first warning cancels the guard's wait to re-check.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logged := &cancelOnWrite{cancel: cancel}
	plan.Guard, plan.Logger = nil, slog.New(slog.NewTextHandler(logged, nil))
	p, err := plan.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Guard(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("guard err %v, want the cancellation by the logged warning", err)
	}
	if !strings.Contains(logged.buf.String(), "resource guard failed") {
		t.Errorf("plan logger got %q, want the guard's warning", logged.buf.String())
	}
}

// cancelOnWrite records log output and cancels a context on the first
// write. The guard logs from the calling goroutine, so no locking is
// needed.
type cancelOnWrite struct {
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(b []byte) (int, error) {
	defer w.cancel()
	return w.buf.Write(b)
}
