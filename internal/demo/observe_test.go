package demo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/model"
)

// An observing run starts no stack: it shows the observed gateway's
// per-tenant statistics (the cluster's a and c as the configured A and C),
// the observed controller's pool size and decision log, and stops on
// StopAB.
func TestObserveRun(t *testing.T) {
	const admin = "observe-test-admin-key-0001"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+admin {
			http.Error(w, "this endpoint needs the admin key", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(gateway.Stats{Endpoints: 3, Tenants: []model.TenantObservation{
			{Tenant: "a", ArrivalRequestsPerSec: 1, InFlight: 1, Completed: 4, HasSLOData: true, SLOAttainment: 1, P99TTFTSeconds: 0.2},
			{Tenant: "c", ArrivalRequestsPerSec: 5, QueueLength: 2, Shed: 3},
		}})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pool":{"readyReplicas":2,"startingReplicas":1,"desiredReplicas":3},"decision":{"poolReplicas":4}}`)
	})
	mux.HandleFunc("GET /decisions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.DecisionRecord{
			{Tenant: "c", Reason: "c is shed"}, {Tenant: control.PoolRecordTenant, Reason: "pool"},
		})
	})
	cluster := httptest.NewServer(mux)
	defer cluster.Close()

	srv, _ := newTestServer(t, testOptions(t))
	if _, err := srv.StartObserve(ObserveTarget{GatewayURL: cluster.URL}); err == nil {
		t.Fatal("observing without a controller URL was accepted")
	}
	if _, err := srv.StartObserve(ObserveTarget{GatewayURL: cluster.URL + "/", ControllerURL: cluster.URL, AdminKey: admin}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.StartAB(1); err != ErrBusy {
		t.Fatalf("StartAB while observing: %v, want ErrBusy", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var st State
	for {
		st = srv.State()
		if len(st.Stacks) == 2 && len(st.Stacks[1].Samples) > 0 && srv.Decisions(0, "").Total > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observed sample or decision: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !st.Live || st.LiveAddress != "" || st.Stacks[0].Status != StatusSkipped || st.Stacks[1].Status != StatusRunning ||
		!st.Stacks[1].Simulated {
		t.Fatalf("observing state %+v: want live without an address, baseline skipped, TenantScaler running, simulated", st)
	}
	smp := st.Stacks[1].Samples[len(st.Stacks[1].Samples)-1]
	if smp.Ready != 3 || smp.Desired != 4 || smp.Starting != 1 {
		t.Errorf("sample ready/desired/starting = %d/%d/%d, want 3 (gateway endpoints)/4 (applied pool)/1",
			smp.Ready, smp.Desired, smp.Starting)
	}
	a, okA := smp.Tenants["A"]
	c, okC := smp.Tenants["C"]
	if _, raw := smp.Tenants["a"]; raw || !okA || !okC || a.InFlight != 1 || a.Completed != 4 || c.Queued != 2 || c.Shed != 3 {
		t.Errorf("tenants %+v: want the cluster's a and c shown as A and C with their statistics", smp.Tenants)
	}
	d := srv.Decisions(0, "C")
	if len(d.Records) != 1 || d.Records[0].Reason != "c is shed" || !d.Live {
		t.Errorf("decisions of C %+v: want the observed record, live", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.StopAB(ctx); err != nil {
		t.Fatal(err)
	}
	if st := srv.State(); st.Phase != PhaseStopped || st.Stacks[1].Status != StatusStopped {
		t.Fatalf("after stop: %+v", st)
	}
	if d := srv.Decisions(0, ""); d.Live || d.Total != 2 {
		t.Errorf("decisions after stop %+v: want the last records kept, not live", d)
	}
}

// Without the admin key the observed gateway answers 401: the samples keep
// the controller's pool size and have no per-tenant statistics.
func TestObserveWithoutAdminKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this endpoint needs the admin key", http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pool":{"readyReplicas":2,"startingReplicas":0,"desiredReplicas":2}}`)
	})
	mux.HandleFunc("GET /decisions", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) })
	cluster := httptest.NewServer(mux)
	defer cluster.Close()

	srv, _ := newTestServer(t, testOptions(t))
	if _, err := srv.StartObserve(ObserveTarget{GatewayURL: cluster.URL, ControllerURL: cluster.URL}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := srv.State()
		if n := len(st.Stacks[1].Samples); n > 0 {
			smp := st.Stacks[1].Samples[n-1]
			if smp.Ready != 2 || smp.Desired != 2 || smp.Tenants != nil {
				t.Fatalf("sample %+v: want the controller's 2/2 and no tenant statistics", smp)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observed sample: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
