package k8s

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
)

// crdVersion returns the CRD's single version entry and its root schema.
func crdVersion(t *testing.T, crd *unstructured.Unstructured) (version, root map[string]interface{}) {
	t.Helper()
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if len(versions) != 1 {
		t.Fatalf("CRD has %d versions, want exactly 1", len(versions))
	}
	version = versions[0].(map[string]interface{})
	root, ok, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
	if !ok {
		t.Fatal("CRD version has no schema.openAPIV3Schema")
	}
	return version, root
}

// schemaType maps the Go values the unstructured converter produces to
// OpenAPI types.
func schemaType(v interface{}) string {
	switch v.(type) {
	case map[string]interface{}:
		return "object"
	case int64:
		return "integer"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case string:
		return "string"
	}
	return fmt.Sprintf("%T", v)
}

// checkAgainstSchema reports every key of obj the schema does not declare
// (the API server would prune it) or declares with another type. With
// exact, it also reports schema properties obj lacks.
func checkAgainstSchema(t *testing.T, where string, obj, schema map[string]interface{}, exact bool) {
	t.Helper()
	props, _ := schema["properties"].(map[string]interface{})
	for k, v := range obj {
		prop, ok := props[k].(map[string]interface{})
		if !ok {
			t.Errorf("%s.%s is not declared in the CRD schema", where, k)
			continue
		}
		got, want := schemaType(v), prop["type"]
		if got != want && !(got == "integer" && want == "number") {
			t.Errorf("%s.%s: value type %s, schema type %v", where, k, got, want)
		}
		if child, ok := v.(map[string]interface{}); ok {
			checkAgainstSchema(t, where+"."+k, child, prop, exact)
		}
	}
	if exact {
		for k := range props {
			if _, ok := obj[k]; !ok {
				t.Errorf("CRD schema declares %s.%s but the Go type has no such JSON field", where, k)
			}
		}
	}
}

