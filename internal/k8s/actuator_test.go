package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"tenantscaler/internal/model"
)

var deploymentsGVR = appsv1.SchemeGroupVersion.WithResource("deployments")

func deployment(name string, specReplicas *int32, statusReplicas, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: specReplicas},
		Status:     appsv1.DeploymentStatus{Replicas: statusReplicas, ReadyReplicas: ready},
	}
}

// addScaleReactors gives the fake clientset a deployments/scale subresource
// (the plain object tracker would return the Deployment itself) backed by
// the tracked Deployment's spec.replicas. It returns a pointer to the number
// of scale updates served.
func addScaleReactors(cs *kubefake.Clientset) *int {
	updates := new(int)
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		if get.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(deploymentsGVR, get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment)
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, ResourceVersion: d.ResourceVersion},
			Spec:       autoscalingv1.ScaleSpec{Replicas: *d.Spec.Replicas},
			Status:     autoscalingv1.ScaleStatus{Replicas: d.Status.Replicas},
		}, nil
	})
	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		upd := action.(k8stesting.UpdateAction)
		if upd.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := upd.GetObject().(*autoscalingv1.Scale)
		obj, err := cs.Tracker().Get(deploymentsGVR, upd.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Spec.Replicas = &scale.Spec.Replicas
		if err := cs.Tracker().Update(deploymentsGVR, d, upd.GetNamespace()); err != nil {
			return true, nil, err
		}
		*updates++
		return true, scale, nil
	})
	return updates
}

func specReplicas(t *testing.T, cs *kubefake.Clientset, name string) int32 {
	t.Helper()
	d, err := cs.AppsV1().Deployments(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func TestSetReplicas(t *testing.T) {
	c, _, cs := newFakeClient(nil, deployment("model-pool", new(int32(3)), 3, 3))
	updates := addScaleReactors(cs)
	act := DeploymentActuator{C: c, Deployment: "model-pool"}
	ctx := context.Background()

	steps := []struct {
		n           int
		wantUpdates int
	}{
		{6, 1}, // scale up
		{6, 1}, // unchanged: no write
		{0, 2}, // scale to zero is allowed
		{2, 3},
	}
	for _, s := range steps {
		if err := act.SetReplicas(ctx, s.n); err != nil {
			t.Fatalf("SetReplicas(%d): %v", s.n, err)
		}
		if got := specReplicas(t, cs, "model-pool"); got != int32(s.n) {
			t.Errorf("after SetReplicas(%d): spec.replicas = %d", s.n, got)
		}
		if *updates != s.wantUpdates {
			t.Errorf("after SetReplicas(%d): %d scale updates, want %d", s.n, *updates, s.wantUpdates)
		}
	}
}

func TestSetReplicasErrors(t *testing.T) {
	c, _, cs := newFakeClient(nil, deployment("model-pool", new(int32(1)), 1, 1))
	addScaleReactors(cs)
	ctx := context.Background()

	for _, n := range []int{-1, 1 << 31} {
		err := DeploymentActuator{C: c, Deployment: "model-pool"}.SetReplicas(ctx, n)
		if err == nil || !strings.Contains(err.Error(), "replicas must be in") {
			t.Errorf("SetReplicas(%d): err = %v, want a range error", n, err)
		}
	}
	err := DeploymentActuator{C: c, Deployment: "absent"}.SetReplicas(ctx, 2)
	if !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "absent") {
		t.Errorf("missing deployment: err = %v, want a wrapped not-found naming it", err)
	}
}

func TestSetReplicasConflictRetry(t *testing.T) {
	tests := []struct {
		name      string
		failures  int
		wantErr   bool
		wantCalls int
	}{
		{"one conflict is retried", 1, false, 2},
		{"second conflict is returned", 2, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, cs := newFakeClient(nil, deployment("model-pool", new(int32(1)), 1, 1))
			addScaleReactors(cs)
			var calls int
			cs.PrependReactor("update", "deployments", conflictReactor("deployments", tt.failures, &calls))
			err := DeploymentActuator{C: c, Deployment: "model-pool"}.SetReplicas(context.Background(), 4)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Errorf("update calls = %d, want %d", calls, tt.wantCalls)
			}
			want := int32(4)
			if tt.wantErr {
				want = 1
			}
			if got := specReplicas(t, cs, "model-pool"); got != want {
				t.Errorf("spec.replicas = %d, want %d", got, want)
			}
		})
	}
}

func TestDeploymentStatus(t *testing.T) {
	tests := []struct {
		name string
		dep  *appsv1.Deployment
		want model.PoolObservation
	}{
		{"steady", deployment("d", new(int32(4)), 4, 4),
			model.PoolObservation{ReadyReplicas: 4, StartingReplicas: 0, DesiredReplicas: 4}},
		{"cold-starting", deployment("d", new(int32(7)), 7, 3),
			model.PoolObservation{ReadyReplicas: 3, StartingReplicas: 4, DesiredReplicas: 7}},
		{"pods not created yet", deployment("d", new(int32(9)), 5, 5),
			model.PoolObservation{ReadyReplicas: 5, StartingReplicas: 0, DesiredReplicas: 9}},
		{"scaling down", deployment("d", new(int32(2)), 6, 6),
			model.PoolObservation{ReadyReplicas: 6, StartingReplicas: 0, DesiredReplicas: 2}},
		{"inconsistent status never goes negative", deployment("d", new(int32(2)), 1, 2),
			model.PoolObservation{ReadyReplicas: 2, StartingReplicas: 0, DesiredReplicas: 2}},
		{"unset replicas defaults to 1", deployment("d", nil, 0, 0),
			model.PoolObservation{ReadyReplicas: 0, StartingReplicas: 0, DesiredReplicas: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _ := newFakeClient(nil, tt.dep)
			got, err := DeploymentActuator{C: c, Deployment: "d"}.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Status() = %+v, want %+v", got, tt.want)
			}
		})
	}
	c, _, _ := newFakeClient(nil)
	if _, err := (DeploymentActuator{C: c, Deployment: "absent"}).Status(context.Background()); !apierrors.IsNotFound(err) {
		t.Errorf("missing deployment: err = %v, want a wrapped not-found", err)
	}
}
