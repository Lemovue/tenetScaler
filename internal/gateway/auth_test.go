package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"tenantscaler/internal/model"
)

const (
	keyA     = "key-of-tenant-A-0123"
	keyC     = "key-of-tenant-C-4567"
	adminKey = "admin-key-89abcdef01"
)

// authCall sends method path with an optional bearer token, tenant header
// and priority header, and returns the status, body and tenant used.
func authCall(t *testing.T, base, method, path, token, tenant, prio, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant != "" {
		req.Header.Set(TenantHeaderAlias, tenant)
	}
	if prio != "" {
		req.Header.Set(PriorityHeader, prio)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header.Get(TenantHeaderUsed)
}

func TestParseTenantKeys(t *testing.T) {
	got, err := ParseTenantKeys(" A = " + keyA + " , C:sheddable=" + keyC)
	if err != nil || len(got) != 2 {
		t.Fatalf("ParseTenantKeys = %v, %v", got, err)
	}
	if got[keyA] != (KeyGrant{Tenant: "A", MaxPriority: model.PriorityCritical}) ||
		got[keyC] != (KeyGrant{Tenant: "C", MaxPriority: model.PrioritySheddable}) {
		t.Fatalf("grants %+v: want A uncapped, C capped at sheddable", got)
	}
	if got, err := ParseTenantKeys("  "); got != nil || err != nil {
		t.Fatalf("empty: %v, %v; want no keys", got, err)
	}
	for _, bad := range []string{"A", "A=", "=" + keyA, "A=short", "A=" + keyA + ",C=" + keyA, "A:urgent=" + keyA} {
		if _, err := ParseTenantKeys(bad); err == nil {
			t.Errorf("ParseTenantKeys(%q) accepted", bad)
		}
	}
}

func TestKeysValidated(t *testing.T) {
	disc := StaticDiscovery{"http://127.0.0.1:1"}
	grant := func(tenant string) KeyGrant { return KeyGrant{Tenant: tenant, MaxPriority: model.PriorityCritical} }
	bad := map[string]Config{
		"unknown tenant": {TenantKeys: map[string]KeyGrant{keyA: grant("Z")}},
		"short key":      {TenantKeys: map[string]KeyGrant{"short": grant("A")}},
		"short admin":    {AdminKey: "short"},
		"admin reused":   {TenantKeys: map[string]KeyGrant{keyA: grant("A")}, AdminKey: keyA},
	}
	for name, cfg := range bad {
		cfg.SlotsPerReplica, cfg.Discovery, cfg.Tenants = 1, disc, testTenants("A", "C")
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted the keys", name)
		}
	}
}

