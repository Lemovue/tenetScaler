package k8s

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// deployDir is the repository's deploy/ directory, seen from this package.
var deployDir = filepath.Join("..", "..", "deploy")

// manifest is one YAML document from deploy/.
type manifest struct {
	file string // path relative to deploy/, slash-separated
	doc  int    // 1-based document index within the file
	obj  *unstructured.Unstructured
}

func (m manifest) String() string {
	return fmt.Sprintf("%s#%d (%s %s)", m.file, m.doc, m.obj.GetKind(), m.obj.GetName())
}

// loadManifests reads every .yaml/.yml file under deploy/, splits it into
// documents the way kubectl does, and decodes each non-empty document.
func loadManifests(t *testing.T) []manifest {
	t.Helper()
	var out []manifest
	err := filepath.WalkDir(deployDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := filepath.Ext(path); d.IsDir() || (ext != ".yaml" && ext != ".yml") {
			return nil
		}
		rel, err := filepath.Rel(deployDir, path)
		if err != nil {
			return err
		}
		docs, err := decodeFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		for i, u := range docs {
			if u != nil {
				out = append(out, manifest{file: filepath.ToSlash(rel), doc: i + 1, obj: u})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	return out
}

// decodeFile returns one object per YAML document; comment-only documents
// decode to nil. Duplicate keys are errors.
func decodeFile(path string) ([]*unstructured.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []*unstructured.Unstructured
	for i := 1; ; i++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		js, err := yaml.YAMLToJSONStrict(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if string(bytes.TrimSpace(js)) == "null" {
			out = append(out, nil)
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(js); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		out = append(out, u)
	}
}

// applySet names the group of files a manifest is applied with: "k8s" for
// the top level of deploy/k8s (`kubectl apply -f deploy/k8s/`), otherwise
// its directory, such as "k8s/baselines" (applied file by file).
func applySet(file string) string { return path.Dir(file) }

// inSet returns the manifests of one apply set.
func inSet(ms []manifest, set string) []manifest {
	var out []manifest
	for _, m := range ms {
		if applySet(m.file) == set {
			out = append(out, m)
		}
	}
	return out
}

// deployed returns the manifests `kubectl apply -f deploy/k8s/` applies.
func deployed(ms []manifest) []manifest { return inSet(ms, "k8s") }

// overlays are objects another apply set deliberately re-applies with a
// change, keyed by identity, with every set that may hold a copy (the
// original's first). TestPassthroughGatewayOverlay checks the gateway's
// change; k3s/llama swaps the simulated pools for llama.cpp ones under the
// same names.
var overlays = map[string][]string{
	"apps/v1/Deployment/tenantscaler/gateway":             {"k8s", "k8s/baselines"},
	"apps/v1/Deployment/tenantscaler/model-pool":          {"k8s", "k3s/llama"},
	"apps/v1/Deployment/tenantscaler/model-pool-baseline": {"k3s/side-by-side", "k3s/llama"},
}

func byKind(ms []manifest, kind string) []manifest {
	var out []manifest
	for _, m := range ms {
		if m.obj.GetKind() == kind {
			out = append(out, m)
		}
	}
	return out
}

// findManifest returns the one manifest of kind and name in ms and fails
// when there is none or more than one.
func findManifest(t *testing.T, ms []manifest, kind, name string) manifest {
	t.Helper()
	var found []manifest
	for _, m := range ms {
		if m.obj.GetKind() == kind && m.obj.GetName() == name {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d %s %q under deploy/ (%v), want exactly 1", len(found), kind, name, found)
	}
	return found[0]
}

func find(t *testing.T, ms []manifest, kind, name string) *unstructured.Unstructured {
	t.Helper()
	return findManifest(t, ms, kind, name).obj
}

func TestManifestsParseAndAreComplete(t *testing.T) {
	ms := loadManifests(t)
	clusterScoped := map[string]bool{"Namespace": true, "CustomResourceDefinition": true, "PersistentVolume": true}
	seen := map[string][]manifest{}
	for _, m := range ms {
		u := m.obj
		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			t.Errorf("%v: apiVersion and kind are required", m)
		}
		if u.GetKind() == "Cluster" && strings.HasPrefix(u.GetAPIVersion(), "kind.x-k8s.io/") {
			if name, _, _ := unstructured.NestedString(u.Object, "name"); name == "" {
				t.Errorf("%v: kind cluster config needs a name", m)
			}
			continue
		}
		if u.GetName() == "" {
			t.Errorf("%v: metadata.name is required", m)
		}
		switch ns := u.GetNamespace(); {
		case clusterScoped[u.GetKind()] && ns != "":
			t.Errorf("%v: cluster-scoped object must not set a namespace", m)
		case !clusterScoped[u.GetKind()] && ns != testNamespace:
			t.Errorf("%v: namespace = %q, want %q", m, ns, testNamespace)
		}
		// Within one apply set every object is unique. Across sets only a
		// listed overlay may repeat an object, in its listed sets.
		id := u.GetAPIVersion() + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
		for _, prev := range seen[id] {
			allowed, a, b := overlays[id], applySet(prev.file), applySet(m.file)
			if a == b || !slices.Contains(allowed, a) || !slices.Contains(allowed, b) {
				t.Errorf("%v duplicates %v", m, prev)
			}
		}
		seen[id] = append(seen[id], m)
	}

	want := []struct{ set, kind, name string }{
		{"k8s", "Namespace", testNamespace},
		{"k8s", "CustomResourceDefinition", Resource + "." + Group},
		{"k8s", "ServiceAccount", "tenantscaler-controller"},
		{"k8s", "Role", "tenantscaler-controller"},
		{"k8s", "RoleBinding", "tenantscaler-controller"},
		{"k8s", "ServiceAccount", "gateway"},
		{"k8s", "Role", "gateway"},
		{"k8s", "RoleBinding", "gateway"},
		{"k8s", "Deployment", "model-pool"},
		{"k8s", "Service", "model-pool"},
		{"k8s", "ConfigMap", "gateway-config"},
		{"k8s", "Deployment", "gateway"},
		{"k8s", "Service", "gateway"},
		{"k8s", "Deployment", "tenantscaler-controller"},
		{"k8s", "Service", "tenantscaler-controller"},
		{"k8s", Kind, "a"}, {"k8s", Kind, "b"}, {"k8s", Kind, "c"},
		{"k8s/baselines", "ScaledObject", "model-pool-queue"},
		{"k8s/baselines", "HorizontalPodAutoscaler", "model-pool-cpu"},
		{"k8s/baselines", "Deployment", "gateway"},
		{"kind", "Cluster", ""},
	}
	for _, w := range want {
		if w.name == "" {
			if len(byKind(inSet(ms, w.set), w.kind)) == 0 {
				t.Errorf("no %s under deploy/%s", w.kind, w.set)
			}
			continue
		}
		find(t, inSet(ms, w.set), w.kind, w.name)
	}
}

// flagValues maps "-name" to its value in a container's args; a flag
// followed by another flag (or nothing) is a boolean and maps to "true".
func flagValues(args []string) map[string]string {
	out := map[string]string{}
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out[a] = args[i+1]
		} else {
			out[a] = "true"
		}
	}
	return out
}

// externalKinds are the manifests whose Go types client-go's scheme does
// not hold: the CRD (k8s.io/apiextensions-apiserver is not a dependency),
// KEDA's ScaledObject and TriggerAuthentication, kind's cluster config and
// the TenantScaler itself (checked against the CRD schema by
// TestSampleTenantsAreValid).
var externalKinds = map[string]bool{
	"apiextensions.k8s.io/v1/CustomResourceDefinition": true,
	"keda.sh/v1alpha1/ScaledObject":                    true,
	"keda.sh/v1alpha1/TriggerAuthentication":           true,
	"kind.x-k8s.io/v1alpha4/Cluster":                   true,
	Group + "/" + Version + "/" + Kind:                 true,
}

// strictDecode decodes a built-in object into its typed Go struct the way
// the API server's strict field validation does: unknown, misspelled,
// wrongly cased or misplaced fields are errors.
func strictDecode(u *unstructured.Unstructured) error {
	js, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	strict := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, scheme.Scheme, scheme.Scheme,
		kjson.SerializerOptions{Strict: true})
	_, _, err = strict.Decode(js, nil, nil)
	return err
}

func TestBuiltinManifestsDecodeStrictly(t *testing.T) {
	ms := loadManifests(t)
	for _, m := range ms {
		if externalKinds[m.obj.GetAPIVersion()+"/"+m.obj.GetKind()] {
			continue
		}
		if err := strictDecode(m.obj); err != nil {
			t.Errorf("%v: %v (if this is not a built-in kind, list it in externalKinds)", m, err)
		}
	}

	// The check must catch the mistakes it exists for.
	gateway := find(t, deployed(ms), "Deployment", "gateway")
	for _, tt := range []struct {
		name  string
		value interface{}
		path  []string
	}{
		{"misspelled pod field", int64(30), []string{"spec", "template", "spec", "terminationGracePeriodSecond"}},
		{"wrongly cased pod field", int64(30), []string{"spec", "template", "spec", "TerminationGracePeriodSeconds"}},
		{"field at the wrong level", "RuntimeDefault", []string{"spec", "template", "seccompProfile"}},
	} {
		bad := gateway.DeepCopy()
		if err := unstructured.SetNestedField(bad.Object, tt.value, tt.path...); err != nil {
			t.Fatal(err)
		}
		if err := strictDecode(bad); !runtime.IsStrictDecodingError(err) {
			t.Errorf("%s: err = %v, want a strict decoding error", tt.name, err)
		}
	}
}
