package pool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRoundTrip(t *testing.T) {
	p := newPool(t, 30*time.Millisecond, 4)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	c := Client{BaseURL: srv.URL + "/"} // trailing slash is tolerated
	ctx := context.Background()

	if err := c.Scale(ctx, 2); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Desired != 2 || len(st.Replicas) != 2 || st.Replicas[0].ID != "p-1" || st.Replicas[0].URL == "" {
		t.Fatalf("status %+v", st)
	}
	waitFor(t, 2*time.Second, "2 ready endpoints", func() bool {
		eps, err := c.Endpoints(ctx)
		return err == nil && len(eps) == 2
	})
	eps, err := c.Endpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := p.Endpoints(); strings.Join(eps, ",") != strings.Join(want, ",") {
		t.Fatalf("client endpoints %v, pool endpoints %v", eps, want)
	}
	if err := c.Scale(ctx, 9); err != nil { // clamped to MaxReplicas
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); st.Desired != 4 {
		t.Fatalf("desired %d after Scale(9) with max 4", st.Desired)
	}
	// Negative counts clamp to 0 over HTTP exactly as Pool.Scale does.
	if err := c.Scale(ctx, -1); err != nil {
		t.Fatalf("Scale(-1): %v, want it clamped to 0", err)
	}
	st, err = c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Desired != 0 || st.Ready != 0 || st.Starting != 0 {
		t.Fatalf("after Scale(-1): %+v, want desired 0 and every replica draining", st)
	}
}

func TestClientErrors(t *testing.T) {
	p := newPool(t, 0, 1)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	ctx := context.Background()
	if _, err := (Client{BaseURL: srv.URL + "/missing"}).Status(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Status on a wrong path: %v, want a 404 error", err)
	}
	srv.Close()
	if _, err := (Client{BaseURL: srv.URL}).Endpoints(ctx); err == nil {
		t.Fatal("Endpoints on a closed server succeeded")
	}
}

func TestHandlerAPI(t *testing.T) {
	p := newPool(t, 0, 2)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	put := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/pool/scale", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	tests := []struct {
		name, body string
		code       int
		desired    float64 // checked on 200 responses
	}{
		{"invalid json", `{"replicas":`, http.StatusBadRequest, 0},
		{"missing field", `{}`, http.StatusBadRequest, 0},
		{"negative clamps to 0", `{"replicas":-2}`, http.StatusOK, 0},
		{"above max clamps to max", `{"replicas":5}`, http.StatusOK, 2},
		{"valid", `{"replicas":1}`, http.StatusOK, 1},
	}
	for _, tc := range tests {
		code, out := put(tc.body)
		if code != tc.code {
			t.Errorf("%s: status %d, want %d (%v)", tc.name, code, tc.code, out)
		}
		if msg, _ := out["error"].(string); code == http.StatusBadRequest && msg == "" {
			t.Errorf("%s: no error message", tc.name)
		}
		if code == http.StatusOK && out["desired"] != tc.desired {
			t.Errorf("%s: response %v, want desired %v", tc.name, out, tc.desired)
		}
	}

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/healthz"); code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("/healthz: %d %s", code, body)
	}
	if code, body := get("/endpoints"); code != http.StatusOK || !strings.Contains(body, `"endpoints":["http://127.0.0.1:`) {
		t.Fatalf("/endpoints: %d %s", code, body)
	}
	if code, body := get("/pool"); code != http.StatusOK || !strings.Contains(body, `"replicas":[{"id":"p-1"`) {
		t.Fatalf("/pool: %d %s", code, body)
	}
	if code, _ := get("/pool/scale"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /pool/scale: %d, want 405", code)
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := put(`{"replicas":1}`); code != http.StatusServiceUnavailable {
		t.Fatalf("scale after Close: %d %v, want 503", code, out)
	}
}

func TestEndpointsEmptyIsArray(t *testing.T) {
	p := newPool(t, 0, 1)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/endpoints", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"endpoints":[]}` {
		t.Fatalf("/endpoints on an empty pool = %s", got)
	}
}
