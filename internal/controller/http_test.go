package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"tenantscaler/internal/control"
	"tenantscaler/internal/model"
)

// get fetches path from srv and returns the status code and body.
func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

func TestHandlerEndpoints(t *testing.T) {
	rig := newRig(t, testConfig(t))
	srv := httptest.NewServer(rig.r.Handler())
	defer srv.Close()

	// Before the first cycle.
	code, body := get(t, srv, "/status")
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("GET /status before any cycle: %d %v %s", code, err, body)
	}
	if st["cycle"] != float64(0) || st["decision"] != nil {
		t.Errorf("status before any cycle = %s", body)
	}
	if code, body := get(t, srv, "/decisions"); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("GET /decisions before any cycle = %d %s, want 200 []", code, body)
	}

	rig.r.RunOnce(context.Background())
	rig.pub.err = errors.New("gateway down")
	rig.r.RunOnce(context.Background())

	t.Run("healthz", func(t *testing.T) {
		if code, body := get(t, srv, "/healthz"); code != http.StatusOK || !strings.Contains(body, `"ok"`) {
			t.Errorf("GET /healthz = %d %s", code, body)
		}
	})
	t.Run("status", func(t *testing.T) {
		code, body := get(t, srv, "/status")
		var st map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
			t.Fatalf("GET /status: %d %v", code, err)
		}
		for _, key := range []string{"cycle", "cycleSeconds", "time", "failStatic", "pool", "observation", "decision", "errors"} {
			if _, ok := st[key]; !ok {
				t.Errorf("status has no %q: %s", key, body)
			}
		}
		var typed Status
		if err := json.Unmarshal([]byte(body), &typed); err != nil {
			t.Fatal(err)
		}
		if typed.Cycle != 2 || typed.CycleSeconds != 15 || typed.Decision == nil || typed.Decision.Policy == nil ||
			typed.Errors[StagePublish].Cycle != 2 || !strings.Contains(typed.Errors[StagePublish].Error, "gateway down") {
			t.Errorf("status = %s", body)
		}
	})
	t.Run("decisions", func(t *testing.T) {
		code, body := get(t, srv, "/decisions?limit=2&tenant=A")
		var recs []model.DecisionRecord
		if err := json.Unmarshal([]byte(body), &recs); code != http.StatusOK || err != nil {
			t.Fatalf("GET /decisions: %d %v %s", code, err, body)
		}
		if len(recs) != 2 || recs[0].Tenant != "A" || recs[0].Cycle != 2 || recs[1].Cycle != 1 {
			t.Errorf("decisions = %+v, want A's records of cycles 2 then 1", recs)
		}
		if recs[0].Reason == "" || len(recs[0].Steps) == 0 {
			t.Errorf("record %+v has no explanation", recs[0])
		}
		if code, body := get(t, srv, "/decisions?limit=0"); code != http.StatusOK || strings.Count(body, `"cycle"`) != 8 {
			t.Errorf("GET /decisions?limit=0 = %d, want all 8 records: %s", code, body)
		}
		for _, bad := range []string{"-1", "x"} {
			if code, _ := get(t, srv, "/decisions?limit="+bad); code != http.StatusBadRequest {
				t.Errorf("limit=%s: status %d, want 400", bad, code)
			}
		}
	})
	t.Run("metrics", func(t *testing.T) {
		code, body := get(t, srv, "/metrics")
		if code != http.StatusOK {
			t.Fatalf("GET /metrics: %d", code)
		}
		for _, want := range []string{
			"tenantscaler_controller_cycles_total 2",
			"tenantscaler_controller_failstatic_total 0",
			"tenantscaler_controller_desired_replicas 8",
			"tenantscaler_controller_pool_replicas ",
			`tenantscaler_controller_tenant_grant{tenant="A"} `,
			`tenantscaler_controller_tenant_grant{tenant="C"} `,
			`tenantscaler_controller_errors_total{stage="publish"} 1`,
			`tenantscaler_controller_errors_total{stage="metrics"} 0`,
			"go_goroutines",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics has no %q", want)
			}
		}
		if strings.Contains(body, `tenant="(pool)"`) {
			t.Error("/metrics exports the pool record as a tenant")
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		resp, err := srv.Client().Post(srv.URL+"/status", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /status = %d, want 405", resp.StatusCode)
		}
	})
}

func TestMetricsDropTenantsThatLeave(t *testing.T) {
	rig := newRig(t, testConfig(t))
	srv := httptest.NewServer(rig.r.Handler())
	defer srv.Close()
	rig.r.RunOnce(context.Background())
	specs := testConfig(t).Tenants[:2] // C leaves
	if err := rig.r.Core.SetTenants(specs); err != nil {
		t.Fatal(err)
	}
	rig.r.RunOnce(context.Background())
	_, body := get(t, srv, "/metrics")
	if strings.Contains(body, `tenant="C"`) || !strings.Contains(body, `tenant="B"`) {
		t.Errorf("grant series after C left:\n%s", grepLines(body, "tenant_grant"))
	}
}

// A fail-static cycle changes nothing, so the replica and grant gauges keep
// the last applied decision, also with gateway coupling off (no policy).
func TestMetricsHoldDuringFailStatic(t *testing.T) {
	cfg := testConfig(t)
	cfg.Controller.DisableGatewayCoupling = true
	rig := newRig(t, cfg)
	srv := httptest.NewServer(rig.r.Handler())
	defer srv.Close()
	d := rig.r.RunOnce(context.Background())
	_, before := get(t, srv, "/metrics")
	rig.src.set(model.Observation{}, errBoom)
	rig.r.RunOnce(context.Background())
	_, after := get(t, srv, "/metrics")

	for _, name := range []string{"tenant_grant", "controller_desired_replicas ", "controller_pool_replicas "} {
		if b, a := grepLines(before, name), grepLines(after, name); a != b || a == "" {
			t.Errorf("%s changed in a fail-static cycle:\nbefore %s\nafter  %s", name, b, a)
		}
	}
	if !strings.Contains(after, `tenantscaler_controller_tenant_grant{tenant="A"} `+strconv.Itoa(grantOf(d, "A"))) {
		t.Errorf("A's grant gauge is not the last applied grant %d:\n%s", grantOf(d, "A"), grepLines(after, "tenant_grant"))
	}
	if !strings.Contains(after, "tenantscaler_controller_failstatic_total 1") || !strings.Contains(after, "tenantscaler_controller_cycles_total 2") {
		t.Errorf("counters after one fail-static cycle:\n%s", grepLines(after, "_total"))
	}
}

func grantOf(d control.Decision, tenant string) int {
	for _, r := range d.Records {
		if r.Tenant == tenant {
			return r.Final.Grant
		}
	}
	return -1
}

func TestDecisionsWithoutLog(t *testing.T) {
	rig := newRig(t, testConfig(t))
	rig.r.Log = nil
	rig.r.RunOnce(context.Background())
	srv := httptest.NewServer(rig.r.Handler())
	defer srv.Close()
	if code, body := get(t, srv, "/decisions"); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("GET /decisions without a log = %d %s, want 200 []", code, body)
	}
}

func grepLines(s, substr string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
