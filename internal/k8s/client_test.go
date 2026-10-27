package k8s

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeClient returns a Client backed by client-go fakes: tenants go to
// the dynamic fake, everything else to the typed clientset fake.
func newFakeClient(tenants []*unstructured.Unstructured, kubeObjects ...runtime.Object) (*Client, *dynamicfake.FakeDynamicClient, *kubefake.Clientset) {
	objs := make([]runtime.Object, len(tenants))
	for i, t := range tenants {
		objs[i] = t
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: Kind + "List"}, objs...)
	kube := kubefake.NewClientset(kubeObjects...)
	return &Client{Dynamic: dyn, Kube: kube, Namespace: testNamespace}, dyn, kube
}

// conflictReactor fails the first `failures` matching calls with 409 and
// counts every call it sees.
func conflictReactor(resource string, failures int, calls *int) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		*calls++
		if *calls <= failures {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: resource}, "x", nil)
		}
		return false, nil, nil
	}
}

func TestListTenantsFiltersByPoolAndSorts(t *testing.T) {
	other := tenantObject("z", "shared", 1)
	other.SetNamespace("elsewhere")
	c, _, _ := newFakeClient([]*unstructured.Unstructured{
		tenantObject("c", "shared", 3),
		tenantObject("a", "shared", 3),
		tenantObject("b", "batch-pool", 2),
		other,
	})
	tests := []struct {
		pool string
		want []string
	}{
		{"shared", []string{"a", "c"}},
		{"batch-pool", []string{"b"}},
		{"", []string{"a", "b", "c"}},
		{"missing", nil},
	}
	for _, tt := range tests {
		got, err := c.ListTenants(context.Background(), tt.pool)
		if err != nil {
			t.Fatalf("ListTenants(%q): %v", tt.pool, err)
		}
		var names []string
		for _, ts := range got {
			names = append(names, ts.Name)
		}
		if strings.Join(names, ",") != strings.Join(tt.want, ",") {
			t.Errorf("ListTenants(%q) = %v, want %v", tt.pool, names, tt.want)
		}
	}
}

func TestListTenantsMalformed(t *testing.T) {
	// broken belongs to batch-pool and has a non-integer maxReplicas;
	// lost has a spec.pool that is not a string, so its pool is unknown.
	broken := tenantObject("broken", "batch-pool", 1)
	if err := unstructured.SetNestedField(broken.Object, "lots", "spec", "maxReplicas"); err != nil {
		t.Fatal(err)
	}
	lost := tenantObject("lost", "", 1)
	if err := unstructured.SetNestedField(lost.Object, int64(7), "spec", "pool"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		objs    []*unstructured.Unstructured
		pool    string
		want    string // tenant names, comma-separated, when no error
		wantErr string // a tenant name the error must mention
	}{
		{"another pool's bad tenant is skipped", []*unstructured.Unstructured{tenantObject("a", "shared", 1), broken}, "shared", "a", ""},
		{"own pool's bad tenant fails the list", []*unstructured.Unstructured{tenantObject("a", "shared", 1), broken}, "batch-pool", "", "broken"},
		{"every pool's bad tenant fails an unfiltered list", []*unstructured.Unstructured{tenantObject("a", "shared", 1), broken}, "", "", "broken"},
		{"a tenant of unknown pool fails the list", []*unstructured.Unstructured{tenantObject("a", "shared", 1), lost}, "shared", "", "lost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _ := newFakeClient(tt.objs)
			got, err := c.ListTenants(context.Background(), tt.pool)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want an error naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListTenants(%q): %v", tt.pool, err)
			}
			var names []string
			for _, ts := range got {
				names = append(names, ts.Name)
			}
			if strings.Join(names, ",") != tt.want {
				t.Errorf("ListTenants(%q) = %v, want %s", tt.pool, names, tt.want)
			}
		})
	}
}

func TestUpdateStatusWritesStatusOnly(t *testing.T) {
	c, dyn, _ := newFakeClient([]*unstructured.Unstructured{tenantObject("a", "shared", 3)})
	st := TenantScalerStatus{
		Grant: 5, Demand: 6, Shedding: true, SLOSlack: -0.5, Reason: "reclaimed 1",
		PolicyVersion: 7, ObservedGeneration: 4,
		LastUpdate: metav1.NewTime(time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)),
	}
	if err := c.UpdateStatus(context.Background(), "a", st); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	var updates int
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" {
			updates++
			if a.GetSubresource() != "status" {
				t.Errorf("update went to subresource %q, want status", a.GetSubresource())
			}
		}
	}
	if updates != 1 {
		t.Errorf("got %d updates, want 1", updates)
	}
	u, err := dyn.Resource(GVR).Namespace(testNamespace).Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Status.LastUpdate.Equal(&st.LastUpdate) {
		t.Errorf("lastUpdate = %v, want %v", got.Status.LastUpdate, st.LastUpdate)
	}
	got.Status.LastUpdate = st.LastUpdate
	if got.Status != st {
		t.Errorf("status = %+v, want %+v", got.Status, st)
	}
	if got.Spec.GuaranteedReplicas != 3 || got.Spec.Pool != "shared" {
		t.Errorf("spec changed: %+v", got.Spec)
	}
}

