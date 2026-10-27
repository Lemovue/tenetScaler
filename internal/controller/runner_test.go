package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/model"
)

func TestRunOnceActuatesPublishesAndLogs(t *testing.T) {
	rig := newRig(t, testConfig(t))
	d := rig.r.RunOnce(context.Background())

	if d.FailStatic {
		t.Fatalf("decision failed static: %+v", d.Records)
	}
	if got := rig.act.setCalls(); !reflect.DeepEqual(got, []int{d.PoolReplicas}) {
		t.Errorf("SetReplicas calls = %v, want [%d]", got, d.PoolReplicas)
	}
	// The pool had 3 replicas; tenant A needs 6 of the 8 desired, and one
	// cycle may add at most maxStepPerCycle (4).
	if d.DesiredReplicas != 8 || d.PoolReplicas != 7 {
		t.Errorf("desired %d pool %d, want 8 and 7 (3 + max step 4)", d.DesiredReplicas, d.PoolReplicas)
	}
	pubs := rig.pub.published()
	if d.Policy == nil {
		t.Fatal("decision has no policy")
	}
	// The policy is stamped when it is sent: the clock's second reading.
	want := *d.Policy
	want.IssuedAt = t0.Add(30 * time.Second)
	if len(pubs) != 1 || !reflect.DeepEqual(pubs[0], want) {
		t.Fatalf("published %+v, want the decision's policy stamped at send time %+v", pubs, want)
	}
	if w := pubs[0].Tenants["A"].Weight; w < 4 {
		t.Errorf("tenant A weight %v, want its borrowed grant (>= 4)", w)
	}
	recs := rig.log.List(0, "")
	if len(recs) != len(d.Records) || len(recs) != 4 {
		t.Fatalf("log has %d records, want the decision's %d (3 tenants + pool)", len(recs), len(d.Records))
	}
	if recs[0].Tenant != control.PoolRecordTenant || recs[0].Cycle != 1 {
		t.Errorf("newest record = %+v, want the pool record of cycle 1", recs[0])
	}
	if want := t0.Add(15 * time.Second); !d.Time.Equal(want) {
		t.Errorf("decision time %v, want the runner clock %v", d.Time, want)
	}
	st := rig.r.Status()
	if st.Cycle != 1 || st.FailStatic || st.Decision == nil || st.Pool.DesiredReplicas != 3 || len(st.Errors) != 0 {
		t.Errorf("status = %+v, want cycle 1, the decision, the observed pool and no errors", st)
	}
}

func TestRunOnceFailsStaticOnSourceError(t *testing.T) {
	rig := newRig(t, testConfig(t))
	first := rig.r.RunOnce(context.Background())
	rig.src.set(model.Observation{}, errBoom)
	d := rig.r.RunOnce(context.Background())

	if !d.FailStatic {
		t.Fatal("decision is not fail-static")
	}
	if got := rig.act.setCalls(); len(got) != 1 {
		t.Errorf("SetReplicas calls = %v, want only the first cycle's", got)
	}
	if d.PoolReplicas != first.PoolReplicas {
		t.Errorf("held pool %d, want the previous %d", d.PoolReplicas, first.PoolReplicas)
	}
	// The last policy is republished unchanged.
	pubs := rig.pub.published()
	if len(pubs) != 2 || pubs[1].Version != first.Policy.Version {
		t.Errorf("published versions %v, want the first policy twice", pubs)
	}
	recs := rig.log.List(3, "")
	for _, r := range recs {
		if !r.FailStatic || r.Cycle != 2 || !strings.Contains(r.Reason, "boom") {
			t.Errorf("record %+v: want FailStatic in cycle 2 with the source error in the reason", r)
		}
	}
	st := rig.r.Status()
	e, ok := st.Errors[StageMetrics]
	if !st.FailStatic || !ok || e.Cycle != 2 || !strings.Contains(e.Error, "boom") {
		t.Errorf("status = %+v, want failStatic and the metrics error of cycle 2", st)
	}
	if st.Observation == nil || st.Observation.OK || st.Observation.Error != "boom" {
		t.Errorf("status observation = %+v, want OK=false with the error", st.Observation)
	}
}

func TestRunOnceFailsStaticWithoutPoolStatus(t *testing.T) {
	rig := newRig(t, testConfig(t))
	rig.act.statusErr = errBoom
	d := rig.r.RunOnce(context.Background())
	if !d.FailStatic || len(rig.act.setCalls()) != 0 {
		t.Errorf("failStatic %v, SetReplicas %v: want fail-static and no scaling", d.FailStatic, rig.act.setCalls())
	}
	if _, ok := rig.r.Status().Errors[StagePoolStatus]; !ok {
		t.Error("pool status error missing from status")
	}
}

