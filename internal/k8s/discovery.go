package k8s

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// EndpointSliceDiscovery finds the ready replicas behind a Service by
// reading its EndpointSlices, so the gateway dispatches straight to pods
// instead of through the Service's load balancing. Call Refresh periodically;
// Endpoints is safe to call from any goroutine.
type EndpointSliceDiscovery struct {
	c       *Client
	service string
	port    int

	mu        sync.RWMutex
	endpoints []string
}

// NewEndpointSliceDiscovery watches the Service named service in the
// client's namespace; port is the replica container port used in the
// returned URLs. The endpoint list is empty until the first Refresh.
func NewEndpointSliceDiscovery(c *Client, service string, port int) *EndpointSliceDiscovery {
	return &EndpointSliceDiscovery{c: c, service: service, port: port}
}

// Refresh lists the Service's EndpointSlices and replaces the endpoint set
// with the addresses of ready endpoints, as "http://ip:port", deduplicated
// and sorted. An endpoint whose ready condition is unset counts as ready,
// as the EndpointSlice API specifies. On error the previous set is kept.
func (d *EndpointSliceDiscovery) Refresh(ctx context.Context) error {
	if d.port < 1 || d.port > 65535 {
		return fmt.Errorf("discover endpoints of service %s/%s: port must be in [1, 65535], got %d", d.c.Namespace, d.service, d.port)
	}
	selector := labels.Set{discoveryv1.LabelServiceName: d.service}.String()
	list, err := d.c.Kube.DiscoveryV1().EndpointSlices(d.c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("discover endpoints of service %s/%s: %w", d.c.Namespace, d.service, err)
	}
	port := strconv.Itoa(d.port)
	var urls []string
	for _, slice := range list.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				urls = append(urls, "http://"+net.JoinHostPort(addr, port))
			}
		}
	}
	slices.Sort(urls)
	urls = slices.Compact(urls)

	d.mu.Lock()
	d.endpoints = urls
	d.mu.Unlock()
	return nil
}

// Endpoints returns a copy of the ready endpoint URLs from the last
// successful Refresh.
func (d *EndpointSliceDiscovery) Endpoints() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Clone(d.endpoints)
}