// rvServer emulates the API server's optimistic concurrency for one
// TenantScaler, which the client-go fake tracker does not: every Get serves
// the object at the current resourceVersion, an update must carry exactly
// that version or gets 409, and a successful update bumps it. The first
// `races` updates lose to a concurrent writer that commits just before them.
type rvServer struct {
	dyn   *dynamicfake.FakeDynamicClient
	rv    int
	races int
	// gets and updates are the versions each Get served and each update carried.
	gets, updates []string
}

func (s *rvServer) get(k8stesting.Action) (bool, runtime.Object, error) {
	obj, err := s.dyn.Tracker().Get(GVR, testNamespace, "a")
	if err != nil {
		return true, nil, err
	}
	u := obj.DeepCopyObject().(*unstructured.Unstructured)
	u.SetResourceVersion(strconv.Itoa(s.rv))
	s.gets = append(s.gets, u.GetResourceVersion())
	return true, u, nil
}

func (s *rvServer) update(action k8stesting.Action) (bool, runtime.Object, error) {
	carried := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).GetResourceVersion()
	s.updates = append(s.updates, carried)
	if s.races > 0 {
		s.races--
		s.rv++
	}
	if carried != strconv.Itoa(s.rv) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: Group, Resource: Resource}, "a",
			fmt.Errorf("the object has been modified: resourceVersion %q, current %d", carried, s.rv))
	}
	s.rv++
	return false, nil, nil // let the tracker store it
}

func TestUpdateStatusOptimisticConcurrency(t *testing.T) {
	tests := []struct {
		name    string
		races   int
		wantErr bool
		// wantGets is the versions served by the Gets; each update must
		// carry the version of the Get just before it.
		wantGets []string
	}{
		{"no race", 0, false, []string{"7"}},
		{"one lost race is retried with a fresh read", 1, false, []string{"7", "8"}},
		{"a second lost race is returned", 2, true, []string{"7", "8"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed := tenantObject("a", "shared", 3)
			seed.SetResourceVersion("7")
			c, dyn, _ := newFakeClient([]*unstructured.Unstructured{seed})
			srv := &rvServer{dyn: dyn, rv: 7, races: tt.races}
			dyn.PrependReactor("get", Resource, srv.get)
			dyn.PrependReactor("update", Resource, srv.update)

			err := c.UpdateStatus(context.Background(), "a", TenantScalerStatus{Grant: 2})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !apierrors.IsConflict(err) {
				t.Errorf("err = %v, want a wrapped conflict", err)
			}
			if strings.Join(srv.gets, ",") != strings.Join(tt.wantGets, ",") {
				t.Errorf("gets served versions %v, want %v", srv.gets, tt.wantGets)
			}
			if strings.Join(srv.updates, ",") != strings.Join(srv.gets, ",") {
				t.Errorf("updates carried versions %v, want those of the preceding gets %v", srv.updates, srv.gets)
			}
			if tt.wantErr {
				return
			}
			u, err := dyn.Tracker().Get(GVR, testNamespace, "a")
			if err != nil {
				t.Fatal(err)
			}
			grant, _, _ := unstructured.NestedInt64(u.(*unstructured.Unstructured).Object, "status", "grant")
			if grant != 2 {
				t.Errorf("stored status.grant = %d, want 2", grant)
			}
		})
	}
}

func TestUpdateStatusErrors(t *testing.T) {
	c, _, _ := newFakeClient(nil)
	err := c.UpdateStatus(context.Background(), "ghost", TenantScalerStatus{})
	if !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("missing tenant: err = %v, want a wrapped not-found naming it", err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := c.UpdateStatus(context.Background(), "a", TenantScalerStatus{SLOSlack: v}); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Errorf("sloSlack %v: err = %v, want a finiteness error", v, err)
		}
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: not-a-real-token
`

func TestResolveNamespace(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	podNamespace := write("namespace", "team-x\n")
	emptyNamespace := write("empty", "  \n")
	tests := []struct {
		name, kubeconfig, namespace, saFile string
		want, wantErr                       string
	}{
		{"explicit namespace wins", "", "given", podNamespace, "given", ""},
		{"in-cluster default is the pod's namespace", "", "", podNamespace, "team-x", ""},
		{"kubeconfig needs a namespace", "/some/kubeconfig", "", podNamespace, "", "namespace is required with a kubeconfig"},
		{"not in a cluster", "", "", filepath.Join(dir, "absent"), "", "namespace is required"},
		{"empty namespace file", "", "", emptyNamespace, "", "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := serviceAccountNamespaceFile
			serviceAccountNamespaceFile = tt.saFile
			t.Cleanup(func() { serviceAccountNamespaceFile = old })
			got, err := resolveNamespace(tt.kubeconfig, tt.namespace)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveNamespace = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(path, testNamespace)
	if err != nil {
		t.Fatalf("NewClient(kubeconfig): %v", err)
	}
	if c.Dynamic == nil || c.Kube == nil || c.Namespace != testNamespace {
		t.Errorf("client not fully built: %+v", c)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	tests := []struct {
		name, kubeconfig, namespace, wantErr string
	}{
		{"no namespace", path, "", "namespace is required"},
		{"missing kubeconfig", filepath.Join(t.TempDir(), "absent"), testNamespace, "load kubeconfig"},
		{"not in a cluster", "", testNamespace, "in-cluster config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.kubeconfig, tt.namespace)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