func TestRunOnceReportsActuationAndPublishErrors(t *testing.T) {
	rig := newRig(t, testConfig(t))
	rig.act.setErr = errors.New("scale refused")
	rig.pub.err = errors.New("gateway down")
	d := rig.r.RunOnce(context.Background())
	if d.FailStatic {
		t.Fatal("actuation/publish errors must not make the cycle fail static")
	}
	st := rig.r.Status()
	if e := st.Errors[StageActuation]; e.Cycle != 1 || !strings.Contains(e.Error, "scale refused") {
		t.Errorf("actuation error = %+v", e)
	}
	if e := st.Errors[StagePublish]; e.Cycle != 1 || !strings.Contains(e.Error, "gateway down") {
		t.Errorf("publish error = %+v", e)
	}
	if rig.log.Len() != len(d.Records) {
		t.Errorf("log has %d records, want %d: errors must not drop the explanation", rig.log.Len(), len(d.Records))
	}
	// Errors are kept after recovery, tagged with the cycle they happened in.
	rig.act.setErr, rig.pub.err = nil, nil
	rig.r.RunOnce(context.Background())
	if st := rig.r.Status(); st.Cycle != 2 || st.Errors[StagePublish].Cycle != 1 {
		t.Errorf("after recovery: cycle %d, publish error %+v; want cycle 2 and the cycle-1 error", st.Cycle, st.Errors[StagePublish])
	}
}

func TestRunOnceHooks(t *testing.T) {
	rig := newRig(t, testConfig(t))
	var order []string
	rig.r.BeforeCycle = func(context.Context) error {
		order = append(order, "before")
		return errors.New("list failed")
	}
	var got control.Decision
	rig.r.AfterDecision = func(_ context.Context, d control.Decision) {
		order = append(order, "after")
		got = d
	}
	d := rig.r.RunOnce(context.Background())
	if !reflect.DeepEqual(order, []string{"before", "after"}) || got.Cycle != d.Cycle {
		t.Errorf("hook order %v, AfterDecision got cycle %d; want before, after and cycle %d", order, got.Cycle, d.Cycle)
	}
	if d.FailStatic || len(rig.act.setCalls()) != 1 {
		t.Error("a BeforeCycle error must not stop the cycle")
	}
	if e := rig.r.Status().Errors[StageTenants]; !strings.Contains(e.Error, "list failed") {
		t.Errorf("tenants error = %+v", e)
	}
}

func TestRunOnceWithoutCoupling(t *testing.T) {
	cfg := testConfig(t)
	cfg.Controller.DisableGatewayCoupling = true
	rig := newRig(t, cfg)
	d := rig.r.RunOnce(context.Background())
	if d.Policy != nil || len(rig.pub.published()) != 0 {
		t.Errorf("policy %+v, published %v: want no policy with coupling off", d.Policy, rig.pub.published())
	}
}

