package scalerun

import (
	"context"
	"fmt"
	"math"
	"time"

	"tenantscaler/internal/pool"
	"tenantscaler/internal/replica"
)

// DefaultSampleEvery is how often Run samples slot occupancy for the HPA
// when Runner.SampleEvery is zero; it matches the simulator's 1 s samples.
const DefaultSampleEvery = time.Second

// occupancyWindow accumulates the slot-occupancy samples taken since the
// last HPA sync.
type occupancyWindow struct {
	sum float64
	n   int
}

// occupancy is the slot occupancy of one pool status, the CPU-utilisation
// stand-in the HPA reads: the busy slots of ready replicas over the slots
// of every replica the pool should have, max(Desired, Ready+Starting).
// Replicas that are not ready yet (starting, or not relaunched yet) count
// as idle, which is how the Kubernetes replica calculator treats pods that
// are not ready when it scales up (0% usage); so the HPA does not compound
// a scale-up from the same busy replicas while the replicas it asked for
// are still starting. Draining replicas are left out, like terminating
// pods. ok is false without a ready replica: there is nothing to measure,
// as Kubernetes gets no metrics then.
func occupancy(st pool.Status, slots int) (occ float64, ok bool) {
	ready, starting, busy := 0, 0, 0
	for _, rs := range st.Replicas {
		switch rs.State {
		case replica.StateReady:
			ready++
			busy += rs.Running
		case replica.StateStarting:
			starting++
		}
	}
	if ready == 0 || slots < 1 {
		return 0, false
	}
	n := max(st.Desired, ready+starting)
	return float64(busy) / float64(n*slots), true
}

// Sample reads the pool status and adds its slot occupancy to the samples
// the next HPA sync averages. Run calls it every SampleEvery; call it
// yourself when driving RunOnce from your own loop. It does nothing for
// KEDA, which reads the queue length at sync time, as KEDA polls its
// scaler.
func (r *Runner) Sample(ctx context.Context) error {
	if r.Algo != AlgoHPA {
		return nil
	}
	if err := r.Validate(); err != nil {
		return err
	}
	st, err := r.Pool.Status(ctx)
	if err != nil {
		return fmt.Errorf("pool status: %w", err)
	}
	r.addSample(st)
	return nil
}

// addSample adds the occupancy of st to the window (nothing without a
// ready replica).
func (r *Runner) addSample(st pool.Status) {
	occ, ok := occupancy(st, r.MaxNumSeqs)
	if !ok {
		return
	}
	r.winMu.Lock()
	defer r.winMu.Unlock()
	r.win.sum += occ
	r.win.n++
}

// takeWindow returns the mean occupancy of the samples since the last
// sync (NaN without any) and their number, and starts a new window.
func (r *Runner) takeWindow() (float64, int) {
	r.winMu.Lock()
	defer r.winMu.Unlock()
	w := r.win
	r.win = occupancyWindow{}
	if w.n == 0 {
		return math.NaN(), 0
	}
	return w.sum / float64(w.n), w.n
}

// hpaMetric adds the sync's own status as a sample and returns the mean
// occupancy since the last sync with its sample count and a description;
// NaN when no sample saw a ready replica.
func (r *Runner) hpaMetric(st pool.Status) (float64, int, string) {
	r.addSample(st)
	u, n := r.takeWindow()
	if n == 0 {
		return math.NaN(), 0, "no ready replica to measure slot occupancy"
	}
	return u, n, fmt.Sprintf("slot occupancy %.2f (mean of %d samples; replicas not ready count as idle)", u, n)
}

// sampleEvery returns the effective sampling period of Run.
func (r *Runner) sampleEvery() (time.Duration, error) {
	switch {
	case r.SampleEvery < 0:
		return 0, fmt.Errorf("scalerun: sample period must be >= 0, got %v", r.SampleEvery)
	case r.SampleEvery == 0:
		return DefaultSampleEvery, nil
	}
	return r.SampleEvery, nil
}
