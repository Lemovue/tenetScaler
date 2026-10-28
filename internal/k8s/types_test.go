package k8s

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"tenantscaler/internal/model"
)

const testNamespace = "tenantscaler"

// tenantObject builds a TenantScaler as the dynamic client returns it.
func tenantObject(name, pool string, guaranteed int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata": map[string]interface{}{
			"name":       name,
			"namespace":  testNamespace,
			"generation": int64(4),
		},
		"spec": map[string]interface{}{
			"pool":               pool,
			"guaranteedReplicas": guaranteed,
			"maxReplicas":        int64(8),
			"priority":           int64(100),
			"slo": map[string]interface{}{
				"ttftSeconds":    int64(2),
				"tpotMillis":     float64(100),
				"targetFraction": 0.99,
			},
		},
	}}
}

func TestFromUnstructured(t *testing.T) {
	got, err := FromUnstructured(tenantObject("a", "shared-llm-pool", 3))
	if err != nil {
		t.Fatalf("FromUnstructured: %v", err)
	}
	want := TenantScalerSpec{
		Pool: "shared-llm-pool", GuaranteedReplicas: 3, MaxReplicas: 8, Priority: 100,
		SLO: model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99},
	}
	if got.Spec != want {
		t.Errorf("spec = %+v, want %+v", got.Spec, want)
	}
	if got.Name != "a" || got.Namespace != testNamespace || got.Generation != 4 {
		t.Errorf("metadata = %s/%s gen %d, want %s/a gen 4", got.Namespace, got.Name, got.Generation, testNamespace)
	}
	if got.Kind != Kind || got.APIVersion != Group+"/"+Version {
		t.Errorf("type meta = %s %s", got.APIVersion, got.Kind)
	}
}

func TestFromUnstructuredErrors(t *testing.T) {
	wrongKind := tenantObject("a", "p", 1)
	wrongKind.SetKind("Deployment")
	wrongGroup := tenantObject("a", "p", 1)
	wrongGroup.SetAPIVersion("apps/v1")
	badField := tenantObject("bad", "p", 1)
	if err := unstructured.SetNestedField(badField.Object, "three", "spec", "guaranteedReplicas"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		in      *unstructured.Unstructured
		wantErr string
	}{
		{"nil", nil, "nil"},
		{"wrong kind", wrongKind, "kind \"Deployment\""},
		{"wrong group", wrongGroup, "apiVersion \"apps/v1\""},
		{"bad field type", badField, "\"bad\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromUnstructured(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %s", err, tt.wantErr)
			}
		})
	}
}

func TestRoundTripThroughUnstructured(t *testing.T) {
	in := TenantScaler{
		TypeMeta:   metav1.TypeMeta{APIVersion: Group + "/" + Version, Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
		Spec: TenantScalerSpec{
			Pool: "p", GuaranteedReplicas: 2, MaxReplicas: 5, Priority: -1,
			SLO: model.SLO{TTFTSeconds: 1.5, TPOTMillis: 80, TargetFraction: 0.95},
		},
		Status: TenantScalerStatus{
			Grant: 4, Demand: 6, Shedding: true, SLOSlack: -0.25, Reason: "borrowed 2",
			PolicyVersion: 9, ObservedGeneration: 3,
			LastUpdate: metav1.NewTime(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		},
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := FromUnstructured(&unstructured.Unstructured{Object: m})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spec != in.Spec {
		t.Errorf("spec = %+v, want %+v", out.Spec, in.Spec)
	}
	if !out.Status.LastUpdate.Equal(&in.Status.LastUpdate) {
		t.Errorf("lastUpdate = %v, want %v", out.Status.LastUpdate, in.Status.LastUpdate)
	}
	out.Status.LastUpdate = in.Status.LastUpdate
	if out.Status != in.Status {
		t.Errorf("status = %+v, want %+v", out.Status, in.Status)
	}
}

func TestZeroStatusIsOmitted(t *testing.T) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&TenantScalerStatus{Grant: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["lastUpdate"]; ok {
		t.Errorf("zero lastUpdate must be omitted, got %v", m["lastUpdate"])
	}
}

func TestTenantSpec(t *testing.T) {
	ts, err := FromUnstructured(tenantObject("c", "p", 3))
	if err != nil {
		t.Fatal(err)
	}
	want := model.TenantSpec{
		Name: "c", GuaranteedReplicas: 3, MaxReplicas: 8, Priority: 100,
		SLO: model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99},
	}
	if got := ts.TenantSpec(); got != want {
		t.Errorf("TenantSpec() = %+v, want %+v", got, want)
	}
}
