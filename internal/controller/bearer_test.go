package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tenantscaler/internal/policy"
)

// The admin key reaches the gateway on both controller calls: GET /stats
// (GatewaySource) and POST /policy (HTTPPublisher).
func TestBearerClientSendsAdminKey(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/policy" {
			_, _ = w.Write([]byte(`{"applied":true,"version":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"tenants":[]}`))
	}))
	defer srv.Close()
	hc := BearerClient("admin-key-89abcdef01")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (GatewaySource{URLs: []string{srv.URL}, HTTP: hc}).Observe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (HTTPPublisher{URLs: []string{srv.URL}, HTTP: hc}).Publish(ctx, policy.Policy{Version: 1}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"GET /stats", "POST /policy"} {
		if got := seen[k]; got != "Bearer admin-key-89abcdef01" {
			t.Errorf("%s: Authorization %q", k, got)
		}
	}
}
