package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// ep is one endpoint; ready nil leaves the condition unset.
type ep struct {
	addr  string
	ready *bool
}

func endpointSlice(name, namespace, service string, eps ...ep) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, e := range eps {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{e.addr},
			Conditions: discoveryv1.EndpointConditions{Ready: e.ready},
		})
	}
	return s
}

func TestEndpointSliceDiscovery(t *testing.T) {
	yes, no := new(true), new(false)
	c, _, _ := newFakeClient(nil,
		endpointSlice("model-pool-abc", testNamespace, "model-pool",
			ep{"10.0.0.2", yes}, ep{"10.0.0.3", no}, ep{"10.0.0.1", nil}, ep{"fd00::5", yes}),
		endpointSlice("model-pool-def", testNamespace, "model-pool",
			ep{"10.0.0.2", yes}, ep{"10.0.0.4", yes}),
		endpointSlice("gateway-xyz", testNamespace, "gateway", ep{"10.0.0.9", yes}),
		endpointSlice("model-pool-other", "elsewhere", "model-pool", ep{"10.0.0.8", yes}),
	)
	d := NewEndpointSliceDiscovery(c, "model-pool", 8000)
	if got := d.Endpoints(); len(got) != 0 {
		t.Fatalf("before Refresh: Endpoints() = %v, want none", got)
	}
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	want := []string{
		"http://10.0.0.1:8000", // ready unset counts as ready
		"http://10.0.0.2:8000", // listed in two slices, reported once
		"http://10.0.0.4:8000",
		"http://[fd00::5]:8000",
	}
	got := d.Endpoints()
	if !slices.Equal(got, want) {
		t.Fatalf("Endpoints() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if d.Endpoints()[0] != want[0] {
		t.Error("Endpoints() must return a copy")
	}
}

func TestEndpointSliceDiscoveryKeepsLastSetOnError(t *testing.T) {
	c, _, cs := newFakeClient(nil,
		endpointSlice("model-pool-abc", testNamespace, "model-pool", ep{"10.0.0.1", new(true)}))
	d := NewEndpointSliceDiscovery(c, "model-pool", 8000)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("list", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	err := d.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "model-pool") {
		t.Fatalf("err = %v, want an error naming the service", err)
	}
	if got := d.Endpoints(); !slices.Equal(got, []string{"http://10.0.0.1:8000"}) {
		t.Errorf("after a failed Refresh: Endpoints() = %v, want the previous set", got)
	}
}

func TestEndpointSliceDiscoveryRejectsBadPort(t *testing.T) {
	c, _, _ := newFakeClient(nil)
	for _, port := range []int{0, -1, 65536} {
		err := NewEndpointSliceDiscovery(c, "model-pool", port).Refresh(context.Background())
		if err == nil || !strings.Contains(err.Error(), "port") {
			t.Errorf("port %d: err = %v, want a port error", port, err)
		}
	}
}

func TestEndpointSliceDiscoveryConcurrentUse(t *testing.T) {
	c, _, _ := newFakeClient(nil,
		endpointSlice("model-pool-abc", testNamespace, "model-pool", ep{"10.0.0.1", new(true)}))
	d := NewEndpointSliceDiscovery(c, "model-pool", 8000)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				if err := d.Refresh(context.Background()); err != nil {
					t.Error(err)
					return
				}
				if n := len(d.Endpoints()); n > 1 {
					t.Errorf("Endpoints() has %d entries, want at most 1", n)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := d.Endpoints(); !slices.Equal(got, []string{"http://10.0.0.1:8000"}) {
		t.Errorf("Endpoints() = %v", got)
	}
}