func TestDecisionLogFileErrorIsReported(t *testing.T) {
	rig := newRig(t, testConfig(t))
	log, err := NewDecisionLog(10, t.TempDir()+"/d.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// Closing the file underneath the log makes the next write fail.
	if err := log.file.Close(); err != nil {
		t.Fatal(err)
	}
	rig.r.Log = log
	d := rig.r.RunOnce(context.Background())
	if e := rig.r.Status().Errors[StageDecisionLog]; e.Cycle != 1 {
		t.Errorf("decision log error = %+v, want one in cycle 1", e)
	}
	if log.Len() != len(d.Records) {
		t.Errorf("in-memory log has %d records, want %d", log.Len(), len(d.Records))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	rig := newRig(t, testConfig(t))
	rig.r.Cycle = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.src.onObserve = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	done := make(chan error, 1)
	go func() { done <- rig.r.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if c := rig.r.Status().Cycle; c != 3 {
		t.Errorf("ran %d cycles, want 3", c)
	}
}

func TestRunRejectsMisconfiguration(t *testing.T) {
	rig := newRig(t, testConfig(t))
	tests := []struct {
		name   string
		mutate func(r *Runner)
		want   string
	}{
		{"no core", func(r *Runner) { r.Core = nil }, "Core is nil"},
		{"no source", func(r *Runner) { r.Source = nil }, "Source is nil"},
		{"no actuator", func(r *Runner) { r.Actuator = nil }, "Actuator is nil"},
		{"no cycle", func(r *Runner) { r.Cycle = 0 }, "Cycle must be > 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{Core: rig.r.Core, Source: rig.src, Actuator: rig.act, Cycle: time.Second}
			tt.mutate(r)
			err := r.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Run = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestRunOnceSerializesConcurrentCalls(t *testing.T) {
	rig := newRig(t, testConfig(t))
	var inside, overlap atomic.Int32
	rig.src.onObserve = func(int) {
		if inside.Add(1) > 1 {
			overlap.Add(1)
		}
		time.Sleep(time.Millisecond)
		inside.Add(-1)
	}
	done := make(chan struct{})
	for range 4 {
		go func() {
			rig.r.RunOnce(context.Background())
			done <- struct{}{}
		}()
	}
	for range 4 {
		<-done
	}
	if overlap.Load() != 0 || rig.r.Status().Cycle != 4 {
		t.Errorf("overlapping cycles %d, cycles %d; want 0 and 4", overlap.Load(), rig.r.Status().Cycle)
	}
}

// If the pool status cannot be read before the core's first cycle, the core
// is not stepped: it adopts the running pool's size once it can be read,
// instead of starting from MinReplicas and cutting the pool.
func TestRunOnceWaitsForThePoolBeforeTheFirstStep(t *testing.T) {
	rig := newRig(t, testConfig(t))
	rig.act.statusErr = errBoom
	d := rig.r.RunOnce(context.Background())
	if !d.FailStatic || d.Cycle != 0 || len(d.Records) != 0 || d.Policy != nil || rig.r.Core.State().Cycle != 0 {
		t.Fatalf("decision %+v, core cycle %d: want fail-static without stepping the core", d, rig.r.Core.State().Cycle)
	}
	if e, ok := rig.r.Status().Errors[StagePoolStatus]; !ok || e.Cycle != 0 {
		t.Errorf("pool status error = %+v, %v; want one in cycle 0", e, ok)
	}

	// The pool turns out to run 8 replicas and demand is 8 (A 6, B 1, C 1).
	rig.act.mu.Lock()
	rig.act.statusErr = nil
	rig.act.pool = model.PoolObservation{ReadyReplicas: 8, DesiredReplicas: 8}
	rig.act.mu.Unlock()
	d = rig.r.RunOnce(context.Background())
	if d.FailStatic || d.Cycle != 1 || d.PoolReplicas != 8 {
		t.Errorf("decision cycle %d pool %d failStatic %v, want cycle 1 keeping the 8 replicas", d.Cycle, d.PoolReplicas, d.FailStatic)
	}
	if got := rig.act.setCalls(); !reflect.DeepEqual(got, []int{8}) {
		t.Errorf("SetReplicas calls = %v, want [8]: the running pool must not be cut", got)
	}

	// Once the pool is known, a later status failure is an ordinary
	// fail-static cycle of the core.
	rig.act.mu.Lock()
	rig.act.statusErr = errBoom
	rig.act.mu.Unlock()
	d = rig.r.RunOnce(context.Background())
	if !d.FailStatic || d.Cycle != 2 || d.PoolReplicas != 8 || len(d.Records) != 3 || len(rig.act.setCalls()) != 1 {
		t.Errorf("decision %+v: want the core's fail-static cycle 2 holding 8 replicas, with records and no scaling", d)
	}
}

// Every publish carries the time it is sent; the core's policy is not
// changed, and a fail-static republish gets a fresh stamp too.
func TestRunOnceStampsPolicyWhenSent(t *testing.T) {
	rig := newRig(t, testConfig(t)) // the clock advances 15 s per reading
	d1 := rig.r.RunOnce(context.Background())
	rig.src.set(model.Observation{}, errBoom)
	d2 := rig.r.RunOnce(context.Background())

	pubs := rig.pub.published()
	if len(pubs) != 2 || !d2.FailStatic {
		t.Fatalf("published %d policies, failStatic %v; want 2 and a fail-static second cycle", len(pubs), d2.FailStatic)
	}
	if want := t0.Add(30 * time.Second); !pubs[0].IssuedAt.Equal(want) {
		t.Errorf("first IssuedAt %v, want the send time %v (after the observation at %v)", pubs[0].IssuedAt, want, d1.Time)
	}
	if want := t0.Add(60 * time.Second); !pubs[1].IssuedAt.Equal(want) || pubs[1].Version != pubs[0].Version {
		t.Errorf("republished v%d at %v, want v%d stamped %v", pubs[1].Version, pubs[1].IssuedAt, pubs[0].Version, want)
	}
	if !d1.Policy.IssuedAt.Equal(d1.Time) || !d2.Policy.IssuedAt.Equal(d1.Time) {
		t.Errorf("core policy IssuedAt %v / %v, want the decision time %v unchanged", d1.Policy.IssuedAt, d2.Policy.IssuedAt, d1.Time)
	}
}
