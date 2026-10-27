package k8s

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"tenantscaler/internal/engine"
)

// sideBySideSet is the apply set of the side-by-side baseline, which runs
// BESIDE TenantScaler on its own pool (TestSideBySideBaseline); every other
// autoscaler replaces TenantScaler on model-pool.
const sideBySideSet = "k3s/side-by-side"

// replacingAutoscalers are the baseline autoscalers that run instead of
// the TenantScaler controller.
func replacingAutoscalers(ms []manifest) []manifest {
	var out []manifest
	for _, m := range append(byKind(ms, "ScaledObject"), byKind(ms, "HorizontalPodAutoscaler")...) {
		if applySet(m.file) != sideBySideSet {
			out = append(out, m)
		}
	}
	return out
}

func TestBaselinesTargetModelPool(t *testing.T) {
	ms := loadManifests(t)
	for _, m := range replacingAutoscalers(ms) {
		if !strings.HasPrefix(m.file, "k8s/baselines/") {
			t.Errorf("%v: autoscalers must live in k8s/baselines/ so `kubectl apply -f deploy/k8s/` skips them", m)
		}
		if name, _, _ := unstructured.NestedString(m.obj.Object, "spec", "scaleTargetRef", "name"); name != "model-pool" {
			t.Errorf("%v: scales %q, want model-pool", m, name)
		}
	}
	keda := find(t, ms, "ScaledObject", "model-pool-queue")
	if keda.GetAPIVersion() != "keda.sh/v1alpha1" {
		t.Errorf("ScaledObject apiVersion = %s", keda.GetAPIVersion())
	}
	triggers, _, _ := unstructured.NestedSlice(keda.Object, "spec", "triggers")
	if len(triggers) != 1 {
		t.Fatalf("ScaledObject has %d triggers, want 1", len(triggers))
	}
	trigger := triggers[0].(map[string]interface{})
	query, _, _ := unstructured.NestedString(trigger, "metadata", "query")
	threshold, _, _ := unstructured.NestedString(trigger, "metadata", "threshold")
	if trigger["type"] != "prometheus" || trigger["metricType"] != "AverageValue" ||
		!strings.Contains(query, "vllm:num_requests_waiting") || threshold != "4" {
		t.Errorf("KEDA trigger = %v, want a prometheus query on vllm:num_requests_waiting, 4 per replica (AverageValue)", trigger)
	}

	hpa := find(t, ms, "HorizontalPodAutoscaler", "model-pool-cpu")
	if hpa.GetAPIVersion() != "autoscaling/v2" {
		t.Errorf("HPA apiVersion = %s", hpa.GetAPIVersion())
	}
	metrics, _, _ := unstructured.NestedSlice(hpa.Object, "spec", "metrics")
	if len(metrics) != 1 {
		t.Fatalf("HPA has %d metrics, want 1", len(metrics))
	}
	resource, _, _ := unstructured.NestedString(metrics[0].(map[string]interface{}), "resource", "name")
	target, _, _ := unstructured.NestedInt64(metrics[0].(map[string]interface{}), "resource", "target", "averageUtilization")
	if resource != "cpu" || target != 70 {
		t.Errorf("HPA metric = %s at %d%%, want cpu at 70%%", resource, target)
	}

	// Same bounds and scale-down window as the simulated baselines
	// (autoscale.DefaultKEDAConfig / DefaultHPAConfig): 1..capacity, 300 s.
	capacity, err := strconv.ParseInt(flagValues(containerArgs(t, find(t, deployed(ms), "Deployment", "tenantscaler-controller")))["-capacity"], 10, 64)
	if err != nil {
		t.Fatalf("controller -capacity: %v", err)
	}
	for _, b := range []struct {
		obj                 *unstructured.Unstructured
		minPath, maxPath    []string
		scaleDownWindowPath []string
	}{
		{keda, []string{"spec", "minReplicaCount"}, []string{"spec", "maxReplicaCount"},
			[]string{"spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleDown", "stabilizationWindowSeconds"}},
		{hpa, []string{"spec", "minReplicas"}, []string{"spec", "maxReplicas"},
			[]string{"spec", "behavior", "scaleDown", "stabilizationWindowSeconds"}},
	} {
		lo, _, _ := unstructured.NestedInt64(b.obj.Object, b.minPath...)
		hi, _, _ := unstructured.NestedInt64(b.obj.Object, b.maxPath...)
		window, _, _ := unstructured.NestedInt64(b.obj.Object, b.scaleDownWindowPath...)
		if lo != 1 || hi != capacity || window != 300 {
			t.Errorf("%s %s: replicas %d..%d, scale-down window %d s; want 1..%d (the pool capacity) and 300 s",
				b.obj.GetKind(), b.obj.GetName(), lo, hi, window, capacity)
		}
	}
}

