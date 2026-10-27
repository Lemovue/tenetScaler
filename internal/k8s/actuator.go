package k8s

import (
	"context"
	"fmt"
	"math"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"tenantscaler/internal/model"
)

// defaultDeploymentReplicas is the API default for an unset spec.replicas.
const defaultDeploymentReplicas = 1

// DeploymentActuator applies the controller's pool size to a Deployment in
// the client's namespace through the scale subresource, the same interface
// HPA and KEDA use, so RBAC can grant deployments/scale without full write
// access to the Deployment.
type DeploymentActuator struct {
	C          *Client
	Deployment string
}

// SetReplicas sets the Deployment's desired replica count. It is a no-op
// when the count is already n, and retries once on a write conflict.
func (a DeploymentActuator) SetReplicas(ctx context.Context, n int) error {
	if n < 0 || n > math.MaxInt32 {
		return fmt.Errorf("scale deployment %s/%s: replicas must be in [0, %d], got %d", a.C.Namespace, a.Deployment, math.MaxInt32, n)
	}
	deployments := a.C.Kube.AppsV1().Deployments(a.C.Namespace)
	err := retryOnConflictOnce(func() error {
		scale, err := deployments.GetScale(ctx, a.Deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if int(scale.Spec.Replicas) == n {
			return nil
		}
		scale.Spec.Replicas = int32(n)
		_, err = deployments.UpdateScale(ctx, a.Deployment, scale, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("scale deployment %s/%s to %d: %w", a.C.Namespace, a.Deployment, n, err)
	}
	return nil
}

// Status reads the Deployment and reports the pool as the controller sees
// it: Ready = status.readyReplicas, Desired = spec.replicas, and Starting =
// pods that exist but are not ready yet (cold-starting replicas).
func (a DeploymentActuator) Status(ctx context.Context) (model.PoolObservation, error) {
	d, err := a.C.Kube.AppsV1().Deployments(a.C.Namespace).Get(ctx, a.Deployment, metav1.GetOptions{})
	if err != nil {
		return model.PoolObservation{}, fmt.Errorf("read deployment %s/%s: %w", a.C.Namespace, a.Deployment, err)
	}
	desired := defaultDeploymentReplicas
	if d.Spec.Replicas != nil {
		desired = int(*d.Spec.Replicas)
	}
	ready := int(d.Status.ReadyReplicas)
	return model.PoolObservation{
		ReadyReplicas:    ready,
		StartingReplicas: max(0, int(d.Status.Replicas)-ready),
		DesiredReplicas:  desired,
	}, nil
}
