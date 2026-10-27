package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"tenantscaler/internal/config"
	"tenantscaler/internal/control"
	"tenantscaler/internal/k8s"
	"tenantscaler/internal/model"
)

const kubeNS = "tenantscaler"

// tenantCR builds a TenantScaler resource as the API server would return it.
func tenantCR(name, pool string, guaranteed, generation int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k8s.Group + "/" + k8s.Version,
		"kind":       k8s.Kind,
		"metadata":   map[string]any{"name": name, "namespace": kubeNS, "generation": generation},
		"spec": map[string]any{
			"pool": pool, "guaranteedReplicas": guaranteed, "maxReplicas": int64(8), "priority": int64(100),
			"slo": map[string]any{"ttftSeconds": int64(2), "tpotMillis": int64(100), "targetFraction": 0.99},
		},
	}}
}

func fakeKube(objs ...*unstructured.Unstructured) (*k8s.Client, *dynamicfake.FakeDynamicClient) {
	ro := make([]runtime.Object, len(objs))
	for i, o := range objs {
		ro[i] = o
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{k8s.GVR: k8s.Kind + "List"}, ro...)
	return &k8s.Client{Dynamic: dyn, Namespace: kubeNS}, dyn
}

func readStatus(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) k8s.TenantScalerStatus {
	t.Helper()
	u, err := dyn.Resource(k8s.GVR).Namespace(kubeNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	ts, err := k8s.FromUnstructured(u)
	if err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return ts.Status
}

func TestKubeTenantsLoad(t *testing.T) {
	c, _ := fakeKube(tenantCR("b", "shared", 3, 1), tenantCR("a", "shared", 3, 2), tenantCR("x", "other", 9, 1))
	kt := &KubeTenants{Client: c, Pool: "shared", Capacity: 10}
	specs, err := kt.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(specs) != 2 || specs[0].Name != "a" || specs[1].Name != "b" || specs[0].GuaranteedReplicas != 3 {
		t.Errorf("Load = %+v, want a and b of pool shared", specs)
	}

	tests := []struct {
		name string
		kt   *KubeTenants
		want string
	}{
		{"no tenants of the pool", &KubeTenants{Client: c, Pool: "empty", Capacity: 10}, "no TenantScaler resources"},
		{"guarantees above capacity", &KubeTenants{Client: c, Pool: "shared", Capacity: 5}, "more than pool capacity"},
		{"no client", &KubeTenants{Pool: "shared", Capacity: 10}, "no client"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.kt.Load(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestKubeTenantsWait(t *testing.T) {
	c, _ := fakeKube(tenantCR("a", "shared", 1, 1))
	kt := &KubeTenants{Client: c, Pool: "shared", Capacity: 4, Logger: discardLogger()}
	if specs, err := kt.Wait(context.Background(), time.Hour); err != nil || len(specs) != 1 {
		t.Errorf("Wait with a tenant present = %+v, %v", specs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	empty := &KubeTenants{Client: c, Pool: "missing", Capacity: 4, Logger: discardLogger()}
	if _, err := empty.Wait(ctx, time.Millisecond); err == nil {
		t.Error("Wait without tenants returned no error after the context ended")
	}
}

// kubeRig runs a Runner in Kubernetes mode against fakes: tenants a and b
// (guarantees 3) on a 10-replica pool. mutate, when set, changes the config.
func kubeRig(t *testing.T, mutate func(*config.Config)) (*testRig, *KubeTenants, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	c, dyn := fakeKube(tenantCR("a", "shared", 3, 5), tenantCR("b", "shared", 3, 1))
	kt := &KubeTenants{Client: c, Pool: "shared", Capacity: 10, Logger: discardLogger()}
	specs, err := kt.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Tenants = specs
	if mutate != nil {
		mutate(&cfg)
	}
	rig := newRig(t, cfg)
	kt.Core = rig.r.Core
	rig.r.BeforeCycle = kt.BeforeCycle
	rig.r.AfterDecision = kt.AfterDecision
	rig.src.set(model.Observation{Tenants: []model.TenantObservation{
		{Tenant: "a", ArrivalWeightedTokensPerSec: 2000, HasSLOData: true, P99TTFTSeconds: 1, Completed: 10, SLOAttainment: 1},
		{Tenant: "b", ArrivalWeightedTokensPerSec: 100, QueueLength: 1},
	}}, nil)
	return rig, kt, dyn
}

func TestKubeTenantsWritesStatus(t *testing.T) {
	rig, _, dyn := kubeRig(t, nil)
	d := rig.r.RunOnce(context.Background())
	var recA model.DecisionRecord
	for _, r := range d.Records {
		if r.Tenant == "a" {
			recA = r
		}
	}
	st := readStatus(t, dyn, "a")
	if st.Grant != recA.Final.Grant || st.Demand != 6 || st.SLOSlack != 0.5 || st.Shedding ||
		st.PolicyVersion != 1 || st.ObservedGeneration != 5 || st.Reason != recA.Reason ||
		!st.LastUpdate.Time.Equal(d.Time) {
		t.Errorf("status of a = %+v, want grant %d, demand 6, slack 0.5, policy v1, generation 5, time %v",
			st, recA.Final.Grant, d.Time)
	}
	if _, err := dyn.Resource(k8s.GVR).Namespace(kubeNS).Get(context.Background(), control.PoolRecordTenant, metav1.GetOptions{}); err == nil {
		t.Error("the pool record was written as a tenant")
	}

	// Fail-static keeps the last known demand and slack.
	rig.src.set(model.Observation{}, errBoom)
	rig.r.RunOnce(context.Background())
	st2 := readStatus(t, dyn, "a")
	if st2.Demand != 6 || st2.SLOSlack != 0.5 || st2.Grant != st.Grant || !strings.Contains(st2.Reason, "Metrics unavailable") {
		t.Errorf("fail-static status of a = %+v, want the held demand, slack and grant with the fail-static reason", st2)
	}
}

// With gateway coupling off there is no last policy to hold, so a
// fail-static cycle must keep the last written grant and shedding itself.
func TestKubeTenantsFailStaticHoldsStatusWithoutCoupling(t *testing.T) {
	rig, _, dyn := kubeRig(t, func(c *config.Config) { c.Controller.DisableGatewayCoupling = true })
	rig.r.RunOnce(context.Background())
	before := readStatus(t, dyn, "a")
	if before.Grant != 6 || before.Demand != 6 || before.PolicyVersion != 0 {
		t.Fatalf("status of a = %+v, want grant and demand 6 without a policy", before)
	}

	rig.src.set(model.Observation{}, errBoom)
	d := rig.r.RunOnce(context.Background())
	after := readStatus(t, dyn, "a")
	if after.Grant != before.Grant || after.Shedding != before.Shedding || after.Demand != before.Demand ||
		after.SLOSlack != before.SLOSlack || !strings.Contains(after.Reason, "Metrics unavailable") ||
		!after.LastUpdate.Time.Equal(d.Time) {
		t.Errorf("fail-static status of a = %+v, want %+v held with the fail-static reason at %v", after, before, d.Time)
	}
}

func TestKubeTenantsPolicyVersion(t *testing.T) {
	rig, kt, _ := kubeRig(t, nil)
	if v, err := kt.PolicyVersion(context.Background()); err != nil || v != 0 {
		t.Errorf("PolicyVersion before any status = %d, %v; want 0", v, err)
	}
	rig.r.RunOnce(context.Background())
	rig.r.RunOnce(context.Background())
	if v, err := kt.PolicyVersion(context.Background()); err != nil || v != 2 {
		t.Errorf("PolicyVersion = %d, %v; want the v2 written by cycle 2", v, err)
	}
	if _, err := (&KubeTenants{}).PolicyVersion(context.Background()); err == nil {
		t.Error("PolicyVersion without a client succeeded")
	}
}

func TestKubeTenantsReloadsTenants(t *testing.T) {
	rig, _, dyn := kubeRig(t, nil)
	rig.r.RunOnce(context.Background())

	// A third tenant appears.
	if _, err := dyn.Resource(k8s.GVR).Namespace(kubeNS).Create(context.Background(), tenantCR("c", "shared", 2, 1), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	d := rig.r.RunOnce(context.Background())
	if !hasTenant(d, "c") {
		t.Errorf("cycle 2 records %v do not include the new tenant c", tenantsOf(d))
	}

	// An invalid change (guarantees above capacity) keeps the previous set.
	if _, err := dyn.Resource(k8s.GVR).Namespace(kubeNS).Create(context.Background(), tenantCR("d", "shared", 9, 1), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	d = rig.r.RunOnce(context.Background())
	if hasTenant(d, "d") || !hasTenant(d, "c") || d.FailStatic {
		t.Errorf("cycle 3 tenants %v, want a, b, c kept and no fail-static", tenantsOf(d))
	}
	if e := rig.r.Status().Errors[StageTenants]; e.Cycle != 3 || !strings.Contains(e.Error, "keeping the previous set") {
		t.Errorf("tenants error = %+v", e)
	}
}

func TestKubeTenantsWithoutCoreOrClient(t *testing.T) {
	kt := &KubeTenants{Logger: discardLogger()}
	if err := kt.BeforeCycle(context.Background()); err == nil {
		t.Error("BeforeCycle without a core succeeded")
	}
	// Must not panic.
	kt.AfterDecision(context.Background(), control.Decision{Records: []model.DecisionRecord{{Tenant: "a"}}})
}

func hasTenant(d control.Decision, name string) bool {
	for _, r := range d.Records {
		if r.Tenant == name {
			return true
		}
	}
	return false
}

func tenantsOf(d control.Decision) []string {
	var out []string
	for _, r := range d.Records {
		out = append(out, r.Tenant)
	}
	return out
}