func TestDockerfile(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	for _, want := range []string{
		"FROM golang:1.27 AS build",
		"CGO_ENABLED=0",
		"./cmd/tenantscaler",
		"FROM gcr.io/distroless/static:nonroot",
		`ENTRYPOINT ["/tenantscaler"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	dockerfileUID(t)
	ignore, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Fields(string(ignore)), ".env") {
		t.Error(".dockerignore must exclude .env (local secrets)")
	}
}

func TestPassthroughGatewayOverlay(t *testing.T) {
	ms := loadManifests(t)
	fair := find(t, deployed(ms), "Deployment", "gateway")
	over := findManifest(t, inSet(ms, "k8s/baselines"), "Deployment", "gateway")
	if mode := flagValues(containerArgs(t, fair))["-mode"]; mode != "fair" {
		t.Errorf("deployed gateway -mode = %q, want fair", mode)
	}
	if mode := flagValues(containerArgs(t, over.obj))["-mode"]; mode != "passthrough" {
		t.Errorf("%v: -mode = %q, want passthrough (requests must queue at the replicas, as in the simulated S1/S2)", over, mode)
	}
	// Apart from -mode the overlay is the deployed gateway, so switching to
	// a baseline changes nothing else (tenants, discovery, security).
	got := over.obj.DeepCopy()
	list, _, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "template", "spec", "containers")
	args := list.([]interface{})[0].(map[string]interface{})["args"].([]interface{})
	if i := slices.Index(args, interface{}("-mode")); i >= 0 && i+1 < len(args) {
		args[i+1] = "fair"
	}
	if !reflect.DeepEqual(got.Object, fair.Object) {
		t.Errorf("%v differs from the deployed gateway in more than -mode", over)
	}

	// Each autoscaler's runbook stops the controller and switches the
	// gateway to passthrough before it applies the autoscaler.
	autoscalers := replacingAutoscalers(ms)
	if len(autoscalers) == 0 {
		t.Fatal("no baseline autoscalers under deploy/")
	}
	for _, m := range autoscalers {
		data, err := os.ReadFile(filepath.Join(deployDir, filepath.FromSlash(m.file)))
		if err != nil {
			t.Fatal(err)
		}
		text, last := string(data), -1
		for _, step := range []string{
			"kubectl -n tenantscaler scale deployment tenantscaler-controller --replicas=0",
			"kubectl apply -f deploy/" + over.file,
			"kubectl apply -f deploy/" + m.file,
		} {
			i := strings.Index(text, step)
			if i <= last {
				t.Errorf("%s: runbook lacks %q after the previous step", m.file, step)
			}
			last = max(last, i)
		}
	}
}

// The side-by-side baseline runs BESIDE TenantScaler on its own pool: real
// KEDA scales model-pool-baseline (never model-pool) from the passthrough
// baseline gateway's /stats, with the queue-based baseline's settings.
func TestSideBySideBaseline(t *testing.T) {
	ms := inSet(loadManifests(t), sideBySideSet)
	find(t, ms, "Deployment", "model-pool-baseline")
	gw := flagValues(containerArgs(t, find(t, ms, "Deployment", "gateway-baseline")))
	if gw["-mode"] != "passthrough" || gw["-k8s-service"] != "model-pool-baseline" {
		t.Errorf("gateway-baseline -mode %q -k8s-service %q, want passthrough in front of model-pool-baseline", gw["-mode"], gw["-k8s-service"])
	}
	so := find(t, ms, "ScaledObject", "model-pool-baseline")
	if name, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "name"); name != "model-pool-baseline" {
		t.Errorf("side-by-side ScaledObject scales %q, want model-pool-baseline (model-pool is TenantScaler's)", name)
	}
	lo, _, _ := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
	hi, _, _ := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount")
	poll, _, _ := unstructured.NestedInt64(so.Object, "spec", "pollingInterval")
	window, _, _ := unstructured.NestedInt64(so.Object, "spec", "advanced", "horizontalPodAutoscalerConfig", "behavior", "scaleDown", "stabilizationWindowSeconds")
	if lo != 1 || hi != 4 || poll != 15 || window != 300 {
		t.Errorf("side-by-side KEDA: replicas %d..%d, polling %d s, scale-down window %d s; want 1..4 (the demo budget), 15 s and 300 s", lo, hi, poll, window)
	}
	triggers, _, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if len(triggers) != 1 {
		t.Fatalf("side-by-side ScaledObject has %d triggers, want 1", len(triggers))
	}
	trigger := triggers[0].(map[string]interface{})
	meta, _, _ := unstructured.NestedStringMap(trigger, "metadata")
	auth, _, _ := unstructured.NestedString(trigger, "authenticationRef", "name")
	// In flight per replica = the simulated engine's running requests plus
	// the queue-based baseline's 4 waiting ones.
	want := strconv.Itoa(engine.DefaultParams().MaxNumSeqs + 4)
	if trigger["type"] != "metrics-api" || trigger["metricType"] != "AverageValue" ||
		!strings.HasPrefix(meta["url"], "http://gateway-baseline.tenantscaler.svc.cluster.local:8080/stats") ||
		meta["valueLocation"] != "inFlight" || meta["targetValue"] != want || meta["authMode"] != "bearer" {
		t.Errorf("side-by-side KEDA trigger = %v, want metrics-api on gateway-baseline /stats inFlight, %s per replica (AverageValue), bearer auth", trigger, want)
	}
	ta := find(t, ms, "TriggerAuthentication", auth)
	refs, _, _ := unstructured.NestedSlice(ta.Object, "spec", "secretTargetRef")
	if len(refs) != 1 {
		t.Fatalf("TriggerAuthentication %s has %d secret refs, want 1", auth, len(refs))
	}
	ref := refs[0].(map[string]interface{})
	if ref["parameter"] != "token" || ref["name"] != keysSecret || ref["key"] != "TS_ADMIN_KEY" {
		t.Errorf("TriggerAuthentication %s ref = %v, want the token from %s/TS_ADMIN_KEY", auth, ref, keysSecret)
	}
}
