package k8s

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// userAgent identifies TenantScaler in API server audit logs.
const userAgent = "tenantscaler"

// serviceAccountNamespaceFile is where Kubernetes mounts the pod's own
// namespace. It is a variable so tests can point it at a temporary file.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Client bundles the two API clients TenantScaler needs, scoped to one
// namespace: the dynamic client for TenantScaler resources and the typed
// clientset for Deployments and EndpointSlices. Tests build it directly from
// client-go fakes.
type Client struct {
	Dynamic   dynamic.Interface
	Kube      kubernetes.Interface
	Namespace string
}

// NewClient connects to a cluster. An empty kubeconfig means in-cluster
// configuration (the pod's service account); otherwise the file is loaded
// with its current context. An empty namespace means the pod's own
// namespace in-cluster and is an error with a kubeconfig.
func NewClient(kubeconfig, namespace string) (*Client, error) {
	namespace, err := resolveNamespace(kubeconfig, namespace)
	if err != nil {
		return nil, err
	}
	var cfg *rest.Config
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes client: in-cluster config (pass a kubeconfig path when running outside a cluster): %w", err)
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubernetes client: load kubeconfig %s: %w", kubeconfig, err)
		}
	}
	cfg.UserAgent = userAgent
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: clientset: %w", err)
	}
	return &Client{Dynamic: dyn, Kube: kube, Namespace: namespace}, nil
}

// resolveNamespace returns namespace when it is set. Otherwise, in-cluster
// (no kubeconfig), it returns the namespace of the pod's service account, so
// a component started without -namespace stays in its own namespace instead
// of failing or reaching into one its Role does not cover.
func resolveNamespace(kubeconfig, namespace string) (string, error) {
	if namespace != "" {
		return namespace, nil
	}
	if kubeconfig != "" {
		return "", fmt.Errorf("kubernetes client: namespace is required with a kubeconfig")
	}
	data, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return "", fmt.Errorf("kubernetes client: namespace is required (no in-cluster default: %w)", err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("kubernetes client: namespace is required (%s is empty)", serviceAccountNamespaceFile)
	}
	return ns, nil
}

// ListTenants returns the TenantScalers in the client's namespace whose
// spec.pool equals pool (all of them when pool is empty), sorted by name.
// Resources of other pools are skipped before they are decoded, so another
// pool's malformed resource cannot stop this pool's controller. A malformed
// resource of this pool, or one whose spec.pool is not a string (so its pool
// is unknown), fails the whole list: the controller then fails static
// instead of silently dropping a tenant's guarantee.
func (c *Client) ListTenants(ctx context.Context, pool string) ([]TenantScaler, error) {
	list, err := c.Dynamic.Resource(GVR).Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s in namespace %s: %w", Resource, c.Namespace, err)
	}
	out := make([]TenantScaler, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		if p, _, err := unstructured.NestedString(item.Object, "spec", "pool"); pool != "" && err == nil && p != pool {
			continue
		}
		t, err := FromUnstructured(item)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateStatus replaces the status of one TenantScaler. It reads the
// resource first so the write carries its resourceVersion (optimistic
// concurrency), and retries once on a conflict with a fresh read.
func (c *Client) UpdateStatus(ctx context.Context, name string, st TenantScalerStatus) error {
	if math.IsNaN(st.SLOSlack) || math.IsInf(st.SLOSlack, 0) {
		return fmt.Errorf("update status of %s %s/%s: sloSlack must be finite, got %v", Kind, c.Namespace, name, st.SLOSlack)
	}
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		return fmt.Errorf("update status of %s %s/%s: encode status: %w", Kind, c.Namespace, name, err)
	}
	res := c.Dynamic.Resource(GVR).Namespace(c.Namespace)
	err = retryOnConflictOnce(func() error {
		u, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		u.Object["status"] = status
		_, err = res.UpdateStatus(ctx, u, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("update status of %s %s/%s: %w", Kind, c.Namespace, name, err)
	}
	return nil
}

// retryOnConflictOnce runs a read-modify-write function and, if the write
// lost a race (409 Conflict), runs it exactly once more.
func retryOnConflictOnce(fn func() error) error {
	err := fn()
	if apierrors.IsConflict(err) {
		err = fn()
	}
	return err
}