// With tenant keys the API key decides the tenant (no key or an unknown one
// is 401; a tenant header is ignored: the key wins) and caps the priority
// (a header may only lower it). The admin endpoints need the admin key;
// /healthz stays open.
func TestTenantAndAdminKeys(t *testing.T) {
	up := newFakeUpstream(t, nil)
	keys, err := ParseTenantKeys("A=" + keyA + ",C:standard=" + keyC)
	if err != nil {
		t.Fatal(err)
	}
	g, base := startGateway(t, Config{
		Tenants: testTenants("A", "C"), Discovery: StaticDiscovery{up.URL()}, TenantKeys: keys, AdminKey: adminKey,
	})
	body := completionBody("w", 2, false)
	cases := []struct {
		name, token, tenant, prio string
		want                      int
		served                    string
	}{
		{"no key", "", "A", "", http.StatusUnauthorized, ""},
		{"unknown key", "not-a-key-0123456789", "", "", http.StatusUnauthorized, ""},
		{"admin key is not a tenant key", adminKey, "", "", http.StatusUnauthorized, ""},
		{"key A, no header", keyA, "", "", http.StatusOK, "A"},
		{"key A, header A", keyA, "A", "", http.StatusOK, "A"},
		{"key A, header C: the key wins", keyA, "C", "", http.StatusOK, "A"},
		{"key C asks critical, capped at standard", keyC, "", "critical", http.StatusOK, "C"},
		{"key C lowers to sheddable", keyC, "", "sheddable", http.StatusOK, "C"},
	}
	for _, c := range cases {
		status, got, served := authCall(t, base, http.MethodPost, "/v1/completions", c.token, c.tenant, c.prio, body)
		if status != c.want || served != c.served {
			t.Errorf("%s: status %d served as %q, want %d as %q: %s", c.name, status, served, c.want, c.served, got)
		}
	}
	for _, ts := range g.Stats(0).Tenants {
		if want := map[string]int{"A": 3, "C": 2}[ts.Tenant]; ts.Completed != want {
			t.Errorf("tenant %s completed %d, want %d (the key decides the tenant)", ts.Tenant, ts.Completed, want)
		}
	}
	metric := func(prio string) float64 {
		_, text, _ := authCall(t, base, http.MethodGet, "/metrics", adminKey, "", "", "")
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, `tenantscaler_gateway_arrivals_total{priority="`+prio+`",tenant="C"}`) {
				return mustFloat(t, strings.Fields(line)[1])
			}
		}
		return 0
	}
	if critical, standard := metric("critical"), metric("standard"); critical != 0 || standard != 1 {
		t.Errorf("C's arrivals: critical %v standard %v; want the critical request counted as standard", critical, standard)
	}

	for _, path := range []string{"/stats", "/policy", "/metrics"} {
		for _, token := range []string{"", keyA} {
			if status, _, _ := authCall(t, base, http.MethodGet, path, token, "", "", ""); status != http.StatusUnauthorized {
				t.Errorf("GET %s with token %q: status %d, want 401", path, token, status)
			}
		}
		if status, got, _ := authCall(t, base, http.MethodGet, path, adminKey, "", "", ""); status != http.StatusOK {
			t.Errorf("GET %s with the admin key: status %d: %s", path, status, got)
		}
	}
	pol := `{"version":7,"tenants":{"A":{"weight":2}}}`
	if status, _, _ := authCall(t, base, http.MethodPost, "/policy", keyA, "", "", pol); status != http.StatusUnauthorized {
		t.Errorf("POST /policy with a tenant key: status %d, want 401", status)
	}
	if status, got, _ := authCall(t, base, http.MethodPost, "/policy", adminKey, "", "", pol); status != http.StatusOK {
		t.Errorf("POST /policy with the admin key: status %d: %s", status, got)
	}
	if status, _, _ := authCall(t, base, http.MethodGet, "/healthz", "", "", "", ""); status != http.StatusOK {
		t.Errorf("GET /healthz without a key: status %d, want 200", status)
	}
}

// The public handler serves completions (keys required) and /healthz only.
func TestPublicHandler(t *testing.T) {
	up := newFakeUpstream(t, nil)
	keys, _ := ParseTenantKeys("A=" + keyA)
	g, _ := startGateway(t, Config{Tenants: testTenants("A"), Discovery: StaticDiscovery{up.URL()}, TenantKeys: keys})
	pub := httptest.NewServer(g.PublicHandler())
	defer pub.Close()
	if status, got, _ := authCall(t, pub.URL, http.MethodPost, "/v1/completions", keyA, "", "", completionBody("w", 1, false)); status != http.StatusOK {
		t.Errorf("public completion: %d %s", status, got)
	}
	if status, _, _ := authCall(t, pub.URL, http.MethodPost, "/v1/completions", "", "", "", completionBody("w", 1, false)); status != http.StatusUnauthorized {
		t.Errorf("public completion without a key: %d, want 401", status)
	}
	for _, path := range []string{"/stats", "/policy", "/metrics"} {
		if status, _, _ := authCall(t, pub.URL, http.MethodGet, path, keyA, "", "", ""); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("public GET %s: %d, want not served", path, status)
		}
	}
	if status, _, _ := authCall(t, pub.URL, http.MethodGet, "/healthz", "", "", "", ""); status != http.StatusOK {
		t.Errorf("public /healthz: %d", status)
	}
	noKeys, _ := startGateway(t, Config{Tenants: testTenants("A"), Discovery: StaticDiscovery{up.URL()}})
	open := httptest.NewServer(noKeys.PublicHandler())
	defer open.Close()
	if status, _, _ := authCall(t, open.URL, http.MethodPost, "/v1/completions", "", "A", "", completionBody("w", 1, false)); status != http.StatusServiceUnavailable {
		t.Errorf("public completion without configured keys: %d, want 503", status)
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}
