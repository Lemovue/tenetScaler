package controller

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"tenantscaler/internal/config"
)

// Metrics source kinds accepted by NewSource.
const (
	SourceGateway    = "gateway"    // the gateways' GET /stats
	SourcePrometheus = "prometheus" // the gateways' metrics through Prometheus
)

// Overrides are command-line settings that take precedence over the config
// file. Zero values keep the config's setting; the switches can only turn
// a feature (or an ablation) on.
type Overrides struct {
	// Cycle replaces controller.cycleSeconds.
	Cycle time.Duration
	// ColdStart is the replica load time that sets the forecast horizon;
	// zero means engine.startupDuration from the config.
	ColdStart time.Duration
	// Capacity replaces pool.capacityReplicas; minReplicas is lowered to fit.
	Capacity int
	// Pool replaces pool.name.
	Pool string

	// DisableBorrowing turns on the no-borrowing ablation.
	DisableBorrowing bool
	// DisableCoupling turns on the no-gateway-coupling ablation.
	DisableCoupling bool
	// Forecast turns on the Holt-Winters demand forecaster.
	Forecast bool
}

// Apply writes the overrides into cfg and returns the cold start for
// control.NewCore. It does not validate cfg: NewCore does, after Kubernetes
// mode has replaced the tenants.
func (o Overrides) Apply(cfg *config.Config) (time.Duration, error) {
	if o.Cycle < 0 || o.ColdStart < 0 || o.Capacity < 0 {
		return 0, fmt.Errorf("cycle (%s), cold start (%s) and capacity (%d) must be >= 0", o.Cycle, o.ColdStart, o.Capacity)
	}
	ctl := &cfg.Controller
	if o.Cycle > 0 {
		ctl.CycleSeconds = o.Cycle.Seconds()
	}
	ctl.DisableBorrowing = ctl.DisableBorrowing || o.DisableBorrowing
	ctl.DisableGatewayCoupling = ctl.DisableGatewayCoupling || o.DisableCoupling
	ctl.Forecast.Enabled = ctl.Forecast.Enabled || o.Forecast
	if o.Pool != "" {
		cfg.Pool.Name = o.Pool
	}
	if o.Capacity > 0 {
		cfg.Pool.CapacityReplicas = o.Capacity
		cfg.Pool.MinReplicas = min(cfg.Pool.MinReplicas, o.Capacity)
	}
	if o.ColdStart > 0 {
		return o.ColdStart, nil
	}
	params, err := cfg.Engine.Params()
	if err != nil {
		return 0, err
	}
	return params.StartupDuration, nil
}

// SourceOptions select and configure the metrics source.
type SourceOptions struct {
	// Kind is SourceGateway or SourcePrometheus.
	Kind          string
	Gateways      []string
	PrometheusURL string
	// Window is the statistics window. Zero means the control cycle for
	// the gateway source and max(cycle, DefaultPrometheusWindow) for
	// Prometheus, whose rate() needs several scrapes inside the window.
	Window time.Duration
	// Cycle is the control period.
	Cycle time.Duration
	// HTTP is the gateway source's client; nil means a client with a 5 s
	// timeout (BearerClient adds the gateway's admin key).
	HTTP *http.Client
}

// NewSource builds the metrics source o describes.
func NewSource(o SourceOptions) (MetricsSource, error) {
	if o.Window < 0 {
		return nil, fmt.Errorf("metrics window must be >= 0, got %s", o.Window)
	}
	switch o.Kind {
	case SourceGateway:
		if len(o.Gateways) == 0 {
			return nil, errors.New("the gateway metrics source needs at least one gateway URL")
		}
		return GatewaySource{URLs: o.Gateways, Window: cmp.Or(o.Window, o.Cycle), HTTP: o.HTTP}, nil
	case SourcePrometheus:
		if o.PrometheusURL == "" {
			return nil, errors.New("the prometheus metrics source needs a Prometheus URL")
		}
		return PrometheusSource{BaseURL: o.PrometheusURL, Window: cmp.Or(o.Window, max(o.Cycle, DefaultPrometheusWindow))}, nil
	default:
		return nil, fmt.Errorf("metrics source must be %q or %q, got %q", SourceGateway, SourcePrometheus, o.Kind)
	}
}

// StartupHandler serves the controller API while the Runner is being built
// (Kubernetes mode first waits for its TenantScalers): GET /healthz answers
// 200 {"status":"starting"} so probes pass, every other request 503. Once
// Set installs the Runner's handler, it serves everything. The zero value
// is ready to use.
type StartupHandler struct{ h atomic.Pointer[http.Handler] }

// Set installs the handler that serves all requests from now on.
func (s *StartupHandler) Set(h http.Handler) { s.h.Store(&h) }

// ServeHTTP implements http.Handler.
func (s *StartupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "starting"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the controller is starting (waiting for its tenants)"})
}