func TestCRDMatchesGoTypes(t *testing.T) {
	crd := find(t, loadManifests(t), "CustomResourceDefinition", Resource+"."+Group)
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(crd.Object, path...)
		return s
	}
	if crd.GetAPIVersion() != "apiextensions.k8s.io/v1" {
		t.Errorf("CRD apiVersion = %s", crd.GetAPIVersion())
	}
	for _, c := range []struct{ got, want, what string }{
		{str("spec", "group"), Group, "group"},
		{str("spec", "names", "plural"), Resource, "plural"},
		{str("spec", "names", "kind"), Kind, "kind"},
		{str("spec", "scope"), "Namespaced", "scope"},
	} {
		if c.got != c.want {
			t.Errorf("CRD %s = %q, want %q", c.what, c.got, c.want)
		}
	}
	version, root := crdVersion(t, crd)
	if version["name"] != Version || version["served"] != true || version["storage"] != true {
		t.Errorf("CRD version = %v served %v storage %v, want %s served and stored", version["name"], version["served"], version["storage"], Version)
	}
	if _, ok, _ := unstructured.NestedMap(version, "subresources", "status"); !ok {
		t.Error("CRD must enable the status subresource")
	}
	columns := map[string]string{}
	cols, _, _ := unstructured.NestedSlice(version, "additionalPrinterColumns")
	for _, c := range cols {
		c := c.(map[string]interface{})
		columns[c["name"].(string)] = c["jsonPath"].(string)
	}
	for name, path := range map[string]string{"Grant": ".status.grant", "Shedding": ".status.shedding", "Demand": ".status.demand"} {
		if columns[name] != path {
			t.Errorf("printer column %s = %q, want %q", name, columns[name], path)
		}
	}

	spec, _, _ := unstructured.NestedMap(root, "properties", "spec")
	minimum := func(field string) interface{} {
		v, _, _ := unstructured.NestedFieldCopy(spec, "properties", field, "minimum")
		return v
	}
	if minimum("guaranteedReplicas") != int64(0) || minimum("maxReplicas") != int64(1) {
		t.Errorf("minimums: guaranteedReplicas %v, maxReplicas %v; want 0 and 1", minimum("guaranteedReplicas"), minimum("maxReplicas"))
	}
	required, _, _ := unstructured.NestedStringSlice(spec, "properties", "slo", "required")
	if strings.Join(required, ",") != "ttftSeconds,tpotMillis,targetFraction" {
		t.Errorf("slo.required = %v", required)
	}
	// The same bounds as config.ValidateTenants: latencies > 0 and
	// targetFraction in (0, 1]. A nil want means the keyword must be absent.
	for field, bounds := range map[string]map[string]interface{}{
		"ttftSeconds":    {"minimum": int64(0), "exclusiveMinimum": true, "maximum": nil},
		"tpotMillis":     {"minimum": int64(0), "exclusiveMinimum": true, "maximum": nil},
		"targetFraction": {"minimum": int64(0), "exclusiveMinimum": true, "maximum": int64(1), "exclusiveMaximum": nil},
	} {
		prop, _, _ := unstructured.NestedMap(spec, "properties", "slo", "properties", field)
		for keyword, want := range bounds {
			if prop[keyword] != want {
				t.Errorf("slo.%s %s = %v, want %v", field, keyword, prop[keyword], want)
			}
		}
	}

	full := TenantScaler{
		TypeMeta:   metav1.TypeMeta{APIVersion: Group + "/" + Version, Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: TenantScalerSpec{Pool: "p", GuaranteedReplicas: 1, MaxReplicas: 2, Priority: 3,
			SLO: model.SLO{TTFTSeconds: 1.5, TPOTMillis: 50.5, TargetFraction: 0.9}},
		Status: TenantScalerStatus{Grant: 1, Demand: 2, Shedding: true, SLOSlack: 0.5, Reason: "r",
			PolicyVersion: 4, ObservedGeneration: 5, LastUpdate: metav1.NewTime(time.Unix(1, 0))},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&full)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"spec", "status"} {
		schema, _, _ := unstructured.NestedMap(root, "properties", part)
		checkAgainstSchema(t, part, obj[part].(map[string]interface{}), schema, true)
	}
}

func TestSampleTenantsAreValid(t *testing.T) {
	ms := loadManifests(t)
	_, root := crdVersion(t, find(t, ms, "CustomResourceDefinition", Resource+"."+Group))
	specSchema, _, _ := unstructured.NestedMap(root, "properties", "spec")
	flags := flagValues(containerArgs(t, find(t, deployed(ms), "Deployment", "tenantscaler-controller")))
	capacity, err := strconv.Atoi(flags["-capacity"])
	if err != nil {
		t.Fatalf("controller -capacity: %v", err)
	}

	wantSLO := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	var specs []model.TenantSpec
	for _, m := range byKind(deployed(ms), Kind) {
		ts, err := FromUnstructured(m.obj)
		if err != nil {
			t.Fatalf("%v: %v", m, err)
		}
		spec, _, _ := unstructured.NestedMap(m.obj.Object, "spec")
		checkAgainstSchema(t, m.String()+" spec", spec, specSchema, false)
		if ts.Spec.Pool != flags["-pool"] {
			t.Errorf("%v: pool %q is not the controller's -pool %q", m, ts.Spec.Pool, flags["-pool"])
		}
		if ts.Spec.GuaranteedReplicas != 3 || ts.Spec.MaxReplicas != 8 || ts.Spec.SLO != wantSLO {
			t.Errorf("%v: spec = %+v, want guarantee 3, max 8, SLO %+v", m, ts.Spec, wantSLO)
		}
		specs = append(specs, ts.TenantSpec())
	}
	if len(specs) != 3 {
		t.Fatalf("got %d sample tenants, want 3", len(specs))
	}
	if err := config.ValidateTenants(specs, capacity); err != nil {
		t.Errorf("sample tenants are invalid for capacity %d: %v", capacity, err)
	}
}
