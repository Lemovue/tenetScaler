package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/pool"
)

// fakeGateway serves /stats with fixed tenants and records POST /policy.
type fakeGateway struct {
	mu       sync.Mutex
	tenants  []model.TenantObservation
	policies []policy.Policy
}

func (g *fakeGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"mode": "fair", "tenants": g.tenants})
	})
	mux.HandleFunc("POST /policy", func(w http.ResponseWriter, r *http.Request) {
		var p policy.Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Validate() != nil {
			http.Error(w, "bad policy", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.policies = append(g.policies, p)
		g.mu.Unlock()
		_, _ = w.Write([]byte(`{"applied":true}`))
	})
	return mux
}

// TestRunnerDrivesSimulatedPool runs the controller against a real pool
// manager (SIMULATED replicas) over its HTTP API and a fake gateway.
func TestRunnerDrivesSimulatedPool(t *testing.T) {
	params := engine.DefaultParams()
	params.StartupDuration = 0
	p := pool.New(pool.Config{Name: "it", Params: params, MaxReplicas: 10, Seed: 1})
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("pool Close: %v", err)
		}
	}()
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	poolSrv := httptest.NewServer(p.Handler())
	defer poolSrv.Close()
	gw := &fakeGateway{tenants: loadObs().Tenants}
	gwSrv := httptest.NewServer(gw.handler())
	defer gwSrv.Close()

	core, err := control.NewCore(testConfig(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	log, err := NewDecisionLog(0, "")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Core:      core,
		Source:    GatewaySource{URLs: []string{gwSrv.URL}, Window: 15 * time.Second},
		Actuator:  PoolActuator{Client: &pool.Client{BaseURL: poolSrv.URL}},
		Publisher: HTTPPublisher{URLs: []string{gwSrv.URL}},
		Log:       log, Cycle: 15 * time.Second, Logger: discardLogger(),
	}
	d := r.RunOnce(context.Background())
	if d.FailStatic || len(r.Status().Errors) != 0 {
		t.Fatalf("cycle failed: failStatic %v, errors %+v", d.FailStatic, r.Status().Errors)
	}
	// The pool had 2 replicas; one cycle may add at most 4.
	if d.PoolReplicas != 6 {
		t.Errorf("pool replicas %d, want 6 (2 + max step 4)", d.PoolReplicas)
	}
	if got := p.Status().Desired; got != d.PoolReplicas {
		t.Errorf("pool desired %d, want the decision's %d", got, d.PoolReplicas)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.policies) != 1 || gw.policies[0].Version != 1 || len(gw.policies[0].Tenants) != 3 {
		t.Errorf("gateway received %+v, want policy v1 for 3 tenants", gw.policies)
	}
}
