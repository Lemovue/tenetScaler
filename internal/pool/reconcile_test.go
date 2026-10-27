package pool

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newReconcilingPool returns a pool of fake instances whose reconcile loop
// runs every few milliseconds, logging into the returned buffer.
func newReconcilingPool(t *testing.T, maxReplicas int) (*Pool, *fakeBackend, *lockedBuffer) {
	t.Helper()
	b := newFakeBackend()
	logs := &lockedBuffer{}
	p := New(Config{
		Name: "p", MaxReplicas: maxReplicas, Backend: b, ReconcileInterval: 5 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p, b, logs
}

// TestReconcileReplacesCrashedReplica checks that a replica which stopped
// on its own is replaced without any further Scale call (a KEDA/HPA
// baseline holding its target never calls Scale), and that the warning
// names the instance's cause.
func TestReconcileReplacesCrashedReplica(t *testing.T) {
	p, b, logs := newReconcilingPool(t, 3)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	b.inst("p-1").crash(errors.New("exit status 3"))
	waitFor(t, 2*time.Second, "p-1 replaced on its slot", func() bool {
		return slotsOf(p.Status()) == "p-2@1:ready,p-3@0:ready"
	})
	waitFor(t, 2*time.Second, "stopped p-1 closed", func() bool { return b.inst("p-1").closeCount() == 1 })
	out := logs.String()
	if !strings.Contains(out, "replica stopped on its own") || !strings.Contains(out, `cause="exit status 3"`) {
		t.Fatalf("log lacks the replacement warning with its cause:\n%s", out)
	}
	if st := p.Status(); st.Desired != 2 || st.Ready != 2 {
		t.Fatalf("status after replacement %+v", st)
	}
}

// TestReconcileRetriesFailedLaunch checks that failed launches are retried
// by later rounds, after a failed Scale and after a failed replacement,
// and that the reconcile loop logs the failure.
func TestReconcileRetriesFailedLaunch(t *testing.T) {
	p, b, logs := newReconcilingPool(t, 2)
	b.setFailErr(errors.New("port taken"))
	if err := p.Scale(1); err == nil {
		t.Fatal("Scale with a failing backend succeeded")
	}
	waitFor(t, 2*time.Second, "launch retried", func() bool { return p.Status().Ready == 1 })
	if got := b.launched(); got != "p-1@0" {
		t.Fatalf("launches %s, want p-1@0", got)
	}

	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	b.setFailErr(errors.New("no binary")) // the replacement's first launch fails
	b.inst("p-1").crash(errors.New("killed"))
	waitFor(t, 2*time.Second, "p-1 replaced after a failed relaunch", func() bool {
		return slotsOf(p.Status()) == "p-2@1:ready,p-3@0:ready"
	})
	if out := logs.String(); !strings.Contains(out, "launch replica failed") || !strings.Contains(out, "launch replica p-3: no binary") {
		t.Fatalf("log lacks the failed relaunch:\n%s", out)
	}
}

func TestNextReconcileWait(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name          string
		cur, interval time.Duration
		clean         bool
		want          time.Duration
	}{
		{"clean round resets", 8 * s, s, true, s},
		{"repair doubles", s, s, false, 2 * s},
		{"capped at 30 s", 20 * s, s, false, 30 * s},
		{"long interval is its own cap", time.Hour, time.Hour, false, time.Hour},
	}
	for _, tc := range tests {
		if got := nextReconcileWait(tc.cur, tc.interval, tc.clean); got != tc.want {
			t.Errorf("%s: nextReconcileWait = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStatusReadsStatsInParallel checks that Status reads the replicas'
// loads concurrently: each fake Stats waits until all of them have been
// entered, which a sequential Status would never reach.
func TestStatusReadsStatsInParallel(t *testing.T) {
	const n = 4
	b := newFakeBackend()
	all := make(chan struct{})
	var entered atomic.Int32
	var failed atomic.Bool
	b.onStats = func() {
		if entered.Add(1) == n {
			close(all)
		}
		select {
		case <-all:
		case <-time.After(2 * time.Second):
			failed.Store(true)
		}
	}
	p := New(Config{Name: "p", MaxReplicas: n, Backend: b, ReconcileInterval: time.Hour})
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Scale(n); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if failed.Load() {
		t.Fatal("Status read the replicas' stats one at a time")
	}
	if len(st.Replicas) != n || st.Ready != n {
		t.Fatalf("status %+v", st)
	}
	for _, r := range st.Replicas {
		if r.Running != 1 {
			t.Fatalf("replica %+v lost its stats", r)
		}
	}
}
