package llama

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"tenantscaler/internal/replica"
)

// TestCleanup checks that Cleanup kills only listed processes that are
// still running llama-server: a dead PID and a live process of another
// program (this test binary) are left alone, and the file is truncated.
func TestCleanup(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("process listing exists on Windows and Linux only")
	}
	l := fakeLauncher(t)
	dead := launch(t, l, "dead-1", 0)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	live := launch(t, l, "live-1", 1)
	f, err := os.OpenFile(l.PIDFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "%d this-test-binary\ngarbage line\n\n%d live-1-again\n", os.Getpid(), live.PID())
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Cleanup(l.PIDFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.Listed != 3 || len(res.Killed) != 1 || res.Killed[0] != live.PID() {
		t.Fatalf("Cleanup = %+v, want 3 listed and only %d killed", res, live.PID())
	}
	assertGone(t, live.PID())
	waitFor(t, 10*time.Second, "killed replica reaped", func() bool { return live.State() == replica.StateStopped })
	if data, err := os.ReadFile(l.PIDFile); err != nil || len(data) != 0 {
		t.Fatalf("pid file after Cleanup: %q, %v; want empty", data, err)
	}

	if res, err := Cleanup(l.PIDFile); err != nil || res.Listed != 0 || len(res.Killed) != 0 {
		t.Fatalf("second Cleanup = %+v, %v", res, err)
	}
	if res, err := Cleanup(filepath.Join(t.TempDir(), "missing.pids")); err != nil || res.Listed != 0 {
		t.Fatalf("Cleanup of a missing file = %+v, %v", res, err)
	}
}

// TestCleanupSkipsReusedPID checks that a live llama-server whose PID is
// listed with a launch time far from its creation time (the PID was reused
// by a llama-server that is not ours) is left running.
func TestCleanupSkipsReusedPID(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("process listing exists on Windows and Linux only")
	}
	l := fakeLauncher(t)
	other := launch(t, l, "other-1", 0)
	line := fmt.Sprintf("%d old-run %s\n", other.PID(), time.Now().Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(l.PIDFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Cleanup(l.PIDFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.Listed != 1 || len(res.Killed) != 0 || len(res.Skipped) != 1 || res.Skipped[0] != other.PID() {
		t.Fatalf("Cleanup = %+v, want pid %d skipped", res, other.PID())
	}
	alive, err := llamaServerPIDs()
	if err != nil || !alive[other.PID()] {
		t.Fatalf("skipped llama-server %d is gone: %v", other.PID(), err)
	}
	if data, err := os.ReadFile(l.PIDFile); err != nil || len(data) != 0 {
		t.Fatalf("pid file after Cleanup: %q, %v; want empty", data, err)
	}
}

func TestSameLaunch(t *testing.T) {
	launched := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		want    bool
	}{
		{"created just before the record", launched.Add(-20 * time.Millisecond), true},
		{"record truncated to the second", launched.Add(900 * time.Millisecond), true},
		{"PID reused an hour later", launched.Add(time.Hour), false},
		{"PID reused seconds later", launched.Add(launchTimeSlack), false},
		{"older process", launched.Add(-time.Minute), false},
	}
	for _, tc := range tests {
		if got := sameLaunch(tc.created, launched); got != tc.want {
			t.Errorf("%s: sameLaunch = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProcessStartTime checks the creation time read from the operating
// system against the moments around a launch.
func TestProcessStartTime(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("process inspection exists on Windows and Linux only")
	}
	l := fakeLauncher(t)
	before := time.Now()
	p := launch(t, l, "born-1", 0)
	after := time.Now()
	created, err := processStartTime(p.PID())
	if err != nil {
		t.Fatal(err)
	}
	// One second of slack: Linux reports start times in whole seconds.
	if created.Before(before.Add(-time.Second)) || created.After(after.Add(time.Second)) {
		t.Fatalf("created %v, want within [%v, %v]", created, before, after)
	}
	entries, err := readPIDEntries(l.PIDFile)
	if err != nil || len(entries) != 1 || !sameLaunch(created, entries[0].launched) {
		t.Fatalf("pid file entries %+v, %v: want one matching created %v", entries, err, created)
	}
}
