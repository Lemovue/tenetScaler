// Package k8s is TenantScaler's Kubernetes mode. It reads TenantScaler custom
// resources (one per tenant), writes their status, scales the shared model
// pool Deployment through its scale subresource, and discovers ready pool
// replicas from EndpointSlices. The matching manifests live in deploy/k8s.
package k8s

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"tenantscaler/internal/model"
)

// Identity of the TenantScaler custom resource. These must match
// deploy/k8s/01-crd.yaml (a test enforces it).
const (
	Group    = "scaling.tenantscaler.dev"
	Version  = "v1alpha1"
	Resource = "tenantscalers"
	Kind     = "TenantScaler"
)

// GVR is the group/version/resource used with the dynamic client.
var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

// TenantScalerSpec is one tenant's promise on a shared pool. Field names and
// JSON tags match the CRD's openAPIV3Schema.
type TenantScalerSpec struct {
	// Pool names the shared replica pool this tenant belongs to; the
	// controller only reads tenants of its own pool.
	Pool string `json:"pool"`
	// GuaranteedReplicas is the share the tenant always gets when it needs it.
	GuaranteedReplicas int `json:"guaranteedReplicas"`
	// MaxReplicas caps what the tenant may hold, borrowing included.
	MaxReplicas int `json:"maxReplicas"`
	// Priority orders tenants when spare capacity is lent: higher first.
	Priority int `json:"priority"`
	// SLO is the tenant's latency promise.
	SLO model.SLO `json:"slo"`
}

// TenantScalerStatus is what the controller reports back on the resource
// every control cycle.
type TenantScalerStatus struct {
	// Grant is the number of replicas the arbiter granted this cycle.
	Grant int `json:"grant"`
	// Demand is the replicas the tenant needs (reactive or forecast).
	Demand int `json:"demand"`
	// Shedding is true when the gateway returns 429 to this tenant's
	// sheddable requests.
	Shedding bool `json:"shedding"`
	// SLOSlack is the tenant's distance from its SLO target; negative
	// means it is missing the SLO. It must be finite.
	SLOSlack float64 `json:"sloSlack"`
	// Reason is the human-readable explanation of the decision.
	Reason string `json:"reason,omitempty"`
	// PolicyVersion is the gateway policy version that enforces the grant.
	PolicyVersion int64 `json:"policyVersion,omitempty"`
	// ObservedGeneration is the metadata.generation the decision was based on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastUpdate is when the controller wrote this status.
	LastUpdate metav1.Time `json:"lastUpdate,omitzero"`
}

// TenantScaler is the custom resource: one tenant on a shared pool.
type TenantScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantScalerSpec   `json:"spec"`
	Status TenantScalerStatus `json:"status,omitzero"`
}

// FromUnstructured converts a TenantScaler read with the dynamic client. It
// rejects objects of another group or kind. Unknown fields are ignored, as
// the API server prunes them anyway.
func FromUnstructured(u *unstructured.Unstructured) (TenantScaler, error) {
	if u == nil {
		return TenantScaler{}, fmt.Errorf("convert TenantScaler: object is nil")
	}
	gvk := u.GroupVersionKind()
	if gvk.Group != Group || gvk.Version != Version || gvk.Kind != Kind {
		return TenantScaler{}, fmt.Errorf("convert %q: got apiVersion %q kind %q, want %s/%s %s",
			u.GetName(), u.GetAPIVersion(), u.GetKind(), Group, Version, Kind)
	}
	var t TenantScaler
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &t); err != nil {
		return TenantScaler{}, fmt.Errorf("convert TenantScaler %q: %w", u.GetName(), err)
	}
	return t, nil
}

// TenantSpec returns the tenant configuration the controller core uses. The
// tenant name is the resource's metadata.name.
func (t TenantScaler) TenantSpec() model.TenantSpec {
	return model.TenantSpec{
		Name:               t.Name,
		GuaranteedReplicas: t.Spec.GuaranteedReplicas,
		MaxReplicas:        t.Spec.MaxReplicas,
		Priority:           t.Spec.Priority,
		SLO:                t.Spec.SLO,
	}
}
