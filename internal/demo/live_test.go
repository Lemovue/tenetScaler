package demo

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"tenantscaler/internal/gateway"
)

// A live run serves teammates' traffic on a public, keyed address, is
// marked live in the state, and stops on StopAB.
func TestLiveRun(t *testing.T) {
	srv, _ := newTestServer(t, testOptions(t))
	keys, err := gateway.ParseTenantKeys("B:standard=live-test-key-of-B-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.StartLive("127.0.0.1:0", nil, false); err == nil {
		t.Fatal("a live run without keys was accepted")
	}
	if _, err := srv.StartLive("127.0.0.1:0", keys, false); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.StartAB(1); err != ErrBusy {
		t.Fatalf("StartAB during a live run: %v, want ErrBusy", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var st State
	for {
		st = srv.State()
		if st.LiveAddress != "" && len(st.Stacks) == 2 && st.Stacks[1].Status == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live stack never ran: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !st.Live || st.Stacks[0].Status != StatusSkipped || st.Progress != 0 {
		t.Fatalf("live state %+v: want live, baseline skipped, no progress", st)
	}
	url := "http://" + st.LiveAddress + "/v1/completions"
	body := []byte(`{"prompt":"w w","max_tokens":2,"stream":false}`)
	status, header, got := do(t, http.MethodPost, url, body, map[string]string{"Authorization": "Bearer live-test-key-of-B-0001"})
	if status != http.StatusOK || header.Get(gateway.TenantHeaderUsed) != "B" {
		t.Fatalf("keyed live request: %d as %q: %s", status, header.Get(gateway.TenantHeaderUsed), got)
	}
	if status, _, _ := do(t, http.MethodPost, url, body, nil); status != http.StatusUnauthorized {
		t.Fatalf("live request without a key: %d, want 401", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.StopAB(ctx); err != nil {
		t.Fatal(err)
	}
	if st := srv.State(); st.Phase != PhaseStopped || st.Stacks[1].Status != StatusStopped {
		t.Fatalf("after stop: %+v", st)
	}
	if resp, err := http.Post(url, "application/json", bytes.NewReader(body)); err == nil {
		resp.Body.Close()
		t.Fatalf("the public address still answers %d after stop", resp.StatusCode)
	}
}

// A side-by-side live run starts both stacks: the client gets
// TenantScaler's answer, the baseline a copy of the request, so both panels
// show the same live traffic; keys are still required, and StopAB stops
// both.
func TestLiveRunSideBySide(t *testing.T) {
	srv, _ := newTestServer(t, testOptions(t))
	keys, err := gateway.ParseTenantKeys("B:standard=live-test-key-of-B-0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.StartLive("127.0.0.1:0", keys, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var st State
	for {
		st = srv.State()
		if st.LiveAddress != "" && st.Stacks[0].Status == StatusRunning && st.Stacks[1].Status == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("side-by-side stacks never ran: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	url := "http://" + st.LiveAddress + "/v1/completions"
	body := []byte(`{"prompt":"w w","max_tokens":2,"stream":false}`)
	auth := map[string]string{"Authorization": "Bearer live-test-key-of-B-0001"}
	for range 3 {
		status, header, got := do(t, http.MethodPost, url, body, auth)
		if status != http.StatusOK || header.Get(gateway.TenantHeaderUsed) != "B" {
			t.Fatalf("keyed side-by-side request: %d as %q: %s", status, header.Get(gateway.TenantHeaderUsed), got)
		}
	}
	if status, _, _ := do(t, http.MethodPost, url, body, nil); status != http.StatusUnauthorized {
		t.Fatalf("side-by-side request without a key: %d, want 401", status)
	}
	if status, _, _ := do(t, http.MethodGet, "http://"+st.LiveAddress+"/stats", nil, nil); status != http.StatusNotFound {
		t.Fatalf("GET /stats on the public address: %d, want 404 (only completions and /healthz)", status)
	}
	// Both stacks measured B's requests: the baseline got the copies.
	for {
		st = srv.State()
		if completedB(st.Stacks[0]) >= 3 && completedB(st.Stacks[1]) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B completed %d on the baseline and %d on TenantScaler, want >= 3 each",
				completedB(st.Stacks[0]), completedB(st.Stacks[1]))
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.StopAB(ctx); err != nil {
		t.Fatal(err)
	}
	if st := srv.State(); st.Phase != PhaseStopped || st.Stacks[0].Status != StatusStopped || st.Stacks[1].Status != StatusStopped {
		t.Fatalf("after stop: %+v", st)
	}
}

// completedB is tenant B's completions in a stack's latest live sample.
func completedB(s StackState) int {
	if len(s.Samples) == 0 {
		return 0
	}
	return s.Samples[len(s.Samples)-1].Tenants["B"].Completed
}
