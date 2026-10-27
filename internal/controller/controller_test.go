package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
)

// fakePoolAPI emulates the pool manager's GET /pool and PUT /pool/scale.
type fakePoolAPI struct {
	mu     sync.Mutex
	status pool.Status
	scales []int
}

func (f *fakePoolAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pool", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.status)
	})
	mux.HandleFunc("PUT /pool/scale", func(w http.ResponseWriter, r *http.Request) {
		var req pool.ScaleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Replicas == nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.scales = append(f.scales, *req.Replicas)
		f.status.Desired = *req.Replicas
		_ = json.NewEncoder(w).Encode(f.status)
	})
	return mux
}

func TestPoolActuator(t *testing.T) {
	api := &fakePoolAPI{status: pool.Status{Desired: 3, Ready: 2, Starting: 1, Draining: 1}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	a := PoolActuator{Client: &pool.Client{BaseURL: srv.URL}}

	got, err := a.Status(context.Background())
	if want := (model.PoolObservation{ReadyReplicas: 2, StartingReplicas: 1, DesiredReplicas: 3}); err != nil || got != want {
		t.Fatalf("Status = %+v, %v; want %+v", got, err, want)
	}
	if err := a.SetReplicas(context.Background(), 5); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if !reflect.DeepEqual(api.scales, []int{5}) {
		t.Errorf("scale requests %v, want [5]", api.scales)
	}

	srv.Close()
	if _, err := a.Status(context.Background()); err == nil {
		t.Error("Status against a stopped pool succeeded")
	}
	if err := (PoolActuator{}).SetReplicas(context.Background(), 1); err == nil {
		t.Error("SetReplicas without a client succeeded")
	}
	if _, err := (PoolActuator{}).Status(context.Background()); err == nil {
		t.Error("Status without a client succeeded")
	}
}
