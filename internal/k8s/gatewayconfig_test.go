package k8s

import (
	"cmp"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
)

// mountedConfigMapKey resolves a file path in a Deployment's single
// container to the ConfigMap and key that provide it, and reports whether
// the mount is read-only.
func mountedConfigMapKey(t *testing.T, d *unstructured.Unstructured, file string) (configMap, key string, readOnly bool) {
	t.Helper()
	mounts, _, _ := unstructured.NestedSlice(containers(t, d)[0], "volumeMounts")
	var volume string
	for _, m := range mounts {
		m := m.(map[string]interface{})
		if m["mountPath"] == path.Dir(file) && m["subPath"] == nil {
			volume, readOnly = m["name"].(string), m["readOnly"] == true
		}
	}
	if volume == "" {
		t.Fatalf("deployment %s mounts no volume at %s", d.GetName(), path.Dir(file))
	}
	volumes, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		v := v.(map[string]interface{})
		if v["name"] != volume {
			continue
		}
		name, _, _ := unstructured.NestedString(v, "configMap", "name")
		if _, remapped, _ := unstructured.NestedFieldNoCopy(v, "configMap", "items"); name == "" || remapped {
			t.Fatalf("deployment %s volume %s must be a whole ConfigMap (no items)", d.GetName(), volume)
		}
		return name, path.Base(file), readOnly
	}
	t.Fatalf("deployment %s has no volume %s", d.GetName(), volume)
	return "", "", false
}

func sortedByName(ts []model.TenantSpec) []model.TenantSpec {
	out := slices.Clone(ts)
	slices.SortFunc(out, func(a, b model.TenantSpec) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// TestGatewayConfigMatchesTenantScalers pins one tenant identity across the
// deployment. The gateway answers 400 to tenants missing from its config;
// without -config it would use the built-in tenants A, B and C, which no
// TenantScaler can be named because Kubernetes names are lower case.
func TestGatewayConfigMatchesTenantScalers(t *testing.T) {
	ms := deployed(loadManifests(t))
	gw := find(t, ms, "Deployment", "gateway")
	file := flagValues(containerArgs(t, gw))["-config"]
	if file == "" {
		t.Fatal("gateway has no -config: it would serve the built-in tenants A, B, C instead of the TenantScalers")
	}
	cmName, key, readOnly := mountedConfigMapKey(t, gw, file)
	if !readOnly {
		t.Errorf("gateway config volume must be mounted read-only")
	}
	data, ok, _ := unstructured.NestedString(find(t, ms, "ConfigMap", cmName).Object, "data", key)
	if !ok {
		t.Fatalf("ConfigMap %s has no key %q", cmName, key)
	}
	// Load it exactly as the gateway does: strict YAML on top of the
	// defaults, then validation.
	tmp := filepath.Join(t.TempDir(), key)
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(tmp)
	if err != nil {
		t.Fatalf("ConfigMap %s/%s: %v", cmName, key, err)
	}

	var crs []model.TenantSpec
	for _, m := range byKind(ms, Kind) {
		ts, err := FromUnstructured(m.obj)
		if err != nil {
			t.Fatalf("%v: %v", m, err)
		}
		crs = append(crs, ts.TenantSpec())
	}
	if got, want := sortedByName(cfg.Tenants), sortedByName(crs); !slices.Equal(got, want) {
		t.Errorf("gateway tenants %+v, want the TenantScalers %+v", got, want)
	}

	ctl := flagValues(containerArgs(t, find(t, ms, "Deployment", "tenantscaler-controller")))
	if cfg.Pool.Name != ctl["-pool"] || strconv.Itoa(cfg.Pool.CapacityReplicas) != ctl["-capacity"] {
		t.Errorf("gateway pool %q capacity %d, controller -pool %q -capacity %q",
			cfg.Pool.Name, cfg.Pool.CapacityReplicas, ctl["-pool"], ctl["-capacity"])
	}
	// The controller has no -config, so it runs on the built-in defaults
	// with tenants from the TenantScalers. Everything else in the gateway's
	// config must match those defaults; in particular a different
	// slotsPerReplica would make the controller's critical slots disagree
	// with the gateway's dispatch window.
	if _, set := ctl["-config"]; set {
		t.Fatal("controller takes -config now: compare the gateway's config with that file instead of the defaults")
	}
	want := config.Default()
	want.Tenants = cfg.Tenants
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("gateway config differs from the built-in defaults outside tenants:\n got %+v\nwant %+v", cfg, want)
	}
}
