package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
)

func TestOverridesApply(t *testing.T) {
	base := config.Default()
	params, err := base.Engine.Params()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		prepare   func(*config.Config)
		o         Overrides
		check     func(t *testing.T, c config.Config)
		coldStart time.Duration
		wantErr   string
	}{
		{name: "zero overrides keep the config", check: func(t *testing.T, c config.Config) {
			if !reflect.DeepEqual(c, base) {
				t.Errorf("config changed: %+v", c)
			}
		}, coldStart: params.StartupDuration},
		{name: "cycle, pool and cold start", o: Overrides{Cycle: 5 * time.Second, Pool: "p", ColdStart: 45 * time.Second},
			check: func(t *testing.T, c config.Config) {
				if c.Controller.CycleSeconds != 5 || c.Pool.Name != "p" {
					t.Errorf("cycle %v pool %q, want 5 and p", c.Controller.CycleSeconds, c.Pool.Name)
				}
			}, coldStart: 45 * time.Second},
		{name: "capacity lowers minReplicas to fit", prepare: func(c *config.Config) { c.Pool.MinReplicas = 6 },
			o: Overrides{Capacity: 4}, check: func(t *testing.T, c config.Config) {
				if c.Pool.CapacityReplicas != 4 || c.Pool.MinReplicas != 4 {
					t.Errorf("capacity %d min %d, want 4 and 4", c.Pool.CapacityReplicas, c.Pool.MinReplicas)
				}
			}, coldStart: params.StartupDuration},
		{name: "capacity keeps a lower minReplicas", o: Overrides{Capacity: 4}, check: func(t *testing.T, c config.Config) {
			if c.Pool.CapacityReplicas != 4 || c.Pool.MinReplicas != base.Pool.MinReplicas {
				t.Errorf("capacity %d min %d, want 4 and %d", c.Pool.CapacityReplicas, c.Pool.MinReplicas, base.Pool.MinReplicas)
			}
		}, coldStart: params.StartupDuration},
		{name: "switches turn features on", o: Overrides{DisableBorrowing: true, DisableCoupling: true, Forecast: true},
			check: func(t *testing.T, c config.Config) {
				ctl := c.Controller
				if !ctl.DisableBorrowing || !ctl.DisableGatewayCoupling || !ctl.Forecast.Enabled {
					t.Errorf("controller = %+v, want both ablations and the forecaster on", ctl)
				}
			}, coldStart: params.StartupDuration},
		{name: "unset switches keep the config's", prepare: func(c *config.Config) {
			c.Controller.DisableBorrowing, c.Controller.Forecast.Enabled = true, true
		}, check: func(t *testing.T, c config.Config) {
			if !c.Controller.DisableBorrowing || !c.Controller.Forecast.Enabled || c.Controller.DisableGatewayCoupling {
				t.Errorf("controller = %+v, want the config's switches", c.Controller)
			}
		}, coldStart: params.StartupDuration},
		{name: "a cold start override skips the engine params", prepare: func(c *config.Config) { c.Engine.StartupDuration = "bad" },
			o: Overrides{ColdStart: time.Second}, coldStart: time.Second},
		{name: "bad engine params", prepare: func(c *config.Config) { c.Engine.StartupDuration = "bad" }, wantErr: "bad"},
		{name: "negative cycle", o: Overrides{Cycle: -time.Second}, wantErr: "must be >= 0"},
		{name: "negative cold start", o: Overrides{ColdStart: -time.Second}, wantErr: "must be >= 0"},
		{name: "negative capacity", o: Overrides{Capacity: -1}, wantErr: "must be >= 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			if tt.prepare != nil {
				tt.prepare(&cfg)
			}
			cold, err := tt.o.Apply(&cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Apply = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || cold != tt.coldStart {
				t.Fatalf("Apply = %v, %v; want cold start %v", cold, err, tt.coldStart)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestNewSource(t *testing.T) {
	gws := []string{"http://g1", "http://g2"}
	tests := []struct {
		name    string
		o       SourceOptions
		want    MetricsSource
		wantErr string
	}{
		{"gateway window defaults to the cycle", SourceOptions{Kind: SourceGateway, Gateways: gws, Cycle: 15 * time.Second},
			GatewaySource{URLs: gws, Window: 15 * time.Second}, ""},
		{"gateway window", SourceOptions{Kind: SourceGateway, Gateways: gws, Cycle: 15 * time.Second, Window: 30 * time.Second},
			GatewaySource{URLs: gws, Window: 30 * time.Second}, ""},
		{"prometheus window is at least a minute", SourceOptions{Kind: SourcePrometheus, PrometheusURL: "http://p", Cycle: 15 * time.Second},
			PrometheusSource{BaseURL: "http://p", Window: time.Minute}, ""},
		{"prometheus window follows a long cycle", SourceOptions{Kind: SourcePrometheus, PrometheusURL: "http://p", Cycle: 90 * time.Second},
			PrometheusSource{BaseURL: "http://p", Window: 90 * time.Second}, ""},
		{"prometheus window", SourceOptions{Kind: SourcePrometheus, PrometheusURL: "http://p", Cycle: 15 * time.Second, Window: 20 * time.Second},
			PrometheusSource{BaseURL: "http://p", Window: 20 * time.Second}, ""},
		{"unknown kind", SourceOptions{Kind: "statsd", Gateways: gws}, nil, `got "statsd"`},
		{"gateway without URLs", SourceOptions{Kind: SourceGateway}, nil, "at least one gateway URL"},
		{"prometheus without URL", SourceOptions{Kind: SourcePrometheus}, nil, "Prometheus URL"},
		{"negative window", SourceOptions{Kind: SourceGateway, Gateways: gws, Window: -time.Second}, nil, "must be >= 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewSource(tt.o)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("NewSource = %v, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewSource = %#v, %v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestStartupHandler(t *testing.T) {
	var h StartupHandler
	srv := httptest.NewServer(&h)
	defer srv.Close()
	check := func(method, path string, wantCode int, wantBody string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != wantCode || !strings.Contains(string(body), wantBody) {
			t.Errorf("%s %s = %d %s, want %d with %q", method, path, resp.StatusCode, body, wantCode, wantBody)
		}
	}
	check(http.MethodGet, "/healthz", http.StatusOK, `"starting"`)
	check(http.MethodGet, "/status", http.StatusServiceUnavailable, "waiting for its tenants")
	check(http.MethodPost, "/healthz", http.StatusServiceUnavailable, "starting")

	rig := newRig(t, testConfig(t))
	h.Set(rig.r.Handler())
	check(http.MethodGet, "/healthz", http.StatusOK, `"ok"`)
	check(http.MethodGet, "/status", http.StatusOK, `"cycle"`)
}
