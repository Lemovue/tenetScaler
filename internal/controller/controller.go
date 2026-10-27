// Package controller is the live runner around control.Core: every cycle it
// reads per-tenant metrics (from the gateways' /stats or from Prometheus),
// reads the pool, steps the pure decision core, scales the pool, pushes the
// versioned policy to the gateways and keeps an explained decision log. When
// the metrics or the pool cannot be read it fails static: nothing is scaled
// and the last policy is repeated.
//
// It works the same against simulated replicas and real llama-server
// replicas: it only talks to the pool manager (or a Kubernetes Deployment)
// and to the gateways, never to replicas directly.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/pool"
)

// defaultHTTP is used when a source or publisher has no HTTP client.
var defaultHTTP = &http.Client{Timeout: 5 * time.Second}

// BearerClient returns a client like the default one that sends
// "Authorization: Bearer <token>" with every request: the gateway's admin
// key for GET /stats and POST /policy (gateway Config.AdminKey).
func BearerClient(token string) *http.Client {
	return &http.Client{Timeout: defaultHTTP.Timeout, Transport: bearerTransport{token: token, base: http.DefaultTransport}}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// maxResponseBytes bounds every JSON response the controller decodes.
const maxResponseBytes = 8 << 20

// MetricsSource reads one observation of every tenant. Observe need not set
// Time, OK or Pool: the Runner stamps the time, marks a nil-error result OK
// and fills the pool from the Actuator.
type MetricsSource interface {
	Observe(ctx context.Context) (model.Observation, error)
}

// Actuator reads and sets the size of the shared replica pool.
type Actuator interface {
	SetReplicas(ctx context.Context, n int) error
	Status(ctx context.Context) (model.PoolObservation, error)
}

// Publisher delivers a policy to the gateways.
type Publisher interface {
	Publish(ctx context.Context, p policy.Policy) error
}

// PoolActuator scales the local pool manager (local mode) through its HTTP
// API. It works for both simulated and real llama-server pools.
type PoolActuator struct {
	Client *pool.Client
}

// SetReplicas sets the pool's desired replica count.
func (a PoolActuator) SetReplicas(ctx context.Context, n int) error {
	if a.Client == nil {
		return errors.New("pool actuator: no pool client")
	}
	return a.Client.Scale(ctx, n)
}

// Status reads the pool's ready, starting and desired replica counts.
func (a PoolActuator) Status(ctx context.Context) (model.PoolObservation, error) {
	if a.Client == nil {
		return model.PoolObservation{}, errors.New("pool actuator: no pool client")
	}
	st, err := a.Client.Status(ctx)
	if err != nil {
		return model.PoolObservation{}, err
	}
	return model.PoolObservation{
		ReadyReplicas:    st.Ready,
		StartingReplicas: st.Starting,
		DesiredReplicas:  st.Desired,
	}, nil
}

// doJSON sends one request with an optional JSON body and, when out is not
// nil, decodes a 2xx JSON response into it. A non-2xx answer is an error
// that quotes the start of the response body.
func doJSON(ctx context.Context, hc *http.Client, method, url string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, url, err)
	}
	return nil
}
