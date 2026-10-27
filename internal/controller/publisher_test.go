package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"tenantscaler/internal/config"
	"tenantscaler/internal/policy"
)

// policyGateway emulates the gateway's policy endpoints as DESIGN section 13
// specifies them: POST /policy applies a policy unless it is older than the
// one in force, which is answered 200 with applied false; GET /policy
// returns the policy in force.
type policyGateway struct {
	mu      sync.Mutex
	version int64
	applied []policy.Policy
}

func newPolicyGateway(t *testing.T, version int64) (*policyGateway, *httptest.Server) {
	t.Helper()
	g := &policyGateway{version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /policy", func(w http.ResponseWriter, r *http.Request) {
		var p policy.Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Validate() != nil {
			http.Error(w, `{"error":"bad policy"}`, http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		applied := p.Version >= g.version
		if applied {
			g.version = p.Version
			g.applied = append(g.applied, p)
		}
		writeJSON(w, http.StatusOK, map[string]any{"applied": applied, "version": g.version, "latencyMs": 0.5})
	})
	mux.HandleFunc("GET /policy", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		writeJSON(w, http.StatusOK, policy.Policy{Version: g.version, Tenants: map[string]policy.TenantPolicy{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

// appliedVersions returns the versions the gateway applied, in order.
func (g *policyGateway) appliedVersions() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]int64, len(g.applied))
	for i, p := range g.applied {
		out[i] = p.Version
	}
	return out
}

func TestHTTPPublisher(t *testing.T) {
	var mu sync.Mutex
	var got []policy.Policy
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/policy" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var p policy.Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"applied":true}`)
	})
	g1 := httptest.NewServer(okHandler)
	defer g1.Close()
	g2 := httptest.NewServer(okHandler)
	defer g2.Close()
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid policy"}`, http.StatusUnprocessableEntity)
	}))
	defer rejecting.Close()

	pol := policy.Policy{Version: 3, Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 5, RetryAfterSeconds: 2, CriticalSlots: 24},
		"B": {Weight: policy.MinWeight, Shed: true, RetryAfterSeconds: 2},
	}}
	if err := (HTTPPublisher{URLs: []string{g1.URL + "/", g2.URL}}).Publish(context.Background(), pol); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], pol) || !reflect.DeepEqual(got[1], pol) {
		t.Errorf("gateways received %+v, want the policy twice", got)
	}

	err := HTTPPublisher{URLs: []string{g1.URL, rejecting.URL}}.Publish(context.Background(), pol)
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "invalid policy") ||
		!strings.Contains(err.Error(), "v3") {
		t.Errorf("Publish to a rejecting gateway = %v, want an error naming the 422, its body and the version", err)
	}
	if len(got) != 3 {
		t.Errorf("the healthy gateway got %d policies, want 3: one failure must not block the others", len(got))
	}
	if err := (HTTPPublisher{}).Publish(context.Background(), pol); err == nil {
		t.Error("Publish without URLs succeeded")
	}
}

// A gateway that keeps a newer policy answers 200 with applied false; that
// must not count as a success.
func TestHTTPPublisherReportsStaleVersion(t *testing.T) {
	newer, newerSrv := newPolicyGateway(t, 50)
	fresh, freshSrv := newPolicyGateway(t, 0)
	pol := policy.Policy{Version: 2, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 1}}}

	err := HTTPPublisher{URLs: []string{freshSrv.URL, newerSrv.URL}}.Publish(context.Background(), pol)
	var stale *StaleVersionError
	if !errors.As(err, &stale) || stale.Version != 2 || stale.GatewayVersion != 50 || stale.URL != newerSrv.URL+"/policy" {
		t.Fatalf("Publish = %v, want a StaleVersionError for v2 against v50 at %s", err, newerSrv.URL)
	}
	if got := staleGatewayVersion(err); got != 50 {
		t.Errorf("staleGatewayVersion = %d, want 50", got)
	}
	if v := fresh.appliedVersions(); !reflect.DeepEqual(v, []int64{2}) {
		t.Errorf("fresh gateway applied %v, want [2]", v)
	}
	if v := newer.appliedVersions(); len(v) != 0 {
		t.Errorf("newer gateway applied %v, want nothing", v)
	}
}

func TestPolicyReplyCheck(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name  string
		reply policyReply
		want  string // "" = no error
	}{
		{"applied", policyReply{Applied: &yes, Version: 4}, ""},
		{"not a gateway answer", policyReply{}, `no "applied" field`},
		{"newer version in force", policyReply{Applied: &no, Version: 9}, "keeps the newer v9"},
		{"not applied otherwise", policyReply{Applied: &no, Version: 4}, "not applied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.reply.check("http://gw/policy", 4)
			if tt.want == "" {
				if err != nil {
					t.Errorf("check = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("check = %v, want an error containing %q", err, tt.want)
			}
		})
	}
	// A 2xx answer that is not JSON is an error too.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>")
	}))
	defer html.Close()
	if err := (HTTPPublisher{URLs: []string{html.URL}}).Publish(context.Background(), policy.Policy{Version: 1}); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Errorf("Publish to a non-gateway = %v, want a decode error", err)
	}
}

func TestStaleGatewayVersionWalksTheErrorTree(t *testing.T) {
	err := errors.Join(
		fmt.Errorf("wrapped: %w", &StaleVersionError{GatewayVersion: 7}),
		errBoom,
		errors.Join(&StaleVersionError{GatewayVersion: 12}),
	)
	if got := staleGatewayVersion(err); got != 12 {
		t.Errorf("staleGatewayVersion = %d, want the newest, 12", got)
	}
	for _, e := range []error{nil, errBoom} {
		if got := staleGatewayVersion(e); got != 0 {
			t.Errorf("staleGatewayVersion(%v) = %d, want 0", e, got)
		}
	}
}

func TestHTTPPublisherPolicyVersion(t *testing.T) {
	_, g7 := newPolicyGateway(t, 7)
	_, g3 := newPolicyGateway(t, 3)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	v, err := HTTPPublisher{URLs: []string{g3.URL, g7.URL + "/"}}.PolicyVersion(context.Background())
	if err != nil || v != 7 {
		t.Errorf("PolicyVersion = %d, %v; want 7", v, err)
	}
	v, err = HTTPPublisher{URLs: []string{g3.URL, down.URL}}.PolicyVersion(context.Background())
	if err == nil || v != 3 {
		t.Errorf("PolicyVersion with a gateway down = %d, %v; want 3 and an error", v, err)
	}
	if _, err := (HTTPPublisher{}).PolicyVersion(context.Background()); err == nil {
		t.Error("PolicyVersion without URLs succeeded")
	}
}

func TestNewPublisher(t *testing.T) {
	cfg := config.Default()
	p, err := NewPublisher(cfg, []string{"http://gw:8080"})
	if err != nil || p == nil || !reflect.DeepEqual(p.URLs, []string{"http://gw:8080"}) {
		t.Errorf("NewPublisher = %+v, %v; want a publisher for the gateway", p, err)
	}
	if _, err := NewPublisher(cfg, nil); err == nil {
		t.Error("NewPublisher with coupling on and no gateway succeeded")
	}
	cfg.Controller.DisableGatewayCoupling = true
	if p, err := NewPublisher(cfg, nil); p != nil || err != nil {
		t.Errorf("NewPublisher with coupling off = %+v, %v; want nil, nil", p, err)
	}
}
