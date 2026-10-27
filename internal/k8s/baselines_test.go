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
)

func TestBaselinesTargetModelPool(t *testing.T) {
	ms := loadManifests(t)
	for _, m := range append(byKind(ms, "ScaledObject"), byKind(ms, "HorizontalPodAutoscaler")...) {
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
	autoscalers := append(byKind(ms, "ScaledObject"), byKind(ms, "HorizontalPodAutoscaler")...)
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
