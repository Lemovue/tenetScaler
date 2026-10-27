package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// versionFunc adapts a function to VersionSource.
type versionFunc func(context.Context) (int64, error)

func (f versionFunc) PolicyVersion(ctx context.Context) (int64, error) { return f(ctx) }

// A restarted controller starts again at policy v1 while the gateway keeps
// v50 and ignores older versions. The ignored publish must show in /status
// and the next cycle must issue a version the gateway applies.
func TestRunnerRecoversFromStaleGatewayVersion(t *testing.T) {
	gw, srv := newPolicyGateway(t, 50)
	rig := newRig(t, testConfig(t))
	rig.r.Publisher = HTTPPublisher{URLs: []string{srv.URL}}

	d1 := rig.r.RunOnce(context.Background())
	if d1.Policy == nil || d1.Policy.Version != 1 {
		t.Fatalf("first policy %+v, want v1", d1.Policy)
	}
	e, ok := rig.r.Status().Errors[StagePublish]
	if !ok || e.Cycle != 1 || !strings.Contains(e.Error, "keeps the newer v50") {
		t.Errorf("publish error = %+v, %v; want the ignored v1 reported in cycle 1", e, ok)
	}

	d2 := rig.r.RunOnce(context.Background())
	if d2.Policy == nil || d2.Policy.Version != 51 {
		t.Fatalf("second policy %+v, want v51", d2.Policy)
	}
	for _, rec := range d2.Records {
		if rec.PolicyVersion != 51 {
			t.Errorf("record %s has policy v%d, want v51", rec.Tenant, rec.PolicyVersion)
		}
	}
	if got := gw.appliedVersions(); !reflect.DeepEqual(got, []int64{51}) {
		t.Errorf("gateway applied %v, want [51]", got)
	}
	if e := rig.r.Status().Errors[StagePublish]; e.Cycle != 1 {
		t.Errorf("publish error = %+v, want only the one of cycle 1", e)
	}
}

// Seeding before the first cycle continues the newest version in use, so
// even the first policy is applied.
func TestSeedPolicyVersion(t *testing.T) {
	g50, srv50 := newPolicyGateway(t, 50)
	g7, srv7 := newPolicyGateway(t, 7)
	pub := HTTPPublisher{URLs: []string{srv7.URL, srv50.URL}}
	rig := newRig(t, testConfig(t))
	rig.r.Publisher = pub

	failing := versionFunc(func(context.Context) (int64, error) { return 0, errBoom })
	statuses := versionFunc(func(context.Context) (int64, error) { return 42, nil })
	err := rig.r.SeedPolicyVersion(context.Background(), pub, failing, statuses)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("SeedPolicyVersion = %v, want the failing source's error", err)
	}
	if v := rig.r.Core.State().PolicyVersion; v != 50 {
		t.Fatalf("core policy version %d, want the newest in use, 50", v)
	}

	d := rig.r.RunOnce(context.Background())
	if d.Policy == nil || d.Policy.Version != 51 || len(rig.r.Status().Errors) != 0 {
		t.Fatalf("first policy %+v, errors %+v; want v51 published without errors", d.Policy, rig.r.Status().Errors)
	}
	if !reflect.DeepEqual(g50.appliedVersions(), []int64{51}) || !reflect.DeepEqual(g7.appliedVersions(), []int64{51}) {
		t.Errorf("gateways applied %v and %v, want v51 on both", g50.appliedVersions(), g7.appliedVersions())
	}
	// The core's first cycle still adopted the running pool (3 replicas).
	if d.Cycle != 1 || d.PoolReplicas != 7 {
		t.Errorf("cycle %d pool %d, want cycle 1 scaling 3 -> 7", d.Cycle, d.PoolReplicas)
	}

	// An older version never lowers the core's.
	if err := rig.r.SeedPolicyVersion(context.Background(), versionFunc(func(context.Context) (int64, error) { return 3, nil })); err != nil {
		t.Fatal(err)
	}
	if v := rig.r.Core.State().PolicyVersion; v != 51 {
		t.Errorf("core policy version %d after an older seed, want 51", v)
	}
	if err := (&Runner{}).SeedPolicyVersion(context.Background()); err == nil {
		t.Error("SeedPolicyVersion without a core succeeded")
	}
}
