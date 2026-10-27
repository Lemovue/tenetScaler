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
	if _, err := srv.StartLive("127.0.0.1:0", nil); err == nil {
		t.Fatal("a live run without keys was accepted")
	}
	if _, err := srv.StartLive("127.0.0.1:0", keys); err != nil {
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
